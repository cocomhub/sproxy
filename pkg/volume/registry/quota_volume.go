// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"

	"github.com/cocomhub/sproxy/pkg/waitq"
)

// quota_volume.go —— **配额层**（统一存储卷模型 M4a；设计
// `docs/designs/2026-10-16-quota-volume-layer.md` §1.3）。
//
// 定位：包装一个**存储卷**（`KindStorage`），成为其配额的**唯一权威** —— 同时提供
// 「**每用户配额**」（owner → Scope）与「**整卷配额**」（卷容量池）。它位于存储卷之上、封装卷
// 之下 ⇒ 与真实字节之间**没有变换层** ⇒ 尺寸**天生物理**（`storage.Root.Stat` 在加密卷上即密文
// 长度）⇒ **无需知道卷是否加密** ✓。
//
// **本片（M4a）不挂载** ✗：与 M1a 同形 —— 只提供实现 + 测试，装配层仍走既有路径 ⇒ **零行为
// 变更** ✓。挂载（**同片删除上层重复记账**）是 M4b：迁移期两套账本并行会 `committed += 2×size`
// （已实证 ✗）⇒ **禁止**分片并存 ✓。
//
// 足迹与上界**委托被包装卷**（`volume.Footprinter`/`volume.PhysBound` 可选能力 ✓）：加密卷 = 公式 ✓、
// secretdata = 分片实测 ✓、外部卷 = 远端 stat ✓ —— **没有一种卷的物理尺寸能由调用方推出** ✗ ⇒
// 必须由卷自报；**能力缺失时显式不支持**（`ok=false` ✓，禁静默回落成逻辑值 ✗）。

// UserScopeResolver 按 owner 解析该用户的配额 Scope（nil = 该用户无配额约束 ⇒ no-op）。
type UserScopeResolver func(owner string) *quota.Scope

// VolumeCounter 是**整卷容量记账**的最小抽象（片 3）。
//
// **为何在此重复定义而不 import `pkg/volume/capacity`**：`capacity` 已 import 本包
// （`registry.ExternalBackend` ✓）⇒ 反向 import **成环** ✗。Go 接口是**结构性**的 ⇒ 在**消费者侧**
// 定义最小接口是惯用法 ✓。生产实现**唯一** = `capacity.LedgerCounter`（分片日志账本，
// 2026-10-17 迁移后旧 `PoolCounter`/`VolumeCapacityCounter` 已删除）。
//
// ⇒ 配额层因此**无需知道**整卷账本的实现形态（分片日志 / 跨进程 ✓）⇒ 「整卷权威单一」可
// **统一表达** ✓（片 3b 把 5 个装配点的 `capacity.WrapBackend` 收敛到本层时，只改注入点 ✓）。
type VolumeCounter interface {
	// TryAdd 在写入前累计 size；超限返回包装后的 ErrVolumeFull 且**不改变** used（fail-closed）。
	TryAdd(size int64) error
	// Release 在删除/覆盖/改名后释放。
	Release(size int64)
	// Used 返回当前占用（记账字节）。
	Used() int64
	// Capacity 返回上限（0 = 不限）。
	Capacity() int64
}

// QuotaVolume 是「**每用户 + 整卷**」配额层。
//
// 并发：与 `quota.Scope`/`quota.Pool` 一致（其内部持锁 ✓）；本类型无自有可变状态 ✓。
type QuotaVolume struct {
	inner   StorageVolume
	scopes  UserScopeResolver
	pool    *quota.Pool   // 整卷容量池（nil ⇒ 不限制）
	counter VolumeCounter // 整卷容量**权威**（非 nil 时优先于 pool —— 片 3）
	log     *slog.Logger
}

// NewQuotaVolume 包装存储卷为配额层（`inner` 必须非 nil：配额层不制造卷）。
//
// `counter` 是**整卷容量权威**（片 3 ✓）：生产唯一实现 `capacity.LedgerCounter`（分片日志
// 账本，跨进程正确 + 重启从已占用起算 ✓）；`nil` ⇒ 回落到 `pool` 语义（与 M4a 同 ✓）。
// **两者不得同时生效**（同时存在就是双账本 ✗）：非 nil 的 `counter` **优先**，且 `pool` 仅作
// 每用户 Scope 的容器（本层不会向它做整卷记账 ✗）。
func NewQuotaVolume(inner StorageVolume, scopes UserScopeResolver, pool *quota.Pool, counter VolumeCounter, log *slog.Logger) *QuotaVolume {
	if log == nil {
		log = slog.Default()
	}
	return &QuotaVolume{inner: inner, scopes: scopes, pool: pool, counter: counter, log: log}
}

// ---- StorageVolume（委托；**不加变换** —— 这正是「与真实字节之间无变换层」的含义 ✓）----

func (q *QuotaVolume) Name() string      { return q.inner.Name() }
func (q *QuotaVolume) Kind() volume.Kind { return q.inner.Kind() }
func (q *QuotaVolume) FS() syncpkg.FS    { return q.inner.FS() }
func (q *QuotaVolume) Close() error      { return q.inner.Close() }

// Inner 暴露被包装卷（过渡期访问器；M5 收敛后评估删除）。
func (q *QuotaVolume) Inner() StorageVolume { return q.inner }

// ---- 整卷配额 ----

// Capacity 返回整卷上限字节（0 = 不限制）。
//
// **权威优先**：注入的 `counter` 优先于 `pool`（片 3 ✓）—— 否则同一卷会有**两个**整卷账本 ✗。
func (q *QuotaVolume) Capacity() int64 {
	if q == nil {
		return 0
	}
	if q.counter != nil {
		return q.counter.Capacity()
	}
	if q.pool == nil {
		return 0
	}
	return q.pool.MaxBytes()
}

// VolumeUsed 返回整卷已用（记账字节）。(used, ok)；ok=false = 无整卷权威（未知，禁当 0 ✗）。
func (q *QuotaVolume) VolumeUsed() (int64, bool) {
	if q == nil {
		return 0, false
	}
	if q.counter != nil {
		return q.counter.Used(), true
	}
	if q.pool == nil {
		return 0, false
	}
	return q.pool.Usage(), true
}

// SetCapacity 运行期调整整卷上限（0 = 取消上限）。无池 ⇒ no-op（不制造池）。
//
// **仅池形态可调**：`counter` 形态（生产唯一实现 `LedgerCounter`）的上限由装配时
// `maxBytes` 固化、上限独立落节点本地 `.limit` 文件 ⇒ 运行期不在本层改 ✗ ——
// 静默 no-op 是可接受的，因为**无副作用** ✓。
func (q *QuotaVolume) SetCapacity(n int64) {
	if q == nil || q.pool == nil || q.counter != nil {
		return
	}
	q.pool.SetMaxBytes(n)
}

// VolumeStats 返回 (已用, 总量, ok)。被包装卷实现 `volume.StatsProvider` 时**优先用后端真值**
// （外部卷可报远端总量 ✓）；否则用整卷权威（`counter` 优先 ✓，再回落整卷池 ✓）。
func (q *QuotaVolume) VolumeStats() (used, total int64, ok bool) {
	if q == nil {
		return 0, 0, false
	}
	used, ok = q.VolumeUsed()
	total = q.Capacity()
	if sp, sok := q.inner.(volume.StatsProvider); sok {
		if bu, bt, bok := sp.VolumeStats(); bok {
			if bu > 0 {
				used = bu
			}
			if bt > 0 {
				total = bt
			}
			ok = true
		}
	}
	return used, total, ok
}

// ---- 每用户读侧（片 5：用户配额**按卷持有** ⇒ 联邦查询的**单卷**入口）----

// UserUsage 返回 owner 在**本卷**的已用字节（记账口径）。(used, ok)。
//
// **ok=false = 无每用户权威**（未装配 resolver / 该 owner 未登记）⇒ **未知** ✗ —— 调用方
// **禁当 0** ✗（联邦求和会把「不知道」静默算成「零用量」⇒ 少算 ✗）；与 `VolumeUsed` 同型 ✓。
//
// `owner` 按调用方契约传入（本层**不再归一** ✗ —— 避免双口径；`LogicalKey.Owner` 亦为原始值 ✓）。
// 返回的是该 owner 根 Scope 的**节点级**用量（`commitUp` 沿父链累计 ⇒ 等于该 owner 全子树 ✓）。
// WaitStats 返回该卷配额池的**等待队列统计**（S4「等待可观测」）。
// ok=false ⇒ 该卷没有配额池（未启用配额层）⇒ 无观测面（禁当「零等待」消费 ✗）。
func (q *QuotaVolume) WaitStats() (waitq.Stats, bool) {
	if q == nil || q.pool == nil {
		return waitq.Stats{}, false
	}
	return q.pool.WaitStats(), true
}

func (q *QuotaVolume) UserUsage(owner string) (int64, bool) {
	if q == nil {
		return 0, false
	}
	sc := q.scopeFor(owner)
	if sc == nil {
		return 0, false
	}
	return sc.Usage(), true
}

// UserCapacity 返回 owner 在**本卷**的上限字节（0 = 不限）。(cap, ok)；ok=false 语义同 `UserUsage`。
//
// 运行期改额（`SetMaxBytes`）后读侧跟随 ⇒ 配额调整**无需重建卷** ✓。
func (q *QuotaVolume) UserCapacity(owner string) (int64, bool) {
	if q == nil {
		return 0, false
	}
	sc := q.scopeFor(owner)
	if sc == nil {
		return 0, false
	}
	return sc.MaxBytes(), true
}

// ---- 每用户配额（双账本预留句柄，与 `UploadRoute.Commit` 同形）----

// Reservation 是配额层的**双账本预留句柄**（每用户 Scope + 整卷池）。两侧各自至多一次
// Commit/Release（由 `quota.Reservation` 内部 CAS 保证幂等 ✓）。
type Reservation struct {
	scope    *quota.Scope
	scopeRes *quota.Reservation
	pool     *quota.Pool
	poolRes  *quota.Reservation
	counter  VolumeCounter // 整卷权威（counter 形态；非 nil ⇒ **不用** pool 做整卷记账 ✗）
	n        int64         // 整卷侧预留量（结算差分用）
}

// Reserve 为 owner 预占 n 字节（**严格上界**，G1 机制）。任一侧拒绝（配额/卷容量不足）⇒
// 回滚已取得的一侧并返回 nil（**不允许只入一半账** ✓）。无配额能力 ⇒ (nil, nil) = no-op。
func (q *QuotaVolume) Reserve(key syncpkg.LogicalKey, n int64) (*Reservation, error) {
	if q == nil || n <= 0 {
		return nil, nil
	}
	// **零值键 = 未声明归属** ⇒ 显式不记账（禁按路径反推 ✗ —— 封装卷路径不可归属 ✓）。
	if key.IsZero() {
		q.warnNoAttribution(n)
		return nil, nil
	}
	r := &Reservation{}
	if err := q.reserveUser(r, key, n); err != nil {
		return nil, err
	}
	if err := q.reserveVolume(r, n); err != nil {
		r.Release() // 回滚已取得的每用户侧预留（**禁半入账** ✗）
		return nil, err
	}
	if r.scopeRes == nil && r.poolRes == nil && r.counter == nil {
		return nil, nil
	}
	return r, nil
}

// warnNoAttribution 记录「未声明归属」的可观测痕迹（遵守「生效状态必须可观测」✓）。
func (q *QuotaVolume) warnNoAttribution(n int64) {
	if q.log != nil {
		q.log.Debug("quota: 写入未声明归属（LogicalKey 零值）⇒ 跳过记账", "n", n)
	}
}

// reserveUser 取得**每用户侧**预留（失败 ⇒ 调用方直接返回，此时尚未取得任何预留 ✓）。
func (q *QuotaVolume) reserveUser(r *Reservation, key syncpkg.LogicalKey, n int64) error {
	sc := q.scopeFor(key.Owner)
	if sc == nil {
		return nil
	}
	rr, err := sc.TryReserve(n)
	if err != nil {
		return err
	}
	r.scope, r.scopeRes = sc, rr
	return nil
}

// reserveVolume 取得**整卷侧**预留：counter 权威优先，与 pool **互斥**（禁双账本 ✗）。
func (q *QuotaVolume) reserveVolume(r *Reservation, n int64) error {
	if q.counter != nil {
		if err := q.counter.TryAdd(n); err != nil {
			return err
		}
		r.counter, r.n = q.counter, n
		return nil
	}
	if q.pool == nil {
		return nil
	}
	rr, err := q.pool.TryReserve(n)
	if err != nil {
		return err
	}
	r.pool, r.poolRes = q.pool, rr
	return nil
}

// Commit 把预留结算为实际占用（**prev → written 差分**；`prev <= 0` = 新文件）。
//
// 口径契约（与 `UploadRoute.Commit` 同）：两参数都必须为**物理**字节，且**与预留同口径** ——
// `prev` 取卷的实测足迹（覆盖写旧值 ✓），`written` 取本次实测 ✓；否则少释放/多释放都会漂移。
func (r *Reservation) Commit(prev, written int64) {
	if r == nil || written < 0 {
		return
	}
	// 覆盖写（prev > 0）：**先按差分调整已确认占用，再归还预留**（与 `UploadRoute.Commit` 同形）——
	// 若只 `Commit(written)` 而不释放 prev，会把旧字节双计 ✗。
	if r.scopeRes != nil {
		if prev > 0 {
			r.scope.Adjust(prev, written)
			r.scopeRes.Release()
		} else {
			r.scopeRes.Commit(written)
		}
	}
	// counter 形态：把预留 `n` 收敛到 `written`（覆盖写额外释放 `prev`）——
	// 净释放 = n + prev − written ✓（这正是「恰好一次」：counter 只被这一处调整 ✓）。
	if r.counter != nil {
		if d := r.n + prev - written; d > 0 {
			r.counter.Release(d)
		} else if d < 0 {
			// 实测 **超出**预留（如未声明长度的大写入）：字节已在盘上 ⇒ **必须记账** ✓
			// （决议 #5：半途失败按实测结算 ✓）。此处忽略错误：`TryAdd` 的 fail-closed 是
			// **写前准入**语义，而这里是**写后结算**（不记账 = 幽灵占用 ✗，比超额更坏）。
			_ = r.counter.TryAdd(-d)
		}
		r.counter, r.n = nil, 0 // 防重复结算
	}
	if r.poolRes != nil {
		if prev > 0 {
			r.pool.Adjust(prev, written)
			r.poolRes.Release()
		} else {
			r.poolRes.Commit(written)
		}
	}
}

// Release 放弃预留（归还两侧已预留额度）；nil 安全、重复调用安全。
func (r *Reservation) Release() {
	if r == nil {
		return
	}
	if r.counter != nil {
		r.counter.Release(r.n)
		r.counter, r.n = nil, 0
	}
	if r.scopeRes != nil {
		r.scopeRes.Release()
	}
	if r.poolRes != nil {
		r.poolRes.Release()
	}
}

// AdjustUsage 覆盖写/改名结算（**无需预留**）：把 owner 的占用从 prev 调整到 written。
// 整卷侧同样以 counter 为权威（非 nil 时不用 pool ✗）。
func (q *QuotaVolume) AdjustUsage(key syncpkg.LogicalKey, prev, written int64) {
	if q == nil || written < 0 || key.IsZero() {
		return
	}
	if sc := q.scopeFor(key.Owner); sc != nil {
		sc.Adjust(prev, written)
	}
	if q.counter != nil {
		if d := written - prev; d > 0 {
			_ = q.counter.TryAdd(d)
		} else if d < 0 {
			q.counter.Release(-d)
		}
		return
	}
	if q.pool != nil {
		q.pool.Adjust(prev, written)
	}
}

// ReleaseUsage 释放 owner 的 n 字节已确认占用（**删除成功后**调用；两侧各自钳 0，重复调用安全）。
func (q *QuotaVolume) ReleaseUsage(key syncpkg.LogicalKey, n int64) {
	if q == nil || n <= 0 || key.IsZero() {
		return
	}
	if sc := q.scopeFor(key.Owner); sc != nil {
		sc.ReleaseUsage(n)
	}
	if q.counter != nil {
		q.counter.Release(n)
		return
	}
	if q.pool != nil {
		q.pool.ReleaseCommitted(n)
	}
}

// scopeFor 解析 owner 的配额 Scope（nil = 无约束）。
func (q *QuotaVolume) scopeFor(owner string) *quota.Scope {
	if q == nil || q.scopes == nil {
		return nil
	}
	return q.scopes(owner)
}

// ---- 足迹 / 上界（委托被包装卷；能力缺失 ⇒ 显式不支持）----

// Footprint 返回 (owner, rel) 对象在本卷上的**物理足迹**：优先被包装卷的 `volume.Footprinter`；
// 能力缺失 ⇒ `ok=false`（**不静默回落成逻辑值** ✗ —— 由调用方显式处置 + 可观测）。
func (q *QuotaVolume) Footprint(owner, rel string) (int64, bool) {
	if q == nil {
		return 0, false
	}
	if fp, ok := q.inner.(volume.Footprinter); ok {
		return fp.Footprint(owner, rel)
	}
	return 0, false
}

// MaxPhysical 返回 logical 字节在本卷上的**记账上界**（写前预占用）：优先被包装卷的
// `volume.PhysBound`；能力缺失 ⇒ 返回 logical（**保守下界**：宁可少预留，也不越界记账 ✗；
// 调用方须在写后按 `Footprint` **实测结算** ✓）。
func (q *QuotaVolume) MaxPhysical(owner, rel string, logical int64) int64 {
	if logical <= 0 {
		return logical
	}
	if q != nil {
		if pb, ok := q.inner.(volume.PhysBound); ok {
			return pb.MaxPhysical(owner, rel, logical)
		}
	}
	return logical
}

// 编译期：配额层是**存储卷**（可向上作为统一视图），并实现足迹/上界/整卷容量/统计能力。
var (
	_ StorageVolume        = (*QuotaVolume)(nil)
	_ volume.Footprinter   = (*QuotaVolume)(nil)
	_ volume.PhysBound     = (*QuotaVolume)(nil)
	_ volume.CapacityOwner = (*QuotaVolume)(nil)
	_ volume.StatsProvider = (*QuotaVolume)(nil)
)
