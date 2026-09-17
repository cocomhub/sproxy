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
	"sync"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
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

// setupBaidupcsFSFactory 装配 baidupcs 载体工厂（V3 接入 T2：配置已并入 volumes[]）。
//
// 前置（fail-closed，仿 setupMeshFSFactory）：
//   - 从 cfg.Volumes 过滤 type=baidupcs 的卷（系统盘），逐卷构造 VolumeBackend；
//   - 单盘 Storage 构造失败 → 该盘跳过 + 告警，**其余盘继续装配**（多盘容忍单盘坏）；
//   - 全部盘都失败/无 baidupcs 卷 → 不注入工厂（kind=baidupcs 远端将以
//     syncexec.ErrBaidupcsNotWired 明确失败，**绝不回落 direct**）；
//   - 工厂按 remote.Volume 查 volumes map（卷名 = volumes[].name）。
func setupBaidupcsFSFactory(exec *syncexec.Executor, cfg *server.Config, log *slog.Logger, factory baidupcsStorageFactory) {
	if exec == nil || cfg == nil {
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
	volumes := make(map[string]*baidupcs.VolumeBackend)
	for i := range cfg.Volumes {
		vc := cfg.Volumes[i]
		if vc.Type != "baidupcs" {
			continue
		}
		bduss, _ := vc.Extra["bduss"].(string)
		baiduRoot, _ := vc.Extra["baidu_root"].(string)
		binaryPath, _ := vc.Extra["binary_path"].(string)
		localRoot := vc.Root
		storage, err := factory(baidupcs.StorageConfig{
			Root:       baiduRoot,
			TempDir:    localRoot,
			BDUSS:      bduss,
			BinaryPath: binaryPath,
		})
		if err != nil {
			log.Warn("baidupcs 盘构造失败（该盘卷不可用，其余盘继续装配）", "disk", vc.Name, "error", err)
			continue
		}
		vb, err := baidupcs.NewVolumeBackend(context.Background(), baidupcs.VolumeBackendConfig{
			Name:      vc.Name,
			Storage:   storage,
			LocalRoot: localRoot,
		})
		if err != nil {
			log.Warn("baidupcs 盘卷构造失败（该盘卷不可用，其余盘继续装配）", "disk", vc.Name, "error", err)
			continue
		}
		// quota 融合（卷级 Scope）：staging 写入预留，上传完成释放。
		if fs, ok := vb.FS.(*baidupcs.StorageFS); ok {
			fs.WithQuota(&scopeQuotaTracker{scope: quota.NewPool(0).Scope("", 0)})
		}
		volumes[vb.Name] = vb
	}
	if len(volumes) == 0 {
		log.Warn("baidupcs 无可用盘（kind=baidupcs 的远端将 fail-closed）")
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

// ---- V3 接入（T1）：baidupcs backend 插件（RegisterBackend 可插拔）----

// baidupcsExternalBackend 是 baidupcs 卷的 ExternalBackend 实现：持有 StorageFS 同步视图，
// Close 无连接资源（幂等安全）。
type baidupcsExternalBackend struct {
	fs syncpkg.FS
}

func (b *baidupcsExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *baidupcsExternalBackend) Close() error { return nil }

// newBaidupcsBackend 按卷描述构造 baidupcs 外部后端（V3 可插拔）。
// 用默认 Storage 工厂（baidupcs.DefaultFactory().New）。
func newBaidupcsBackend(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
	return newBaidupcsBackendWithFactory(ctx, v, nil)
}

// newBaidupcsBackendWithFactory 同 newBaidupcsBackend，但 Storage 工厂可注入（测试用 fake）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "bduss"：百度网盘登录凭据（库兜底需要）；
//   - "baidu_root"：网盘根路径（如 /disk1；空 = "/"）；
//   - "binary_path"：BaiduPCS-Go 可执行路径（空 = PATH 查找）。
//
// v.RootDir = 本地中间态基目录（staging/resume/cache/tmp 落它之下，用户硬规则）。
//
// 凭据（fail-closed）：bduss 与 binary_path 至少一个非空——否则二进制优先（PATH 查找）
// 与库兜底（bduss）都没有可用执行路径，明确报错而非静默跳过。
func newBaidupcsBackendWithFactory(ctx context.Context, v volume.Volume, factory baidupcsStorageFactory) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q 类型 %q 不是外部 baidupcs 卷", v.Name, v.Type)
	}
	bduss, _ := v.Extra["bduss"].(string)
	baiduRoot, _ := v.Extra["baidu_root"].(string)
	binaryPath, _ := v.Extra["binary_path"].(string)
	if bduss == "" && binaryPath == "" {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q 需配置 bduss 或 binary_path 至少一个（fail-closed：否则二进制优先与库兜底都无可用执行路径）", v.Name)
	}
	if factory == nil {
		factory = func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error) {
			return baidupcs.DefaultFactory().New(cfg)
		}
	}
	storage, err := factory(baidupcs.StorageConfig{
		Root:       baiduRoot,
		TempDir:    v.RootDir,
		BDUSS:      bduss,
		BinaryPath: binaryPath,
	})
	if err != nil {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q Storage 构造失败: %w", v.Name, err)
	}
	vb, err := baidupcs.NewVolumeBackend(ctx, baidupcs.VolumeBackendConfig{
		Name:      v.Name,
		Storage:   storage,
		LocalRoot: v.RootDir,
	})
	if err != nil {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q VolumeBackend 构造失败: %w", v.Name, err)
	}
	// quota 融合（卷级 Scope）：staging 写入预留，上传完成释放（延续 T4）。
	if fs, ok := vb.FS.(*baidupcs.StorageFS); ok {
		fs.WithQuota(&scopeQuotaTracker{scope: quota.NewPool(0).Scope("", 0)})
	}
	return &baidupcsExternalBackend{fs: vb.FS}, nil
}

// registerBaidupcsBackendWithFactory 注册 baidupcs 后端类型构造器（测试可注入 fake 工厂，
// 用独立类型名避免与生产注册冲突）。重复注册 → registry panic（编程错误）。
func registerBaidupcsBackendWithFactory(typ string, factory baidupcsStorageFactory) {
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return newBaidupcsBackendWithFactory(ctx, v, factory)
	})
}

// registerBaidupcsBackend 注册 baidupcs 后端（生产默认工厂）。装配层（root.go）调用。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic
// （registry.RegisterBackend 重复注册即 panic，T1 设计）。
var registerBaidupcsOnce sync.Once

func registerBaidupcsBackend() {
	registerBaidupcsOnce.Do(func() {
		registerBaidupcsBackendWithFactory("baidupcs", nil)
	})
}
