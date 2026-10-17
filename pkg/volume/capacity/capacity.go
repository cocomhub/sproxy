// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package capacity 提供外部卷**卷级容量记账与强制**（C2 外部卷容量纳管）。
//
// 用户定案（2026-09-18 / 2026-10-10）：外部卷（配置卷 volumes[] 与用户卷
// /api/volumes/user，含 baidupcs/webdav/s3/secretdata）的本系统可用限额用**卷级计数**
// 强制——写入累计（超限 fail-closed 拒绝）、删除/覆盖/改名释放；与本地卷（owner_quotas
// 物理资源）不同，外部卷不占本机磁盘，限额是「本系统授权占用外部卷的额度」。
//
// **记账位于 FS 层**（`CapacityFS` 包在 backend FS 之外）：凡经 `be.FS()` 的写路径
// （HTTP 上传、云转存、同步 push、备份、服务端 Copy/Move）都被同一卷级计数器拦截，
// 从而保证「所有用户在该卷的占用之和 ≤ 卷限额」。
//
// **唯一实现**（用户裁定 2026-10-17：避免多套同时存在导致复杂性）：卷级记账由
// **分片日志账本**（`ledger.go` 的 `LedgerCounter`）承担 —— 此前两套「单写者假设」的
// 持久化计数器（`VolumeCapacityCounter`、`PoolCounter`）已**删除** ✗：多进程共享同一卷时
// 二者都会丢失更新 ⇒ 可同时通过 ⇒ 总和超限。
//
// 组件：
//   - Counter：卷级计数抽象（TryAdd/Release/Used/Capacity）。
//   - LedgerCounter：分片日志账本适配（**唯一实现**，跨进程正确）。
//   - CapacityFS：sync.FS 装饰器（覆盖写差分 + 透传全部可选能力 + 变更后持久化）。
//   - Backend：把 ExternalBackend 的 FS 包为 CapacityFS 的 backend 包装。
package capacity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ErrVolumeFull 是卷容量超限哨兵（fail-closed：调用方 errors.Is 识别并映射 507）。
var ErrVolumeFull = errors.New("volume capacity exceeded")

// Counter 是卷级容量记账抽象。
//
// TryAdd 在写入前累计 size，超限返回（包装 ErrVolumeFull）且不改变 used；
// Release 在删除/覆盖/改名后释放；Used/Capacity 供查询。
type Counter interface {
	TryAdd(size int64) error
	Release(size int64)
	Used() int64
	Capacity() int64
	// Flush 把待落盘的变更写入快照（幂等；无变更 no-op）。
	//
	// **落盘策略（评审 F9）**：**增加**（TryAdd）恒同步落盘——崩溃丢「增加」= used 偏低 =
	// 限额放宽（不可接受）；**减少**（Release）只标脏，由 Flush / 待写计数达阈值 / 下次增加
	// 触发——崩溃最多丢「减少」→ 重启后 used 偏高 → fail-closed（安全方向）。如此同卷写/删
	// 不再每次各付 tmp+fsync+rename+fsync(dir) 并在 mu 上串行化。
	Flush() error
}

// syncDir fsync 目录项（best-effort；Windows 上打开目录可能失败 → 忽略）。
func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // 目录路径由调用方给定（卷内元数据目录）
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if serr := d.Sync(); serr != nil {
		// 部分平台不支持目录 fsync（EINVAL/Windows）：非致命，忽略。
		return nil //nolint:nilerr // 目录 fsync 不支持的平台视为成功
	}
	return nil
}

// CapacityFS 是外部卷 sync.FS 装饰器（卷级记账强制 + 能力透传）。
//
// 记账规则（覆盖所有 FS 变更面，防重复计数）：
//   - WriteFile：stat 旧文件大小 prev，净增 delta=size-prev；delta>0 先 TryAdd（超限拒绝），
//     写成功后若 delta<0 释放；写失败回滚 delta。
//   - WriteIfAbsent：目标不存在先 TryAdd(size)，返回 (false,nil)/err 时回滚。
//   - Delete：stat 旧大小，删除成功后释放。
//   - Rename/Move：目标旧大小在成功后释放（源已在账上）。
//   - Copy：先 TryAdd(源大小)，成功后释放目标旧大小；失败回滚。
//   - Link：硬链接共享 inode，不新增字节，不计。
//
// 变更后 best-effort 持久化（counter 实现 Save 时）。
type CapacityFS struct {
	inner   syncpkg.FS
	counter Counter
}

// Wrap 包装 fs 为带卷级计数的 FS（counter 为任意 Counter 实现）。
func Wrap(fs syncpkg.FS, c Counter) *CapacityFS {
	return &CapacityFS{inner: fs, counter: c}
}

// WrapStorageVolume 是「**整卷容量** + **配额层**」的**唯一构造入口**（设计 §9.1 片 3b）。
//
// **为何收敛到一处**：整卷记账此前散在 **5 个装配点**各自 `WrapBackend`（配置卷 / 用户卷 /
// 封装卷 / secretdata / 恢复）⇒ **漏一处即「静默无限额」** ✗（新卷型、新装配路径不会报错）——
// 这正是设计 §0 的根因形态：记账是**横切关注点**却实现成 **N 个调用点各自的义务**。
// 本函数把「`CapacityFS` 执行 + `QuotaVolume` 持有」绑定为**一个原子动作** ⇒ **不可能只做一半** ✓；
// 并由 R29 门禁钉住（非测试源码不得直接调 `WrapBackend`）。
//
// **单账本保证**（**不是**两个账本 ✗）：`QuotaVolume` 与 `CapacityFS` 共享**同一个 `counter`
// 实例** ✓ ⇒ ① **执行**（写路径拦截、fail-closed）由 `CapacityFS` 负责；② **查询与结算**
// （`Capacity`/`VolumeUsed`/`Reserve` 的整卷侧）由 `QuotaVolume` 负责 —— 二者是同一本账 ✓。
// 故本函数不得传入两个不同来源的计数器 ✗。
//
// 分工边界（避免误改）：本函数**不**改变写路径的强制点（仍在 `CapacityFS` ✓）⇒ 行为不变 ✓；
// 当写内核改由 `QuotaVolume.Reserve` 统一预占时（设计片 5/6），才**同片删除** `CapacityFS`
// 的重复强制 ✗（现在删就是丢强制 ✗）。
func WrapStorageVolume(name string, be registry.ExternalBackend, scopes registry.UserScopeResolver, pool *quota.Pool, maxBytes, baselineUsed int64, opts ...WrapOption) registry.ExternalBackend {
	if be == nil {
		return nil
	}
	cfg := wrapConfig{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	// **唯一实现**：卷级记账 = 分片日志账本。账本底座默认是**原始**卷 FS（不经 `CapacityFS`
	// ⇒ 天然不自记账）；加密卷（secretdata）经 `WithLedgerDir` 改落**节点本地**明文目录
	// （写加密卷上会被加密成随机 blob，污染卷物理布局 ✗）。节点标识**内部自算**（hostname+pid）
	// ⇒ 装配点无选项 plumbing ⇒ **不可能漏接**（此前 5 个装配点各自 NewPoolCounter* 正是
	// 「漏一处即静默无限额」的根因）。
	var counter *LedgerCounter
	// `be.FS()` 只取一次（同一实例），既作数据面也（默认）作账本底座 ⇒ **不新增直连调用**。
	raw := be.FS()
	switch {
	case cfg.ledgerDir != "":
		counter = newVolumeLedgerCounterLocal(cfg.ledgerDir, maxBytes, pool, baselineUsed)
	case cfg.ledgerDirIfLocal != "":
		// 本地性探测在此（已取 raw）：仅本地卷落节点本地账本；共享卷账本留卷 FS。
		if lv, ok := raw.(syncpkg.LocalVolume); ok && lv.IsLocalVolume() {
			counter = newVolumeLedgerCounterLocal(cfg.ledgerDirIfLocal, maxBytes, pool, baselineUsed)
		} else {
			counter = newVolumeLedgerCounter(raw, maxBytes, pool, baselineUsed)
		}
	default:
		counter = newVolumeLedgerCounter(raw, maxBytes, pool, baselineUsed)
	}
	qv := registry.NewQuotaVolume(storageVolumeOf{name: name, ExternalBackend: be}, scopes, pool, counter, nil)
	// 🔴 **能力透传必须走原后端 `be`**（不是 `qv`）：`Backend` 以**内嵌**方式转发后端级可选能力
	// （`HealthProbe` / `Presigner` / `URLResolver` / `VolumeStatsProvider` / `StagingQuota*`）——
	// 若内嵌 `qv`（只有 Name/Kind/FS/Close ✗），这些能力会**静默丢失** ✗（实测：外部卷探针由
	// healthy/degraded 退化为 unknown ✗，`volumes_probe_test.go` 当场拦下 ✓）。
	// ⇒ **数据面**取配额层视图（`qv.FS()`，与 `be.FS()` 同一实例 ✓）、**能力面**取原后端 ✓。
	// `quota` 字段暴露配额层（统一查询面 ✓），与 `fs` 的 counter 共享同一实例 ✓（单账本）。
	return &Backend{ExternalBackend: be, fs: Wrap(raw, counter), quota: qv}
}

// WrapOption 定制 WrapStorageVolume 的装配行为。
type WrapOption func(*wrapConfig)

type wrapConfig struct {
	// ledgerDir 非空 ⇒ 账本落**节点本地**明文目录（否则默认建在卷 FS 上）。
	ledgerDir string
	// ledgerDirIfLocal 非空 ⇒ 账本落**节点本地**明文目录——**仅当卷 FS 自述本地**
	// （单实例专用）时；非本地（共享底层）⇒ 账本留卷 FS。
	ledgerDirIfLocal string
}

// WithLedgerDir 指定账本的**节点本地**落点（明文目录）。
//
// 默认账本建在卷 FS 上（`be.FS()`）；对**加密卷**（secretdata）必须传：账本写加密卷上
// 会被加密成随机名**内容分块** ⇒ 污染卷物理布局（E2E「内容分块清空」断言实测拦截）✗。
// 本地目录语义：secretdata `target: local` 为**单实例**专用（无跨进程共享需求）⇒
// 落节点本地目录正确，重启仍从已占用起算 ✓。
func WithLedgerDir(dir string) WrapOption {
	return func(c *wrapConfig) { c.ledgerDir = dir }
}

// WithLedgerDirIfLocal 指定账本的**节点本地**落点——**仅当卷 FS 自述本地**时生效。
//
// **为什么按本地性而非卷类型门控**（安全评审 2026-10-18）：加密卷（secretdata）在卷 FS 上
// 写账本会被加密成随机 blob（污染卷物理布局，E2E 断言拦截）⇒ 需要落本地；但若该加密卷的
// 底层是**共享卷**（多实例并发访问），落节点本地会**失去跨进程可见性** ⇒ 各实例独立计数
// ⇒ 配额强制被绕过（可超限）。故仅**本地**卷落本地账本；共享卷保留卷内账本（跨进程正确，
// 代价是 blob 形态——共享卷无「内容分块清空」判据）。
//
// 本地性探测在 `WrapStorageVolume` **内部**完成（已取卷 FS 一次）⇒ 装配点不再出现
// 卷 FS 直连（R27 门禁：`internal/archcheck/external_fs_calls.tsv` 无新增登记）。
func WithLedgerDirIfLocal(dir string) WrapOption {
	return func(c *wrapConfig) { c.ledgerDirIfLocal = dir }
}

// storageVolumeOf 把「外部后端 + 已知卷名」提升为**统一存储卷视图**。
//
// 为何需要：`registry.ExternalBackend` 只有 `FS()+Close()`（无 `Name()`/`Kind()` ✗）而
// `registry.StorageVolume` 要求四方法。上游装配点本就知道卷名 ⇒ **显式传入**
// （禁从 FS 反推卷名 ✗：那会把存储身份变成隐式契约）。
//
// **Kind 恒为 `KindStorage`**：本入口**仅**用于承载真实字节的存储卷
// （配置卷/用户卷/secretdata 底层卷等）。封装卷（`KindWrapper`）按设计 §1.4
// **零配额代码** ✗ ⇒ 它们不走本入口（保留 `WrapBackend`，见 R29 登记理由）。
type storageVolumeOf struct {
	registry.ExternalBackend
	name string
}

func (s storageVolumeOf) Name() string      { return s.name }
func (s storageVolumeOf) Kind() volume.Kind { return volume.KindStorage }

// Counter 返回底层计数器（查询/持久化）。
func (f *CapacityFS) Counter() Counter { return f.counter }

// NewVolumeCounter 在**原始**卷 FS 上构造卷级计数器（分片日志账本）。
//
// 供**不走** `WrapStorageVolume` 的装配路径使用（封装卷 `WrapBackend` 等）：语义与
// `WrapStorageVolume` 内部完全一致（同一实现、同一落点、同一节点标识）⇒ **仍只有一套**。
func NewVolumeCounter(raw syncpkg.FS, maxBytes int64, pool *quota.Pool) Counter {
	return newVolumeLedgerCounter(raw, maxBytes, pool, 0)
}

// VolumeLimitPath 返回用户卷**上限**的持久化路径（**F6 继承来源**）。
//
// 为什么上限**不放在账本里**（设计 §3.6.1）：账本必须落在**共享卷**上（跨进程可见 ✓），
// 而卷 FS 可能承载不了账本（⇒ 运行时降级为内存 ⇒ **不持久** ✗）⇒ 若上限依赖账本，
// F6（`delete+recreate` 以 `capacity:0` 继承旧上限）就会在那些卷上**静默失效** ✗。
// 上限是**配置**（非计数）⇒ 落**节点本地存储根**（恒可写 ✓、与卷 FS 能力无关 ✓）。
func VolumeLimitPath(storageRoot, owner, name string) string {
	return filepath.Join(storageRoot, owner, "meta", "volume", name+".limit")
}

// LoadVolumeLimit 返回创建用户卷时应使用的**上限**（**F6 语义唯一落点**，用户裁定 2026-10-12）：
//   - `unlimited` ⇒ 0（显式不限；调用方随后以 0 覆盖持久化值）；
//   - `capacity > 0` ⇒ 该值；
//   - `capacity == 0` 且未 unlimited ⇒ **继承已持久化上限**（否则 `delete+recreate` 写
//     `capacity:0` 即**解除上限** ✗ —— 远端字节仍在）。
//
// 读不到（不存在/损坏）⇒ 0（语义与「从未有过」一致）。
func LoadVolumeLimit(path string, capacity int64, unlimited bool) int64 {
	if unlimited || capacity > 0 {
		return capacity
	}
	b, err := os.ReadFile(path) //nolint:gosec // 路径由装配层给定的存储根拼接
	if err != nil {
		return 0
	}
	// 状态文件是**两行**（limit / used）⇒ 只取第一行（曾因整段 TrimSpace 后解析两行而恒 0 ✗）。
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	n, perr := strconv.ParseInt(strings.TrimSpace(lines[0]), 10, 64)
	if perr != nil || n < 0 {
		return 0
	}
	return n
}

// LoadVolumeUsed 读回已持久化的**用量基线**（不存在/损坏 ⇒ 0）。
//
// 为什么用量也要落本地（设计 §3.6.3）：卷内账本是**best-effort**（卷 FS 可能承载不了 ⇒ 内存降级
// ⇒ 不持久 ✗）；而「delete+recreate 不得归零绕过限额」是**硬要求** ✓ ⇒ 必须有**恒可用**的
// 本地基线 ✓（节点本地 POSIX 文件系统 ✓ 不依赖任何卷 FS 能力 ✓）。
// 账本仍是**权威**（强制与查询都用它 ✓）；本文件只是**创建时注入的种子** ✓。
func LoadVolumeUsed(path string) int64 {
	b, err := os.ReadFile(path) //nolint:gosec // 路径由装配层给定的存储根拼接
	if err != nil {
		return 0
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 2 {
		return 0
	}
	n, perr := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
	if perr != nil || n < 0 {
		return 0
	}
	return n
}

// WriteVolumeState 持久化用户卷的**生效上限 + 当前用量基线**（best-effort；单文件两行）。
//
// 单文件而非两个：二者同生命周期（随卷元数据一起保留/删除）⇒ 一份状态、一次原子写 ✓。
func WriteVolumeState(path string, limit, used int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := strconv.FormatInt(limit, 10) + "\n" + strconv.FormatInt(used, 10) + "\n"
	return os.WriteFile(path, []byte(body), 0o644) //nolint:gosec // 同上
}

// volumeNodeID 返回本进程的账本节点标识（hostname + pid）。
//
// 为何含 pid：同主机多进程必须写**不同分片**（否则共享分片 ⇒ 远端 FS 上读-改-写互相覆盖 ✗）。
// 代价：每次重启产生新分片（旧分片仍在 ⇒ 和仍正确），由对账后的分片回收收敛。
func volumeNodeID() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		h = "unknown"
	}
	return h + "-" + strconv.Itoa(os.Getpid())
}

// newVolumeLedgerCounter 在**原始**卷 FS 上建账本并适配为卷级计数器，同时持久化**上限**
// （F6 的继承来源）。建账本失败**不得静默无额度** ✗ ⇒ 返回降级计数器（所有写 fail-closed
// 并计入可观测指标，直到运维介入）。
func newVolumeLedgerCounter(raw syncpkg.FS, maxBytes int64, pool *quota.Pool, baselineUsed int64) *LedgerCounter {
	led, err := OpenLedgerFS(raw, LedgerDirName, volumeNodeID())
	if err != nil {
		// 卷 FS 承载不了账本（不支持建目录/列目录）⇒ **退回内存账本**：
		// 进程内上限**仍然强制**（不静默放宽 ✗），只是失去跨进程可见性 ⇒ 计数 + 告警。
		ledgerMemFallback.Add(1)
		mem, merr := OpenMemLedger(volumeNodeID())
		if merr != nil {
			return &LedgerCounter{max: maxBytes, initErr: merr}
		}
		if baselineUsed > 0 {
			_ = mem.Add(baselineUsed)
		}
		return NewLedgerCounter(mem, maxBytes).WithPool(pool)
	}
	// 基线注入（幂等）：仅当账本**当前为空**时写入 ⇒ 不会因重复装配而翻倍 ✓。
	if baselineUsed > 0 {
		if cur, ok := led.Total(); ok && cur == 0 {
			_ = led.Add(baselineUsed)
		}
	}
	return NewLedgerCounter(led, maxBytes).WithPool(pool)
}

// newVolumeLedgerCounterLocal 在**节点本地**明文目录上建账本并适配为卷级计数器
// （加密卷专用；语义与 newVolumeLedgerCounter 一致，只是落点不经卷 FS）。
func newVolumeLedgerCounterLocal(dir string, maxBytes int64, pool *quota.Pool, baselineUsed int64) *LedgerCounter {
	led, err := OpenLedgerLocal(dir, volumeNodeID())
	if err != nil {
		// 节点本地目录（节点盘）建账本失败 = **环境故障**（权限/磁盘）。**不得降级内存** ✗：
		// 降级 ⇒ 重启后账本空、used 回到陈旧基线 ⇒ 限额放宽（fail-open，本包最危险形态）。
		// 按 fail-closed：该卷拒绝新写入（`initErr` ⇒ TryAdd 拒 + Degraded 告警），
		// 直到运维修复本地目录。与卷 FS 版（newVolumeLedgerCounter）区分：卷 FS 承载不了
		// ⇒ 内存降级（跨进程 best-effort）；节点盘失败 ⇒ 全局故障，必须 fail-closed。
		return &LedgerCounter{max: maxBytes, initErr: err}
	}
	// 基线注入（幂等）：仅当账本**当前为空**时写入 ⇒ 不会因重复装配而翻倍 ✓。
	if baselineUsed > 0 {
		if cur, ok := led.Total(); ok && cur == 0 {
			_ = led.Add(baselineUsed)
		}
	}
	return NewLedgerCounter(led, maxBytes).WithPool(pool)
}

// WriteFromStagedPath 转发 staging 路径复用能力（未实现 → ErrUnsupported）。
func (f *CapacityFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	sp, ok := f.inner.(syncpkg.StagedPathReuse)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 WriteFromStagedPath: %w", syncpkg.ErrUnsupported)
	}
	return sp.WriteFromStagedPath(ctx, relPath, stagedPath, size, mtime)
}

// Inner 返回被包装的底层 FS（透明装饰器下探标记——trusted.Wrap 据此区分「转发层」
// 与「卷自带 meta」，避免把 CapacityFS 的 FileMeta 转发误判为自带 Provider）。
func (f *CapacityFS) Inner() syncpkg.FS { return f.inner }

// Flush 把待落盘的变更写入快照（关机/卸载卷/对账前调用；幂等）。
// **F9 说明**：`CapacityFS` 自身不再在每次写/删后落盘——落盘策略由计数器决定（增加恒同步、
// 减少节流），见 `Counter.Flush` 文档。
func (f *CapacityFS) Flush() error { return f.counter.Flush() }

// innerSize 返回 rel 当前大小（不存在/目录 → 0）。
func (f *CapacityFS) innerSize(ctx context.Context, rel string) int64 {
	if e, err := f.inner.Stat(ctx, rel); err == nil && e != nil && !e.IsDir {
		return e.Size
	}
	return 0
}

func (f *CapacityFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	return f.inner.ListDir(ctx, p)
}

func (f *CapacityFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.inner.Stat(ctx, p)
}

func (f *CapacityFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.inner.OpenRead(ctx, p)
}

func (f *CapacityFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, relPath string, r io.Reader, size, mtime int64) error {
	prev := f.innerSize(ctx, relPath)
	// 声明长度已知（>0）时先预留（超限 fail-closed 拒绝写入）；未知/非法（<=0，如
	// 分块传输 ContentLength=-1 / copyMeta）不预留，写后按**实测字节**结算。
	reserved := int64(0)
	if size > 0 {
		if d := size - prev; d > 0 {
			if err := f.counter.TryAdd(d); err != nil {
				return err
			}
			reserved = d
		}
	}
	cr := &countingReader{r: r}
	if err := f.inner.WriteFile(ctx, key, relPath, cr, size, mtime); err != nil {
		// **写错误 ≠ 未落盘**（评审 F2）：远端后端无原子 rename，响应超时/连接中断时对象
		// 可能已提交——此前一律 Release(reserved) 假定未落盘 ⇒ used 永久偏低（限额可穿透）。
		// 改为 Stat 复核实测并结算（无法复核则保留预留，宁可偏高）。
		f.settleAfterWriteError(ctx, relPath, prev, reserved, size)
		return err
	}
	// 结算：以实际写入（实测）为准，扣掉已预留。effective <= 0 时才回落声明值
	// （inner 忽略 reader 但仍写 size 字节的极端实现）。
	effective := cr.n
	if effective <= 0 && size > 0 {
		effective = size
	}
	if serr := f.settle(ctx, relPath, prev, reserved, effective, size); serr != nil {
		return serr
	}
	return nil
}

// settleAfterWriteError 是**写失败后**的记账复核（评审 F2）：远端后端（baidupcs/s3 等）
// 无原子 rename，「写错误」不代表「未落盘」——响应超时/连接中断时对象可能已提交。故不用
// 「释放预留」假定未落盘，而是 Stat 复核实测值并结算到实测（本地卷/未提交场景 Stat 仍见旧
// 对象 → 结算回旧口径，不泄漏占用）。
//
// **Stat 无法复核时保留预留不清账**（宁可偏高——限额宁紧勿松，运维可对账；绝不静默少计）。
func (f *CapacityFS) settleAfterWriteError(ctx context.Context, relPath string, prev, reserved, size int64) {
	actual, ok := f.innerSizeChecked(ctx, relPath)
	if !ok {
		return
	}
	if serr := f.settle(ctx, relPath, prev, reserved, actual, size); serr != nil {
		// 实测超限（未知长度路径）：与 settle 同处置——删除对象恢复「Σ占用 ≤ 卷限额」，
		// 并把预留退回旧口径（对象已删，不再占账）。
		_ = f.inner.Delete(ctx, relPath)
		f.counter.Release(reserved)
	}
}

// innerSizeChecked 返回 rel 的实测大小；ok=false = Stat 调用失败（**与「不存在」区分**——
// 不存在是 (0, true)，用于「复核不可用 → 宁可偏高」的判据）。
func (f *CapacityFS) innerSizeChecked(ctx context.Context, rel string) (int64, bool) {
	e, err := f.inner.Stat(ctx, rel)
	if err != nil {
		return 0, false
	}
	if e == nil || e.IsDir {
		return 0, true
	}
	return e.Size, true
}

// settle 写后结算：按实测补记/退还。未知长度路径（size<=0）无法写前 fail-closed——
// 若补记失败（超出卷限额），删除刚写对象并返回错误，恢复「Σ占用 ≤ 卷限额」不变量。
// 声明长度路径写前已预留，补记失败仅可能是并发漂移，记不了即保留（不误删已成功写入）。
func (f *CapacityFS) settle(ctx context.Context, relPath string, prev, reserved, effective, size int64) error {
	switch adj := (effective - prev) - reserved; {
	case adj > 0:
		if err := f.counter.TryAdd(adj); err != nil {
			if size <= 0 {
				_ = f.inner.Delete(ctx, relPath)
				return err
			}
		}
	case adj < 0:
		f.counter.Release(-adj) // 覆盖写变小 / 多预留
	}
	return nil
}

// countingReader 记录底层实际被读取的字节（记账以实测为准，防声明长度撒谎）。
// r 为 nil（仅声明长度的写，无内容源）时按空流处理——否则对 nil 解引用 panic。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.r == nil {
		return 0, io.EOF
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// WriteIfAbsent 原子唯一写 + 卷级记账（与 WriteFile 同口径：声明长度已知则写前预留，
// 未知/非法长度（<=0，如 copyMeta / 流式）不预留、写后按**实测**结算）。
func (f *CapacityFS) WriteIfAbsent(ctx context.Context, relPath string, r io.Reader, size, mtime int64) (bool, error) {
	pia, ok := f.inner.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("capacity: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	prev := f.innerSize(ctx, relPath)
	reserved := int64(0)
	if size > 0 {
		if d := size - prev; d > 0 {
			if err := f.counter.TryAdd(d); err != nil {
				return false, err
			}
			reserved = d
		}
	}
	cr := &countingReader{r: r}
	written, err := pia.WriteIfAbsent(ctx, relPath, cr, size, mtime)
	if err != nil {
		f.settleAfterWriteError(ctx, relPath, prev, reserved, size)
		return written, err
	}
	if !written {
		// **明确的未落盘答复**（幂等命中已存在对象）→ 回滚预留在本口径下是安全的
		// （与「写返回错误但可能已提交」区分开——后者走 settleAfterWriteError 复核）。
		if reserved > 0 {
			f.counter.Release(reserved)
		}
		return written, nil
	}
	effective := cr.n
	if effective <= 0 && size > 0 {
		effective = size
	}
	if serr := f.settle(ctx, relPath, prev, reserved, effective, size); serr != nil {
		return false, serr
	}
	return true, nil
}

func (f *CapacityFS) Rename(ctx context.Context, from, to string) error {
	targetOld := f.innerSize(ctx, to) // 覆盖目标旧字节（成功后释放）
	if err := f.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	f.counter.Release(targetOld)
	return nil
}

func (f *CapacityFS) MakeDir(ctx context.Context, relPath string) error {
	return f.inner.MakeDir(ctx, relPath)
}

func (f *CapacityFS) Delete(ctx context.Context, relPath string) error {
	size := f.innerSize(ctx, relPath)
	if err := f.inner.Delete(ctx, relPath); err != nil {
		return err
	}
	f.counter.Release(size)
	return nil
}

// ---- 可选能力透传（装饰器透明转发，否则包装后能力静默降级）----

// Move 委托 inner（Mover）；成功后释放目标旧字节。
func (f *CapacityFS) Move(ctx context.Context, from, to string) error {
	mv, ok := f.inner.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	targetOld := f.innerSize(ctx, to)
	if err := mv.Move(ctx, from, to); err != nil {
		return err
	}
	f.counter.Release(targetOld)
	return nil
}

// Copy 委托 inner（Copier）；先预留源大小，成功后释放目标旧字节。
func (f *CapacityFS) Copy(ctx context.Context, from, to string) error {
	cp, ok := f.inner.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	targetOld := f.innerSize(ctx, to)
	added := f.innerSize(ctx, from)
	if added > 0 {
		if err := f.counter.TryAdd(added); err != nil {
			return err
		}
	}
	if err := cp.Copy(ctx, from, to); err != nil {
		if added > 0 {
			f.counter.Release(added)
		}
		return err
	}
	f.counter.Release(targetOld)
	return nil
}

// Link 委托 inner（Linker）；硬链接共享 inode，不新增字节（不计）。
func (f *CapacityFS) Link(ctx context.Context, from, to string) error {
	lk, ok := f.inner.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	return lk.Link(ctx, from, to)
}

// ReserveSpace 委托 inner（卷自管理容量预检；inner 未实现 → ErrUnsupported）。
func (f *CapacityFS) ReserveSpace(ctx context.Context, p string, size int64) error {
	if rs, ok := f.inner.(syncpkg.ReserveSpace); ok {
		return rs.ReserveSpace(ctx, p, size)
	}
	return fmt.Errorf("capacity: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
}

// IsLocalVolume 委托 inner（未实现默认外部）。
func (f *CapacityFS) IsLocalVolume() bool {
	if lv, ok := f.inner.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// OpenRangeRead 委托 inner（RangeReader）。
func (f *CapacityFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if rr, ok := f.inner.(syncpkg.RangeReader); ok {
		return rr.OpenRangeRead(ctx, p, offset, size)
	}
	return nil, fmt.Errorf("capacity: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
}

// DirectURL 委托 inner（DirectURLProvider）。
func (f *CapacityFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if d, ok := f.inner.(syncpkg.DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("capacity: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
}

// FileMeta 委托 inner（meta.Provider：卷自带/装饰器 meta）。
func (f *CapacityFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if pv, ok := f.inner.(meta.Provider); ok {
		return pv.FileMeta(ctx, rel)
	}
	return nil, fmt.Errorf("capacity: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
}

// UpdateMetaExtra 委托 inner（damaged/transfer_verified 旁路标记）。
func (f *CapacityFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := f.inner.(interface {
		UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
	})
	if !ok {
		return fmt.Errorf("capacity: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return ue.UpdateMetaExtra(ctx, rel, extra)
}

// WithStagingQuota 委托 inner（StagingQuotaCapable 自管；per-instance 语义）。
func (f *CapacityFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if qc, ok := f.inner.(syncpkg.StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner（StagingQuotaExempt 显式豁免）。
func (f *CapacityFS) ExemptStagingQuota() bool {
	if ex, ok := f.inner.(syncpkg.StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// Backend 把外部卷 backend 的 FS 包为 CapacityFS（卷级记账），并暴露 Usage/Capacity 供
// 查询（registry.UsageProvider）。**内嵌 registry.ExternalBackend**：backend 级可选能力
// （HealthProbe/VolumeStatsProvider/Presigner/URLResolver）透明转发，不因记账包装而隐藏。
type Backend struct {
	registry.ExternalBackend
	fs    *CapacityFS
	quota *registry.QuotaVolume // 非 nil = 经唯一入口 WrapStorageVolume 构造（配额层可达 ✓）
}

// WrapBackend 包装外部卷 backend（FS 经 CapacityFS 记账）。
func WrapBackend(be registry.ExternalBackend, c Counter) *Backend {
	return &Backend{ExternalBackend: be, fs: Wrap(be.FS(), c)}
}

// Quota 返回该卷的**配额层**（`WrapStorageVolume` 构造时为非 nil ✓；经 `WrapBackend` 直包时为 nil ✗）。
//
// **为何需要它**（用户裁定 2026-10-16：「接线到真实消费者」）：`QuotaVolume` 是「整卷容量 +
// 每用户配额」的**统一查询面**（`Capacity`/`VolumeUsed`/`VolumeStats` ✓）；装配时若不暴露它，
// 消费方只能各自拼「池 Usage + 后端 UsageProvider」（全仓已有**两份重复** ✗）⇒ 多源即多漂移面 ✗。
//
// 单账本保证：`Quota()` 的 counter 与 `fs` 的 `CapacityFS` **共享同一实例** ✓（见 `WrapStorageVolume`）。
func (b *Backend) Quota() *registry.QuotaVolume {
	if b == nil {
		return nil
	}
	return b.quota
}

// FS 返回带卷级记账的 FS。
func (b *Backend) FS() syncpkg.FS { return b.fs }

// Flush 把卷级计数器的待落盘变更写入快照（关机 / 卸载卷 / 对账前调用；幂等）。
// F9：`Release` 是节流的——不 flush 会让本次进程的删除量丢失，下次启动 used 偏高 → 伪 507。
func (b *Backend) Flush() error { return b.fs.Flush() }

// Usage 返回本系统已占用该卷的字节（registry.UsageProvider，权威源 = 本层计数器）。
func (b *Backend) Usage() int64 { return b.fs.Counter().Used() }

// Capacity 返回本系统可用限额（0 = 不限）。
func (b *Backend) Capacity() int64 { return b.fs.Counter().Capacity() }

// Counter 返回底层计数器。
func (b *Backend) Counter() Counter { return b.fs.Counter() }

// ---- backend 级可选能力透传（inner 未实现 → syncpkg.ErrUnsupported 哨兵，
// 消费方 errors.Is 识别后按「不支持」处理，避免包装后能力静默降级）----

// Ping 委托 inner（HealthProbe）。
func (b *Backend) Ping(ctx context.Context) error {
	if hp, ok := b.ExternalBackend.(registry.HealthProbe); ok {
		return hp.Ping(ctx)
	}
	return fmt.Errorf("capacity: backend 未实现 Ping: %w", syncpkg.ErrUnsupported)
}

// Stats 委托 inner（VolumeStatsProvider）。
func (b *Backend) Stats(ctx context.Context) (*registry.VolumeStats, error) {
	if sp, ok := b.ExternalBackend.(registry.VolumeStatsProvider); ok {
		return sp.Stats(ctx)
	}
	return nil, fmt.Errorf("capacity: backend 未实现 Stats: %w", syncpkg.ErrUnsupported)
}

// OpenURL 委托 inner（URLResolver）。
func (b *Backend) OpenURL(ctx context.Context, url string) (io.ReadCloser, error) {
	if ur, ok := b.ExternalBackend.(registry.URLResolver); ok {
		return ur.OpenURL(ctx, url)
	}
	return nil, fmt.Errorf("capacity: backend 未实现 OpenURL: %w", syncpkg.ErrUnsupported)
}

// PresignedURL 委托 inner（Presigner）。
func (b *Backend) PresignedURL(ctx context.Context, relPath, method string, expires int64) (string, error) {
	if p, ok := b.ExternalBackend.(registry.Presigner); ok {
		return p.PresignedURL(ctx, relPath, method, expires)
	}
	return "", fmt.Errorf("capacity: backend 未实现 PresignedURL: %w", syncpkg.ErrUnsupported)
}

// SecretsManagerAny 委托 inner（secrets 卷的 Manager 反取）。
// 用 any 而非具体类型，避免本包依赖具体 secrets 实现；装配层 secrets.ManagerOfExternal
// 反取（未实现 → nil）。修对抗评审 P1：
// 此前 WrapBackend 只提升 {FS,Close}，动态值的 SecretsManager 不在提升集 →
// ManagerOfExternal 断言恒失败 → 配置的 `type: secrets` 卷不可达、密钥被写到默认卷。
func (b *Backend) SecretsManagerAny() any {
	if sp, ok := b.ExternalBackend.(interface{ SecretsManagerAny() any }); ok {
		return sp.SecretsManagerAny()
	}
	return nil
}

// 编译期断言：CapacityFS 实现 sync.FS 与**全部可选能力接口**（能力透传契约，防未来
// 新增能力接口后遗漏——装饰器不转发会让上层断言恒 false → 静默降级）。
// BlockAccessor 有意不实现（documented）：直接转发会让 sync 引擎对无该能力的底层
// 也走块级路径（ErrUnsupported 无法表达「未实现」），需 Capability 探测语义，见
// docs/designs/2026-10-10-trusted-volume.md §8。
var (
	_ syncpkg.FS = (*CapacityFS)(nil)
	_ Counter    = (*LedgerCounter)(nil)

	_ meta.Provider               = (*CapacityFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*CapacityFS)(nil)
	_ syncpkg.ReserveSpace        = (*CapacityFS)(nil)
	_ syncpkg.LocalVolume         = (*CapacityFS)(nil)
	_ syncpkg.Mover               = (*CapacityFS)(nil)
	_ syncpkg.Copier              = (*CapacityFS)(nil)
	_ syncpkg.Linker              = (*CapacityFS)(nil)
	_ syncpkg.RangeReader         = (*CapacityFS)(nil)
	_ syncpkg.DirectURLProvider   = (*CapacityFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*CapacityFS)(nil)
	_ syncpkg.StagedPathReuse     = (*CapacityFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*CapacityFS)(nil)
)
