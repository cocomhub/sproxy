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

// uploadStaged 执行"把暂存文件上传到远端"这一步（内核优先 + 可降级）。
func (s *Storage) uploadStaged(ctx context.Context, key, remote string, st *stagedUpload) error {
	mode := s.coreMode
	if mode == "" {
		mode = "auto"
	}
	pcs, repair, coreOK := s.coreDepsOf()
	// 内核需要库 client + 本地文件；不满足（如 binary-only 无库兜底）⇒ 直接走既有路径。
	useCore := coreOK && st != nil && st.tmpPath != "" && st.size >= 0
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
