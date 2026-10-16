// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"log/slog"

	"github.com/cocomhub/sproxy/pkg/quota"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
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

// QuotaVolume 是「**每用户 + 整卷**」配额层。
//
// 并发：与 `quota.Scope`/`quota.Pool` 一致（其内部持锁 ✓）；本类型无自有可变状态 ✓。
type QuotaVolume struct {
	inner  StorageVolume
	scopes UserScopeResolver
	pool   *quota.Pool // 整卷容量池（nil ⇒ 不限制）
	log    *slog.Logger
}

// NewQuotaVolume 包装存储卷为配额层（`inner` 必须非 nil：配额层不制造卷）。
func NewQuotaVolume(inner StorageVolume, scopes UserScopeResolver, pool *quota.Pool, log *slog.Logger) *QuotaVolume {
	if log == nil {
		log = slog.Default()
	}
	return &QuotaVolume{inner: inner, scopes: scopes, pool: pool, log: log}
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
func (q *QuotaVolume) Capacity() int64 {
	if q == nil || q.pool == nil {
		return 0
	}
	return q.pool.MaxBytes()
}

// SetCapacity 运行期调整整卷上限（0 = 取消上限）。无池 ⇒ no-op（不制造池）。
func (q *QuotaVolume) SetCapacity(n int64) {
	if q == nil || q.pool == nil {
		return
	}
	q.pool.SetMaxBytes(n)
}

// VolumeStats 返回 (已用, 总量, ok)。被包装卷实现 `volume.StatsProvider` 时**优先用后端真值**
// （外部卷可报远端总量 ✓）；否则用整卷池（已用 = committed、总量 = 上限 ✓）。
func (q *QuotaVolume) VolumeStats() (used, total int64, ok bool) {
	if q == nil {
		return 0, 0, false
	}
	if q.pool != nil {
		used, total, ok = q.pool.Usage(), q.pool.MaxBytes(), true
	}
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

// ---- 每用户配额（双账本预留句柄，与 `UploadRoute.Commit` 同形）----

// Reservation 是配额层的**双账本预留句柄**（每用户 Scope + 整卷池）。两侧各自至多一次
// Commit/Release（由 `quota.Reservation` 内部 CAS 保证幂等 ✓）。
type Reservation struct {
	scope    *quota.Scope
	scopeRes *quota.Reservation
	pool     *quota.Pool
	poolRes  *quota.Reservation
}

// Reserve 为 owner 预占 n 字节（**严格上界**，G1 机制）。任一侧拒绝（配额/卷容量不足）⇒
// 回滚已取得的一侧并返回 nil（**不允许只入一半账** ✓）。无配额能力 ⇒ (nil, nil) = no-op。
func (q *QuotaVolume) Reserve(owner string, n int64) (*Reservation, error) {
	if q == nil || n <= 0 {
		return nil, nil
	}
	r := &Reservation{}
	if sc := q.scopeFor(owner); sc != nil {
		rr, err := sc.TryReserve(n)
		if err != nil {
			return nil, err
		}
		r.scope, r.scopeRes = sc, rr
	}
	if q.pool != nil {
		rr, err := q.pool.TryReserve(n)
		if err != nil {
			r.Release()
			return nil, err
		}
		r.pool, r.poolRes = q.pool, rr
	}
	if r.scopeRes == nil && r.poolRes == nil {
		return nil, nil
	}
	return r, nil
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
	if r.scopeRes != nil {
		r.scopeRes.Release()
	}
	if r.poolRes != nil {
		r.poolRes.Release()
	}
}

// AdjustUsage 覆盖写/改名结算（**无需预留**）：把 owner 的占用从 prev 调整到 written。
func (q *QuotaVolume) AdjustUsage(owner string, prev, written int64) {
	if q == nil || written < 0 {
		return
	}
	if sc := q.scopeFor(owner); sc != nil {
		sc.Adjust(prev, written)
	}
	if q.pool != nil {
		q.pool.Adjust(prev, written)
	}
}

// ReleaseUsage 释放 owner 的 n 字节已确认占用（**删除成功后**调用；两侧各自钳 0，重复调用安全）。
func (q *QuotaVolume) ReleaseUsage(owner string, n int64) {
	if q == nil || n <= 0 {
		return
	}
	if sc := q.scopeFor(owner); sc != nil {
		sc.ReleaseUsage(n)
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
