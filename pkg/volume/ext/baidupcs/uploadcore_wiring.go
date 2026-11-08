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

// coreMaxVerifiedSize 是内核**能用 ETag 校验**的文件大小上限（单片：≤ uploadBlockSize = 4MiB）。
//
// 依据（2026-11-08 真实网盘 e2e，**随机内容**以排除秒传；命令见 `real_network_compare_e2e_test.go` 的 E2E_* 旋钮）：
//
//	1MiB / 3MiB（单片，无 merge）   ⇒ ETag == 本地 md5 ✓
//	8 / 20 / 48 / 96 / 120MiB（多分片）⇒ **回读内容逐字节一致 ✓**（内核对，无损坏），
//	                                  但百度给的 ETag **既非整文件 md5、也非块列表 md5** ⇒ 无法用 ETag 判权威
//
// ⇒ 超过该值时**不走内核**（交既有路径），原因是**校验语义**而非内容缺陷：内核的 `Verify`
// 以「ETag == 整文件 md5」判据，对多块文件会误报不一致（fail-closed 白报错，且秒传自愈也改不了
// ETag 语义）。要让内核覆盖多分片，需把校验改为**回读内容比对**（带宽成本需评估）。
// 详见 `docs/external-dependencies.md` §7.8.3（含实测三值与复现命令）。
const coreMaxVerifiedSize = uploadBlockSize

// coreAllowed 判定本次上传是否允许走内核：内核依赖齐备 + 暂存文件可用 + 尺寸在已验证范围内。
// 超限时按模式处理（auto 降级并 WARN；nucleus 显式报错）——抽出以确保 uploadStaged 认知复杂度达标。
func (s *Storage) coreAllowed(mode, key string, st *stagedUpload, coreOK bool) (bool, error) {
	useCore := coreOK && st != nil && st.tmpPath != "" && st.size >= 0
	var limit int64 = coreMaxVerifiedSize
	if s.coreMaxSizeOverride > 0 { // 仅测试/真实验收放开（见 StorageConfig.coreMaxVerifiedSizeOverride）
		limit = s.coreMaxSizeOverride
	}
	if !useCore || st.size <= limit {
		return useCore, nil
	}
	if mode == "nucleus" {
		return false, fmt.Errorf("baidupcs: upload_core=nucleus 不支持 %d 字节（超过内核尺寸护栏 %d）",
			st.size, limit)
	}
	s.log.Warn("文件超过上传内核尺寸护栏，改走既有路径", "key", key, "size", st.size, "limit", limit)
	return false, nil
}

// uploadStaged 执行"把暂存文件上传到远端"这一步（内核优先 + 可降级）。
func (s *Storage) uploadStaged(ctx context.Context, key, remote string, st *stagedUpload) error {
	mode := s.coreMode
	if mode == "" {
		mode = "auto"
	}
	pcs, repair, coreOK := s.coreDepsOf()
	useCore, gerr := s.coreAllowed(mode, key, st, coreOK)
	if gerr != nil {
		return gerr
	}
	if mode == "legacy" || !useCore {
		if mode == "nucleus" && !useCore {
			return fmt.Errorf("baidupcs: upload_core=nucleus 但无库 adapter（无法运行内核）")
		}
		return s.adapter.Upload(ctx, st.tmpPath, remote, true)
	}
	// 本进程已降级（一次失败后不再重试内核，避免每个文件都踩同一个坑）。
	if mode == "auto" && s.coreDisabled.Load() {
		return s.adapter.Upload(ctx, st.tmpPath, remote, true)
	}

	if err := s.uploadViaCore(ctx, pcs, repair, key, remote, st); err == nil {
		return nil
	} else if mode == "nucleus" {
		return err
	} else {
		s.coreDisabled.Store(true)
		s.log.Warn("上传内核失败，本进程降级为既有路径（auto）",
			"key", key, "err", err)
	}
	return s.adapter.Upload(ctx, st.tmpPath, remote, true)
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
