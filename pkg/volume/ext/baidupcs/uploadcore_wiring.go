// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// uploadcore_wiring.go —— 把自研上传内核接到 `Storage.putAttempt` 的**上传一步**（S5 接线）。
//
// 设计：docs/design-baidupcs-upload-core.md §5「回退」。
// 开关语义（`StorageConfig.UploadCore`，**空 = auto** —— 硬规则 7c：string 配置默认空字符串）：
//
//	legacy  : 只用既有路径（binaryAdapter 二进制优先 / libraryAdapter 库 MultiUploader）
//	nucleus : 只用内核；失败即报错（不降级，行为最可预测）
//	空/auto : **内核优先；失败一次 ⇒ 本进程降级 legacy + WARN**（灰度友好：新代码不阻塞生产上传）
//
// 接线范围刻意最小：**只替换"上传动作"这一步**，其外的 precheck/复核/秒传刷新等既有逻辑不变
// ⇒ 数据一致性红线（上传后必须按 md5 复核）完全沿用，不由内核绕过。

import (
	"context"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs/uploadcore"
)

// coreClientProvider 是"能提供库客户端与秒传刷新能力"的窄接口（**依赖倒置，不绑定具体类型**）：
// libraryAdapter 天然满足；binaryAdapter 经 Fallback 透传（见 coreDepsOf）。
type coreClientProvider interface {
	coreClient() *Client
	coreRepair() func(ctx context.Context, remote string, st *stagedUpload) (bool, error)
}

func (a *libraryAdapter) coreClient() *Client { return a.pcs }

func (a *libraryAdapter) coreRepair() func(context.Context, string, *stagedUpload) (bool, error) {
	return a.RapidUpload
}

// coreDepsOf 取内核所需依赖（库客户端 + 自愈能力）；binaryAdapter 时经 Fallback 透传。
func (s *Storage) coreDepsOf() (*Client, func(context.Context, string, *stagedUpload) (bool, error), bool) {
	if p, ok := s.adapter.(coreClientProvider); ok && p.coreClient() != nil {
		return p.coreClient(), p.coreRepair(), true
	}
	if ba, ok := s.adapter.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if p, ok := ba.cfg.Fallback.(coreClientProvider); ok && p.coreClient() != nil {
			return p.coreClient(), p.coreRepair(), true
		}
	}
	return nil, nil, false
}

// uploadStaged 执行"把暂存文件上传到远端"这一步（内核优先 + 可降级）。
func (s *Storage) uploadStaged(ctx context.Context, key, remote string, st *stagedUpload) error {
	// G2：每卷上传并发闸 —— 限制"同时进行的上传动作"数；并发叠乘（块级 × 分片 × 卷数）
	// 是百度风控（31034）的来源。等待经 waitq（ctx 可中断），释放后唤醒等待者。
	release, aErr := s.acquireUploadSlot(ctx, key)
	if aErr != nil {
		return aErr
	}
	defer release()

	mode := s.coreMode
	if mode == "" {
		mode = "auto"
	}
	pcs, repair, coreOK := s.coreDepsOf()
	// 内核需要库 client + 本地文件。尺寸不再设护栏：2026-11-08 修复「秒传刷新缺会话 uploadid」后，
	// 多块（含 120MiB/30 块）已实测收敛为权威整文件 md5；内核失败仍由 auto 降级兜底。
	useCore := coreOK && st != nil && st.tmpPath != "" && st.size >= 0
	route, rErr := s.pickUploadRoute(mode, useCore, coreOK)
	if rErr != nil {
		return rErr
	}
	if route == routeLegacy {
		return s.adapter.Upload(ctx, st.tmpPath, remote, true)
	}
	// G3：auto 降级带**冷却窗口** —— 窗口内不再试内核（避免每个文件都踩同一个坑），
	// 窗口过后**自动重新尝试**（否则一次偶发抖动会让本进程永久失去内核能力）。
	if mode == "auto" && s.coreDegradedNow(key) {
		return s.adapter.Upload(ctx, st.tmpPath, remote, true)
	}

	s.coreStatUsed.Add(1)
	if err := s.doCoreUpload(ctx, pcs, repair, key, remote, st); err != nil {
		if nErr := s.onCoreFailure(mode, key, err); nErr != nil {
			return nErr
		}
		return s.adapter.Upload(ctx, st.tmpPath, remote, true)
	}
	s.coreDisabledAt.Store(0) // 内核恢复 ⇒ 确保不带残留降级标记
	return nil
}

// uploadRoute 是上传动作的路由结论（内核 or 既有路径）。
type uploadRoute int

const (
	routeLegacy uploadRoute = iota // 走既有路径（binary 优先 / 库 MultiUploader）
	routeCore                      // 走自研上传内核
)

// pickUploadRoute 决定本次上传走内核还是既有路径（含路径计数）。
// nucleus 且无内核依赖 ⇒ 显式报错（fail-closed，不静默降级）。
func (s *Storage) pickUploadRoute(mode string, useCore, coreOK bool) (uploadRoute, error) {
	if mode == "legacy" {
		s.coreStatLegacyMode.Add(1)
	}
	if !useCore && !coreOK {
		s.coreStatNoDeps.Add(1)
	}
	if mode == "legacy" || !useCore {
		if mode == "nucleus" {
			return routeLegacy, fmt.Errorf("baidupcs: upload_core=nucleus 但无库 adapter（无法运行内核）")
		}
		return routeLegacy, nil
	}
	return routeCore, nil
}

// onCoreFailure 处理内核失败：nucleus ⇒ 返回原错误（不降级）；auto ⇒ 进入降级冷却窗口。
func (s *Storage) onCoreFailure(mode, key string, err error) error {
	s.coreStatFailed.Add(1)
	if mode == "nucleus" {
		return err
	}
	s.coreStatDegraded.Add(1)
	s.coreDisabledAt.Store(s.now().UnixNano())
	s.log.Warn("上传内核失败，本进程降级为既有路径（auto，带冷却窗口）",
		"key", key, "err", err, "cooldown", s.cooldown())
	return nil
}

// coreDegradedNow 报告 auto 模式当前是否应跳过内核：冷却窗口内 ⇒ true（走既有路径）；
// 窗口已过 ⇒ 清除降级标记 + 计数 + 记日志后返回 false（允许重新尝试内核）。
func (s *Storage) coreDegradedNow(key string) bool {
	if s.coreDisabledAt.Load() == 0 {
		return false
	}
	if s.coreDegraded(s.now()) {
		return true
	}
	s.coreDisabledAt.Store(0)
	s.coreDegradeRetry.Add(1)
	s.log.Info("上传内核降级冷却结束，重新尝试内核", "key", key,
		"retry", s.coreDegradeRetry.Load())
	return false
}

// coreUploadFunc 是内核上传动作的签名（可注入：测试用确定性替身驱动成功/失败）。
type coreUploadFunc func(ctx context.Context, pcs *Client,
	repair func(context.Context, string, *stagedUpload) (bool, error),
	key, remote string, st *stagedUpload) error

// doCoreUpload 执行内核上传（未注入替身时走真实实现）。
func (s *Storage) doCoreUpload(ctx context.Context, pcs *Client,
	repair func(context.Context, string, *stagedUpload) (bool, error),
	key, remote string, st *stagedUpload) error {
	if s.coreUpload != nil {
		return s.coreUpload(ctx, pcs, repair, key, remote, st)
	}
	return s.uploadViaCore(ctx, pcs, repair, key, remote, st)
}

// defaultCoreDegradeCooldown 是 auto 降级后的冷却窗口（过后自动重试内核）。
const defaultCoreDegradeCooldown = 5 * time.Minute

// now 返回当前时间（测试可注入假时钟）。
func (s *Storage) now() time.Time {
	if s.coreNow != nil {
		return s.coreNow()
	}
	return time.Now()
}

// cooldown 返回归一化后的冷却窗口。
func (s *Storage) cooldown() time.Duration {
	if s.coreCooldown > 0 {
		return s.coreCooldown
	}
	return defaultCoreDegradeCooldown
}

// coreDegraded 判定"在给定时刻是否处于降级冷却窗口内"（纯函数，便于测试推进时钟）。
func (s *Storage) coreDegraded(now time.Time) bool {
	at := s.coreDisabledAt.Load()
	if at == 0 {
		return false
	}
	return now.Sub(time.Unix(0, at)) < s.cooldown()
}

// uploadViaCore 用内核执行一次上传编排（状态机 probe→rapid→slices→merge→verify + 自愈）。
func (s *Storage) uploadViaCore(ctx context.Context, pcs *Client,
	repair func(context.Context, string, *stagedUpload) (bool, error),
	key, remote string, st *stagedUpload) error {
	wal, resume, err := s.uploadCoreWAL(ctx, key)
	if err != nil {
		return err
	}
	ad := newUCAdapter(s, newLibPCS(pcs), key, st.tmpPath, remote, st, true, repair)
	u, err := uploadcore.New(uploadcore.Options{
		Probe: ad, Rapid: ad, Slicer: ad, Parts: ad, Merge: ad, Verify: ad, Repair: ad,
		WAL:    wal,
		Resume: resume,
		Logger: s.log,
	})
	if err != nil {
		return fmt.Errorf("baidupcs: 初始化上传内核: %w", err)
	}
	res, err := u.Upload(ctx, uploadcore.Request{
		LocalPath: st.tmpPath, TargetPath: remote, Size: st.size, MD5: st.md5, Overwrite: true,
	})
	if err != nil {
		return fmt.Errorf("baidupcs: 内核上传失败: %w", err)
	}
	s.log.Debug("内核上传完成", "key", key, "via", res.Via,
		"parts", res.PartsTotal, "upload_id", res.UploadID)
	return nil
}

// uploadCoreWAL 返回（WAL 记录器, 恢复态）：装配了 audit 则用作用域 WAL（可崩溃续传），
// 否则用空 WAL（流程一致，但无跨进程续传）。
func (s *Storage) uploadCoreWAL(ctx context.Context, key string) (uploadcore.Recorder, *uploadcore.ScopeState, error) {
	svc := s.auditSvc
	if svc == nil {
		return uploadcore.NopRecorder(), nil, nil
	}
	volume := s.root
	rec, err := uploadcore.OpenAuditRecorder(ctx, svc, volume, key)
	if err != nil {
		return nil, nil, err
	}
	resume, err := uploadcore.ResumeScope(ctx, svc, volume, key)
	if err != nil {
		return nil, nil, err
	}
	return rec, resume, nil
}
