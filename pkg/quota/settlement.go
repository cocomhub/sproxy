// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"errors"
	"sync"
)

// settlement.go 实现**配额结算状态机** `Settlement`（设计 docs/designs/2026-10-31-policy-object-extraction.md §2.2）。
//
// 背景：`Reserve → 写入 → Reconcile → Commit/Release` 这一顺序此前散落在 cloud 归档、卷迁移、
// 版本保存、S3 complete 等调用点，全靠每处自觉——顺序错或漏回滚会直接造成**配额泄漏**
// （预留永不归还 ⇒ 该租户假满）或**超卖**（未对账就提交 ⇒ 账本小于磁盘）。Settlement 把这一
// 协议收敛成唯一实现：一个句柄 = 一段已预留但尚未定案的用量。
//
// 状态机（非法迁移返回哨兵错误，绝不 panic）：
//
//	open ──Reserve/Reconcile（可多次）──▶ open
//	open ──Commit──▶ committed（终态：committed=已提交量，预留归零）
//	open ──Release──▶ released（终态：预留全额归还）
//	committed ──Commit──▶ committed（幂等，账本不变）
//	committed ──Release──▶ committed（空操作，不撤销已确认占用）
//	released ──Release──▶ released（幂等）
//	released ──Commit/Reserve/Reconcile──▶ ErrSettlementReleased
//	committed ──Reserve/Reconcile──▶ ErrSettlementCommitted
//
// **为何不持有 `Reservation` 句柄**：`Reservation` 只支持「整笔 Commit/Release」，无法就地
// 缩小预留；`Reconcile` 若实现为「先整笔 Release 再重新 TryReserve」，中间窗口会把额度让给
// 并发写入者，收敛可能因瞬时竞争失败（而数据已在盘上 ⇒ 记账漂移）。故 Settlement 直接按
// **增量**操作账本（`reserveUp` / `releaseReserve` / `commitReservation`，三者已含父链聚合与
// 「释放必广播」的丢唤醒修复，见 quota.go 的 R34 口径）。
type Settlement struct {
	mu sync.Mutex
	// pool 是结算目标账本（根池或某作用域的底层池；父链聚合由账本自身完成）。
	// nil 表示未绑定目标 ⇒ 所有改变账本的操作 fail-closed（ErrSettlementNoTarget）。
	pool *Pool
	// reserved 是本句柄**当前持有**的预留量（始终等于它在账本上占用的字节数）。
	reserved int64
	// committed 是 Commit 后定案的已提交量（仅 committed 终态有意义，供审计/诊断读取）。
	committed int64
	state     State
}

// State 是 Settlement 状态机状态。
type State uint8

const (
	// StateOpen 是初始态：可 Reserve/Reconcile，并最终 Commit 或 Release。
	StateOpen State = iota
	// StateCommitted 是终态：预留已按实际值转为已确认占用，句柄不再改动账本。
	StateCommitted
	// StateReleased 是终态：未提交的预留已全额归还，句柄不再改动账本。
	StateReleased
)

// String 返回状态名（日志/诊断可读；未定义值返回 "unknown"）。
func (st State) String() string {
	switch st {
	case StateOpen:
		return "open"
	case StateCommitted:
		return "committed"
	case StateReleased:
		return "released"
	default:
		return "unknown"
	}
}

// Settlement 的哨兵错误（状态机非法迁移；调用方用 errors.Is 判定）。
var (
	// ErrSettlementCommitted 表示句柄已提交，不可再 Reserve/Reconcile。
	ErrSettlementCommitted = errors.New("quota: settlement 已提交")
	// ErrSettlementReleased 表示句柄已释放，不可再 Reserve/Reconcile/Commit。
	ErrSettlementReleased = errors.New("quota: settlement 已释放")
	// ErrSettlementNoTarget 表示未绑定配额账本（NewSettlement(nil)），改变账本的操作 fail-closed。
	ErrSettlementNoTarget = errors.New("quota: settlement 未绑定配额账本")
)

// NewSettlement 创建绑定 pool 的结算句柄（初始 StateOpen，未预留任何字节）。
// 作用域调用方传 `scope.Pool()`——作用域只是同一条账本的视图，语义无差别。
// pool 为 nil ⇒ 句柄可用但改变账本的操作返回 ErrSettlementNoTarget（不 panic）。
func NewSettlement(pool *Pool) *Settlement {
	return &Settlement{pool: pool}
}

// Reserve 在账本上预留 n（负数按 0 处理）；可多次调用，累加为同一笔预留。
// 容量不足返回 ErrStorageFull（沿父链任一层）且账本不变。
func (s *Settlement) Reserve(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return err
	}
	amt := nonNeg(n)
	if amt == 0 {
		return nil
	}
	if s.pool == nil {
		return ErrSettlementNoTarget
	}
	if err := s.pool.reserveUp(amt); err != nil {
		return err
	}
	s.reserved += amt
	return nil
}

// Reconcile 把预留**收敛到落盘实际值** actual（负数按 0 处理）：预留大于实际 ⇒ 归还差额并
// 唤醒等待者；小于实际 ⇒ 追加预留。
//
// 追加预留可能因容量不足返回 ErrStorageFull——此时预留**保持调用前的值**且状态仍为 open：
// 实际字节已落盘（账本欠记），调用方须显式处理（重试/按可提交量 Commit/Release 后走对账）。
func (s *Settlement) Reconcile(actual int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.requireOpenLocked(); err != nil {
		return err
	}
	target := nonNeg(actual)
	switch {
	case target == s.reserved:
		return nil
	case s.pool == nil:
		return ErrSettlementNoTarget
	case target > s.reserved:
		if err := s.pool.reserveUp(target - s.reserved); err != nil {
			return err
		}
	default:
		// 缩小预留释放了可用额度 ⇒ releaseReserve 在根等待锁内变更并广播（防丢唤醒）。
		s.pool.releaseReserve(s.reserved - target)
	}
	s.reserved = target
	return nil
}

// Commit 把当前预留定案为已确认占用（幂等：重复调用不重复记账）。
// 已释放的句柄返回 ErrSettlementReleased（不允许把已回滚的额度再抬回账本）。
// 已提交的句柄再次 Commit 返回 nil（幂等，账本不变）。
func (s *Settlement) Commit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.state {
	case StateCommitted:
		return nil
	case StateReleased:
		return ErrSettlementReleased
	}
	if s.pool == nil {
		return ErrSettlementNoTarget
	}
	amount := s.reserved
	// amount → amount：reserved 转为 committed，可用额度不变（commitReservation 的快路不抢根等待锁）。
	s.pool.commitReservation(amount, amount)
	s.committed = amount
	s.reserved = 0
	s.state = StateCommitted
	return nil
}

// Release 放弃预留（幂等）：open 态全额归还账本并进入 released；committed 态为空操作
// （已确认占用不能被撤销——删除文件走 Scope.ReleaseUsage）；released 态为空操作。
func (s *Settlement) Release() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != StateOpen {
		return
	}
	if s.pool != nil && s.reserved > 0 {
		s.pool.releaseReserve(s.reserved)
	}
	s.reserved = 0
	s.state = StateReleased
}

// State 返回当前状态（便于测试与可观测）。
func (s *Settlement) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Reserved 返回本句柄**当前持有**的预留言节数（已提交后为 0）。
func (s *Settlement) Reserved() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserved
}

// Committed 返回 Commit 定案的已提交量（未提交为 0）。
func (s *Settlement) Committed() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.committed
}

// requireOpenLocked 校验句柄仍可改变预留（调用方须已持 s.mu）。
func (s *Settlement) requireOpenLocked() error {
	switch s.state {
	case StateOpen:
		return nil
	case StateCommitted:
		return ErrSettlementCommitted
	default: // StateReleased（state 只由本类型写入，无其它取值）
		return ErrSettlementReleased
	}
}
