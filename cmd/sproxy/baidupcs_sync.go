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
// quota 融合（P5 per-owner）：staging 配额按**任务 owner** 分桶（owner_quotas 生效）——
// 工厂签名带 owner（syncexec.BaidupcsFSFactory 加 owner 参数），装配层经 scopeFor(owner)
// 解析该 owner 的 user 桶 quota.Scope，WithQuota 挂到 StorageFS（owner 分桶防单用户占满
// 本地磁盘）。scopeFor 为 nil 或返回 nil Scope 时**不装配 quota**（兼容无配额装配）。
// 适配器语义（Ruling-5 修正）：ReserveUsage = TryReserve + Commit（计数器）→
// ReleaseUsage = ReleaseUsage（从 committed 扣减）。

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	baidupcs "github.com/cocomhub/sproxy/pkg/baidupcs"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// baidupcsStorageFactory 构造网盘 Storage（可注入，测试用内存 fake）。
// nil（生产）时用 baidupcs.DefaultFactory().New。
type baidupcsStorageFactory func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error)

// ownerQuotaTracker 是 QuotaTracker 的 per-owner quota.Scope 适配器（P5，Ruling-5 修正）：
//
//	ReserveUsage(size) → scope.TryReserve(size) + res.Commit(size)  // 入账 committed（计数器语义）
//	ReleaseUsage(size) → scope.ReleaseUsage(size)                    // 从 committed 扣减
//
// 对齐 syncfs.go WriteFile 的「预留→上传完成释放」；无单 Reservation 状态 → 并发 WriteFile
// （多任务共享同一 StorageFS）安全（Scope 内部 mutex 串行化账本操作）。
// scope 由装配层按任务 owner 解析（owner_quotas 的 user 桶）——每任务构造（工厂调用时
// WithQuota 注入），owner 绑定在 quota.Scope 上。
type ownerQuotaTracker struct {
	scope *quota.Scope
}

func (q *ownerQuotaTracker) ReserveUsage(size int64) error {
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

func (q *ownerQuotaTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.scope.ReleaseUsage(size)
}

// setupBaidupcsFSFactory 装配 baidupcs 载体工厂（V3 接入 T3：工厂查 registry external；
// P5：staging quota 按任务 owner 分桶）。
//
// 语义（fail-closed，仿 setupMeshFSFactory）：
//   - set 是装配后卷集合（assembleVolumes 产物）：type=baidupcs 的卷由 V3 装配层经
//     registry.NewBackend 构造并持有在 Set.external（T1 backend 插件 + T2 volumes[] 配置）；
//   - 工厂按 remote.Volume 查 Set.External（统一寻址，**不再自建 map**）；
//   - scopeFor 按任务 owner 解析配额 Scope（owner_quotas 的 user 桶）：非 nil 时对
//     StorageFS WithQuota(ownerQuotaTracker{scope})——per-owner staging 记账；nil（装配层
//     无 quota）或返回 nil Scope（该 owner 无配额）→ 不装配（兼容，配额由外部兜底）；
//   - 工厂总是注入：Set.External 查不到卷时在**调用点**报错（fail-closed，绝不回落
//     direct——回落会让「已声明本机卷」的配置静默走远程 HTTP，破坏卷寻址语义）。
//   - set 为 nil（未装配卷集合）→ 不注入（防御；正常装配下恒非 nil）。
func setupBaidupcsFSFactory(exec *syncexec.Executor, set *registry.Set, log *slog.Logger, scopeFor func(owner string) *quota.Scope) {
	if exec == nil || set == nil {
		return
	}
	if log == nil {
		log = slog.Default()
	}
	exec.SetBaidupcsFSFactory(func(ctx context.Context, remote syncmgr.RemoteConfig, owner string) (syncpkg.FS, func(), error) {
		be := set.External(remote.Volume)
		if be == nil {
			return nil, nil, fmt.Errorf("remote %q: baidupcs 卷 %q 未装配（volumes[] 需含 type=baidupcs 且 name 匹配）", remote.Name, remote.Volume)
		}
		fs := be.FS()
		// P5：staging quota 按任务 owner 分桶（StorageFS.WithQuota；ownerScope nil → 不装配，
		// 兼容无配额装配）。注意：同一卷的 StorageFS 是 backend 持有单例——per-owner WithQuota
		// 覆盖是单值限制（同一卷多 owner 并发任务为低频场景，写入后立即上传释放，预留窗口短；
		// 记录为已知限制，per-owner 多任务并发隔离留后续装饰器方案）。
		if sfs, ok := fs.(*baidupcs.StorageFS); ok && scopeFor != nil {
			if ownerScope := scopeFor(owner); ownerScope != nil {
				sfs.WithQuota(&ownerQuotaTracker{scope: ownerScope})
			}
		}
		return fs, func() {}, nil
	})
	log.Info("baidupcs 载体已装配（工厂查 registry.Set.External；quota per-owner）")
}

// ---- V3 接入（T1）：baidupcs backend 插件（RegisterBackend 可插拔）----

// baidupcsExternalBackend 是 baidupcs 卷的 ExternalBackend 实现：持有 StorageFS 同步视图，
// Close 无连接资源（幂等安全）。
type baidupcsExternalBackend struct {
	fs syncpkg.FS
}

func (b *baidupcsExternalBackend) FS() syncpkg.FS { return b.fs }

func (b *baidupcsExternalBackend) Close() error { return nil }

// newBaidupcsBackendWithFactory 按卷描述构造 baidupcs 外部后端（V3 可插拔）；
// Storage 工厂可注入（测试用 fake，nil = 默认 baidupcs.DefaultFactory().New）。
//
// 从 v.Extra 读类型特有配置（map[string]any，值须为 string）：
//   - "bduss"：百度网盘登录凭据（库兜底需要）；
//   - "baidu_root"：网盘根路径（如 /disk1；空 = "/"）；
//   - "binary_path"：BaiduPCS-Go 可执行路径（空 = PATH 查找）；
//   - "local_root"：本地中间态基目录（staging/resume/cache/tmp 落它之下，用户硬规则；
//     空时回落 v.RootDir——V3 框架外部卷 RootDir 恒空，故正常走 extra.local_root）。
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
	localRoot, _ := v.Extra["local_root"].(string)
	if localRoot == "" {
		localRoot = v.RootDir
	}
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
		TempDir:    localRoot,
		BDUSS:      bduss,
		BinaryPath: binaryPath,
	})
	if err != nil {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q Storage 构造失败: %w", v.Name, err)
	}
	vb, err := baidupcs.NewVolumeBackend(ctx, baidupcs.VolumeBackendConfig{
		Name:      v.Name,
		Storage:   storage,
		LocalRoot: localRoot, // 已解析的 local_root（优先）或 RootDir 兜底——NewVolumeBackend 内部 MkdirAll
	})
	if err != nil {
		return nil, fmt.Errorf("baidupcs backend: 卷 %q VolumeBackend 构造失败: %w", v.Name, err)
	}
	// P5：quota 装配上移到工厂闭包（per-owner，见 setupBaidupcsFSFactory）——backend 构造
	// 不再挂卷级 quota（避免双重记账/跨 owner 串桶）。
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
