// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// baidupcs_sync.go 是百度网盘存储后端（P4）的装配：把 `syncexec` 的 `BaidupcsFSFactory`
// 接到 `pkg/baidupcs` 的 VolumeBackend，使 `sync_remotes[].kind=baidupcs` 的同步任务
// 真正可执行（本地↔网盘双向同步）。
//
// 与 mesh 载体同构（见 mesh_sync.go）：`cmd/sproxy` 是唯一装配层，领域包（pkg/baidupcs、
// pkg/syncexec）互不依赖——syncexec 只消费 `sync.FS` 抽象 + 工厂签名，baidupcs 只提供
// 卷构造器，卷名→StorageFS 的映射关系在装配层维护。
//
// quota 融合（Ruling-5）：staging 配额用**卷级** quota.Scope（NewPool + Scope，装配层建）——
// 与 mesh 工厂同构（工厂只拿 remote 配置，无任务 owner 上下文，无法按 owner 分桶）。
// 适配器语义：ReserveUsage = TryReserve（占 reserved）→ ReleaseUsage = Release（归还），
// 不 Commit（staging 是暂态非最终文件，精确对齐 syncfs.go WriteFile 的「预留→上传完成释放」）。

import (
	"context"
	"fmt"
	"log/slog"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
)

// baidupcsStorageFactory 构造网盘 Storage（可注入，测试用内存 fake）。
// nil（生产）时用 baidupcs.DefaultFactory().New。
type baidupcsStorageFactory func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error)

// scopeQuotaTracker 是 QuotaTracker 的卷级 quota.Scope 适配器（Ruling-5 修正）：
//
//	ReserveUsage(size) → scope.TryReserve(size) + res.Commit(size)  // 入账 committed（计数器语义）
//	ReleaseUsage(size) → scope.ReleaseUsage(size)                    // 从 committed 扣减
//
// 与 P2 Quota（used += size / -= size）等价、对齐 syncfs.go WriteFile 的「预留→上传完成
// 释放」；无单 Reservation 状态 → 并发 WriteFile（多任务共享同一 StorageFS）安全
// （Scope 内部 mutex 串行化账本操作）。
type scopeQuotaTracker struct {
	scope *quota.Scope
}

func (q *scopeQuotaTracker) ReserveUsage(size int64) error {
	if size <= 0 {
		return nil
	}
	res, err := q.scope.TryReserve(size)
	if err != nil {
		return err
	}
	res.Commit(size)
	return nil
}

func (q *scopeQuotaTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.scope.ReleaseUsage(size)
}

// setupBaidupcsFSFactory 装配 baidupcs 载体工厂（多盘，T7）。
//
// 前置（fail-closed，仿 setupMeshFSFactory）：
//   - cfg.Baidupcs.Enabled 才装配；
//   - 逐盘构造（for _, d := range cfg.Baidupcs.Disks）：单盘 Storage 构造失败 → 该盘
//     跳过 + 告警，**其余盘继续装配**（多盘容忍单盘坏）；
//   - 全部盘都失败 → 不注入工厂（kind=baidupcs 远端将以 syncexec.ErrBaidupcsNotWired
//     明确失败，**绝不回落 direct**）；
//   - 工厂按 remote.Volume 查 volumes map（卷名 = 盘名）。
func setupBaidupcsFSFactory(exec *syncexec.Executor, cfg *server.Config, log *slog.Logger, factory baidupcsStorageFactory) {
	if exec == nil || cfg == nil || !cfg.Baidupcs.Enabled {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	if factory == nil {
		factory = func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
			return baidupcs.DefaultFactory().New(cfg)
		}
	}
	volumes := make(map[string]*baidupcs.VolumeBackend, len(cfg.Baidupcs.Disks))
	for i := range cfg.Baidupcs.Disks {
		d := cfg.Baidupcs.Disks[i]
		storage, err := factory(baidupcs.StorageConfig{
			Root:       d.Root,
			TempDir:    d.LocalRoot,
			BDUSS:      d.BDUSS,
			BinaryPath: d.BinaryPath,
		})
		if err != nil {
			log.Warn("baidupcs 盘构造失败（该盘卷不可用，其余盘继续装配）", "disk", d.Name, "error", err)
			continue
		}
		vb, err := baidupcs.NewVolumeBackend(context.Background(), baidupcs.VolumeBackendConfig{
			Name:      d.Name,
			Storage:   storage,
			LocalRoot: d.LocalRoot,
		})
		if err != nil {
			log.Warn("baidupcs 盘卷构造失败（该盘卷不可用，其余盘继续装配）", "disk", d.Name, "error", err)
			return
		}
		// quota 融合（卷级 Scope）：staging 写入预留，上传完成释放。
		if fs, ok := vb.FS.(*baidupcs.StorageFS); ok {
			fs.WithQuota(&scopeQuotaTracker{scope: quota.NewPool(0).Scope("", 0)})
		}
		volumes[vb.Name] = vb
	}
	if len(volumes) == 0 {
		log.Warn("baidupcs 全部盘构造失败（kind=baidupcs 的远端将 fail-closed）")
		return
	}
	exec.SetBaidupcsFSFactory(func(ctx context.Context, remote syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		v, ok := volumes[remote.Volume]
		if !ok {
			return nil, nil, fmt.Errorf("remote %q: baidupcs 卷 %q 未装配（baidupcs.enabled 且 disks[].name 匹配）", remote.Name, remote.Volume)
		}
		return v.FS, func() {}, nil
	})
	log.Info("baidupcs 载体已装配", "disks", len(volumes), "volumes", mapKeys(volumes))
}

// mapKeys 返回 map 的键切片（日志用；无序）。
func mapKeys(m map[string]*baidupcs.VolumeBackend) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
