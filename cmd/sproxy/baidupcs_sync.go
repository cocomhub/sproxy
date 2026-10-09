// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// baidupcs_sync.go 是百度网盘存储后端（P4）的装配：把 `syncexec` 的 `BaidupcsFSFactory`
// 接到 `pkg/volume/ext/baidupcs` 的 VolumeBackend，使 `sync_remotes[].kind=baidupcs` 的同步任务
// 真正可执行（本地↔网盘双向同步）。
//
// 与 mesh 载体同构（见 mesh_sync.go）：`cmd/sproxy` 是唯一装配层，领域包（pkg/volume/ext/baidupcs、
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
	"io"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncexec"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume"
	baidupcs "github.com/cocomhub/sproxy/pkg/volume/ext/baidupcs"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// baidupcsStorageFactory 构造网盘 Storage（可注入，测试用内存 fake）。
// nil（生产）时用 baidupcs.DefaultFactory().New。
type baidupcsStorageFactory func(cfg baidupcs.StorageConfig) (baidupcs.StorageAPI, error)

// ownerQuotaTracker 是 StagingQuotaTracker 的 per-owner quota.Scope 适配器（P5，Ruling-5
// 修正 + 排队等待）：本地磁盘不足时**排队等待**（quota.Scope 无 wait API——用 sync.Cond
// 在 ReleaseUsage 广播唤醒，ctx.Done 中断），而不是立即拒绝（staging 写满应等待释放而非
// 任务失败；防本地磁盘被 staging/cache 占满仍由配额上限兜底）。
// **有界等待（bounded wait）**：排队不无限挂起——ctx 取消 / 等待超时（stagingWaitTimeout）
// 均中断返回错误（永久不足如「单文件超 owner 配额」快速失败为单文件 ActionError，任务
// 不卡 syncing 永不完成）。
//
//	ReserveUsage(ctx, size) → scope.TryReserve(size) + res.Commit(size) // 入账 committed
//	                         不足 → 等待（cond；Release 广播 / ctx.Done / 超时中断）
//	ReleaseUsage(size)     → scope.ReleaseUsage(size) + cond.Broadcast() // 唤醒等待者
//
// 对齐 syncfs.go WriteFile 的「预留→上传完成释放」；无单 Reservation 状态 → 并发 WriteFile
// （多任务共享同一 StorageFS）安全（Scope 内部 mutex 串行化账本操作 + cond 持锁等待）。
// scope 由装配层按任务 owner 解析（owner_quotas 的 user 桶）——每任务构造（工厂调用时
// WithQuota 注入），owner 绑定在 quota.Scope 上。
type ownerQuotaTracker struct {
	scope *quota.Scope
	cond  *sync.Cond // 排队等待：Release 广播唤醒
}

// stagingWaitTimeout 是 staging 配额排队等待超时（本地磁盘不足等待释放的最长时限）。
// **瞬时磁盘紧张**（并发任务占满、释放后继续）在此时限内重试成功；**永久不足**
// （单文件超 owner 配额、无并发释放）超过此时限快速失败为单文件 ActionError——排队
// 不无限挂起，任务不卡 syncing。5s 足够瞬时释放窗口，且远小于任务/测试等待窗口。
const stagingWaitTimeout = 5 * time.Second

func newOwnerQuotaTracker(scope *quota.Scope) *ownerQuotaTracker {
	return &ownerQuotaTracker{scope: scope, cond: sync.NewCond(&sync.Mutex{})}
}

func (q *ownerQuotaTracker) ReserveUsage(ctx context.Context, size int64) error {
	if size <= 0 {
		return nil
	}
	timer := time.NewTimer(stagingWaitTimeout)
	defer timer.Stop()
	for {
		res, err := q.scope.TryReserve(size)
		if err == nil {
			res.Commit(size)
			return nil
		}
		// 磁盘不足：排队等待释放（ctx 取消 / 超时中断；cond 无 ctx 原语——用 goroutine
		// 感知 ctx/timer 并 Broadcast 打断 cond.Wait）。
		q.cond.L.Lock()
		waiter := make(chan struct{})
		interrupt := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				q.cond.Broadcast() // 打断所有等待者重新评估（含本 ctx 取消者）
			case <-timer.C:
				q.cond.Broadcast() // 等待超时：打断重新评估（本循环感知后返回错误）
			case <-interrupt:
			}
		}()
		q.cond.Wait()
		close(interrupt)
		close(waiter)
		q.cond.L.Unlock()
		if ctx.Err() != nil {
			return fmt.Errorf("quota: 等待本地 staging 空间中断: %w", ctx.Err())
		}
		if !timer.Stop() {
			return fmt.Errorf("quota: 等待本地 staging 空间超时（%s）", stagingWaitTimeout)
		}
	}
}

func (q *ownerQuotaTracker) ReleaseUsage(size int64) {
	if size <= 0 {
		return
	}
	q.scope.ReleaseUsage(size)
	q.cond.Broadcast() // 唤醒排队等待者
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
		// P5：staging quota 按任务 owner 分桶（StagingQuotaCapable 能力接口——从
		// *StorageFS 类型断言解耦为通用接口，Wrap 装饰后的 fs 也实现委托 inner，避免
		// 包装后 quota 丢失；ownerScope nil → 不装配，兼容无配额装配）。注意：同一卷
		// 的 StorageFS 是 backend 持有单例——per-owner WithQuota 覆盖是单值限制（同一
		// 卷多 owner 并发任务为低频场景，写入后立即上传释放，预留窗口短；记录为已知
		// 限制，per-owner 多任务并发隔离留后续装饰器方案）。
		if qc, ok := fs.(syncpkg.StagingQuotaCapable); ok && scopeFor != nil {
			if ownerScope := scopeFor(owner); ownerScope != nil {
				qc.WithStagingQuota(newOwnerQuotaTracker(ownerScope))
			}
		}
		return fs, func() { /* 无清理 */ }, nil
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

// OpenURL 实现 registry.URLResolver（M7：普通卷补 OpenURL，转存产物可 ResolveURL 取用）。
// URL 形如 <scheme>://<volume>/<relPath>；取 path 段后 FS.OpenRead。fail-closed。
func (b *baidupcsExternalBackend) OpenURL(ctx context.Context, urlStr string) (io.ReadCloser, error) {
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("baidupcs OpenURL: 解析 %q 失败: %w", urlStr, err)
	}
	rel := strings.TrimPrefix(u.Path, "/")
	if rel == "" {
		return nil, fmt.Errorf("baidupcs OpenURL: %q 无路径（空 rel）", urlStr)
	}
	rc, err := b.fs.OpenRead(ctx, rel)
	if err != nil {
		return nil, fmt.Errorf("baidupcs OpenURL: 读取 %q: %w", rel, err)
	}
	return rc, nil
}

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
// protocols 变参声明协议（M7：OpenURL/ResolveURL 按 scheme 寻址——生产 "baidupcs" 声明
// "baidupcs" 协议；测试注入独立类型名不传协议，避免多个测试类型同时声明同协议冲突）。

func registerBaidupcsBackendWithFactory(typ string, factory baidupcsStorageFactory, protocols ...string) {
	registry.RegisterBackend(typ, func(ctx context.Context, v volume.Volume) (registry.ExternalBackend, error) {
		return newBaidupcsBackendWithFactory(ctx, v, factory)
	}, protocols...)
}

// registerBaidupcsBackend 注册 baidupcs 后端（生产默认工厂）。装配层（root.go）调用。
// 用 sync.Once 保证只注册一次：多装配/多测试并发调 runServer 时避免重复注册 panic
// （registry.RegisterBackend 重复注册即 panic，T1 设计）。
var registerBaidupcsOnce sync.Once

func registerBaidupcsBackend() {
	registerBaidupcsOnce.Do(func() {
		// 生产类型声明协议 "baidupcs"（M7：ResolveURL/transferURL 按 scheme 寻址）。
		registerBaidupcsBackendWithFactory("baidupcs", nil, "baidupcs")
	})
}
