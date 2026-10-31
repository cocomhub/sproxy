// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"
)

// settlementNew 建「根池 → 子作用域」两级账本并返回绑定该作用域的 Settlement：
// reserved/committed 都必须同时体现在子作用域与根池（父链聚合），测试因此能发现单层记账漏项。
func settlementNew(t *testing.T, poolCap int64) (*Pool, *Scope, *Settlement) {
	t.Helper()
	root := NewPool(poolCap)
	scope := root.EnsureScope([]string{"tenant", "alice"}, poolCap)
	if scope == nil {
		t.Fatalf("EnsureScope(cap=%d) 应成功", poolCap)
	}
	return root, scope, NewSettlement(scope.Pool())
}

// settlementOp 是结算句柄上的一次操作（表驱动用例的 prepare/act）。
type settlementOp func(*Settlement) error

func settleReserve(s *Settlement) error   { return s.Reserve(10) }
func settleReconcile(s *Settlement) error { return s.Reconcile(4) }
func settleCommit(s *Settlement) error    { return s.Commit() }

func settleRelease(s *Settlement) error {
	s.Release()
	return nil
}

// TestSettlement_StateMachine 表驱动覆盖全部状态迁移：合法迁移改账本，非法迁移返回哨兵错误
// 且**账本一字不动**（幂等性由此在同一张表里被锁定）。
func TestSettlement_StateMachine(t *testing.T) {
	t.Parallel()
	preReserved := []settlementOp{settleReserve}
	preCommitted := []settlementOp{settleReserve, settleCommit}
	preReleased := []settlementOp{settleReserve, settleRelease}
	cases := []struct {
		name        string
		prepare     []settlementOp
		act         settlementOp
		wantErr     error
		wantState   State
		wantUsage   int64
		wantReserve int64
	}{
		{name: "open·reserve", act: settleReserve, wantState: StateOpen, wantReserve: 10},
		{name: "open·reconcile", prepare: preReserved, act: settleReconcile, wantState: StateOpen, wantReserve: 4},
		{name: "open·commit", prepare: preReserved, act: settleCommit, wantState: StateCommitted, wantUsage: 10},
		{name: "open·release", prepare: preReserved, act: settleRelease, wantState: StateReleased},
		{
			name: "committed·reserve", prepare: preCommitted, act: settleReserve,
			wantErr: ErrSettlementCommitted, wantState: StateCommitted, wantUsage: 10,
		},
		{
			name: "committed·reconcile", prepare: preCommitted, act: settleReconcile,
			wantErr: ErrSettlementCommitted, wantState: StateCommitted, wantUsage: 10,
		},
		// Commit 幂等：重复提交不改变账本。
		{name: "committed·commit", prepare: preCommitted, act: settleCommit, wantState: StateCommitted, wantUsage: 10},
		// 已提交后的 Release 是空操作：不撤销已确认占用（否则删除文件时会凭空释放他人额度）。
		{name: "committed·release", prepare: preCommitted, act: settleRelease, wantState: StateCommitted, wantUsage: 10},
		{
			name: "released·reserve", prepare: preReleased, act: settleReserve,
			wantErr: ErrSettlementReleased, wantState: StateReleased,
		},
		{
			name: "released·reconcile", prepare: preReleased, act: settleReconcile,
			wantErr: ErrSettlementReleased, wantState: StateReleased,
		},
		// Release 后不得再 Commit：返回哨兵错误而不 panic，且不得把预留抬回账本。
		{
			name: "released·commit", prepare: preReleased, act: settleCommit,
			wantErr: ErrSettlementReleased, wantState: StateReleased,
		},
		{name: "released·release", prepare: preReleased, act: settleRelease, wantState: StateReleased},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, scope, st := settlementNew(t, 1000)
			for _, op := range tc.prepare {
				if err := op(st); err != nil {
					t.Fatalf("prepare 失败: %v", err)
				}
			}
			err := tc.act(st)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("err=%v want nil", err)
			}
			if got := st.State(); got != tc.wantState {
				t.Fatalf("State()=%v want %v", got, tc.wantState)
			}
			// 账本断言覆盖两级（子作用域 + 根池）：单层记账/漏传播都会红。
			if got := scope.Usage(); got != tc.wantUsage {
				t.Fatalf("scope.Usage()=%d want %d", got, tc.wantUsage)
			}
			if got := root.Usage(); got != tc.wantUsage {
				t.Fatalf("root.Usage()=%d want %d（父链聚合）", got, tc.wantUsage)
			}
			if got := scope.Reserved(); got != tc.wantReserve {
				t.Fatalf("scope.Reserved()=%d want %d", got, tc.wantReserve)
			}
			if got := root.Reserved(); got != tc.wantReserve {
				t.Fatalf("root.Reserved()=%d want %d（父链聚合）", got, tc.wantReserve)
			}
		})
	}
}

// TestState_String 锁定可观测口径（日志/诊断里打印状态名，而不是裸数字）。
func TestState_String(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		state State
		want  string
	}{
		{StateOpen, "open"},
		{StateCommitted, "committed"},
		{StateReleased, "released"},
		{State(99), "unknown"},
	} {
		if got := tc.state.String(); got != tc.want {
			t.Fatalf("State(%d).String()=%q want %q", tc.state, got, tc.want)
		}
	}
}

// TestSettlement_ReserveAccumulates 多次 Reserve 累加为同一笔预留（同一结算窗口内的分段预估）。
func TestSettlement_ReserveAccumulates(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 1000)
	if err := st.Reserve(30); err != nil {
		t.Fatalf("Reserve(30): %v", err)
	}
	if err := st.Reserve(70); err != nil {
		t.Fatalf("Reserve(70): %v", err)
	}
	if got := st.Reserved(); got != 100 {
		t.Fatalf("Reserved()=%d want 100", got)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := st.Committed(); got != 100 {
		t.Fatalf("Committed()=%d want 100", got)
	}
	if got := scope.Usage(); got != 100 {
		t.Fatalf("scope.Usage()=%d want 100", got)
	}
	if got := root.Usage(); got != 100 {
		t.Fatalf("root.Usage()=%d want 100", got)
	}
}

// TestSettlement_ReconcileConvergesToActual 按落盘真值收敛预留：小于/等于/大于预估都必须落账实际值。
func TestSettlement_ReconcileConvergesToActual(t *testing.T) {
	t.Parallel()
	for _, actual := range []int64{0, 40, 100, 250} {
		t.Run("actual", func(t *testing.T) {
			t.Parallel()
			root, scope, st := settlementNew(t, 1000)
			if err := st.Reserve(100); err != nil { // 预估 100
				t.Fatalf("Reserve: %v", err)
			}
			if err := st.Reconcile(actual); err != nil {
				t.Fatalf("Reconcile(%d): %v", actual, err)
			}
			if got := st.Reserved(); got != actual {
				t.Fatalf("Reserved()=%d want %d", got, actual)
			}
			if got := root.Reserved(); got != actual {
				t.Fatalf("root.Reserved()=%d want %d", got, actual)
			}
			if err := st.Commit(); err != nil {
				t.Fatalf("Commit: %v", err)
			}
			if got := st.Committed(); got != actual {
				t.Fatalf("Committed()=%d want %d", got, actual)
			}
			if got := scope.Usage(); got != actual {
				t.Fatalf("scope.Usage()=%d want %d", got, actual)
			}
			if got := root.Usage(); got != actual {
				t.Fatalf("root.Usage()=%d want %d", got, actual)
			}
			if got := scope.Reserved(); got != 0 {
				t.Fatalf("提交后 scope.Reserved()=%d want 0", got)
			}
		})
	}
}

// TestSettlement_ReconcileExceedsCapacityFailsClosed 收敛到超出容量的实际值时 fail-closed：
// 返回 ErrStorageFull、预留保持调用前值、状态仍可继续处理（调用方可重试或 Release）。
func TestSettlement_ReconcileExceedsCapacityFailsClosed(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	if err := st.Reserve(80); err != nil {
		t.Fatalf("Reserve(80): %v", err)
	}
	if err := st.Reconcile(120); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("Reconcile(120) err=%v want ErrStorageFull", err)
	}
	if got := st.Reserved(); got != 80 {
		t.Fatalf("失败后 Reserved()=%d want 80（不得改动预留）", got)
	}
	if got := root.Reserved(); got != 80 {
		t.Fatalf("失败后 root.Reserved()=%d want 80", got)
	}
	if got := st.State(); got != StateOpen {
		t.Fatalf("State()=%v want open", got)
	}
	if err := st.Reconcile(120); err == nil {
		// 预留仍未改（重复失败不累积），随后按已落盘的实际可提交部分处理。
		t.Fatal("第二次超容量收敛仍应失败")
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := scope.Usage(); got != 80 {
		t.Fatalf("scope.Usage()=%d want 80", got)
	}
	if got := st.Committed(); got != 80 {
		t.Fatalf("Committed()=%d want 80", got)
	}
}

// TestSettlement_ReserveExceedsCapacityFailsClosed 预留超容量返回 ErrStorageFull 且账本不变
// （句柄仍为 open，可对已能放下的部分继续处理）。
func TestSettlement_ReserveExceedsCapacityFailsClosed(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	if err := st.Reserve(150); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("Reserve(150) err=%v want ErrStorageFull", err)
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("失败后 Reserved()=%d want 0", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("失败后 scope.Reserved()=%d want 0", got)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("失败后 root.Reserved()=%d want 0", got)
	}
	if got := st.State(); got != StateOpen {
		t.Fatalf("State()=%v want open", got)
	}
	// 句柄未被失败污染：可继续按可放下的量预留并提交。
	if err := st.Reserve(100); err != nil {
		t.Fatalf("Reserve(100): %v", err)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := scope.Usage(); got != 100 {
		t.Fatalf("scope.Usage()=%d want 100", got)
	}
}

// TestSettlement_ReleaseRollsBackReserve 未 Commit 的 Settlement 在 Release 时全额回滚，
// 且额度真的可用（后续同区间的预留能成功）——直接证明「不泄漏」。
func TestSettlement_ReleaseRollsBackReserve(t *testing.T) {
	t.Parallel()
	root := NewPool(1000)
	scope := root.EnsureScope([]string{"tenant", "alice"}, 600)
	if scope == nil {
		t.Fatal("EnsureScope 应成功")
	}
	st := NewSettlement(scope.Pool())
	if err := st.Reserve(500); err != nil {
		t.Fatalf("Reserve(500): %v", err)
	}
	if got := scope.Available(); got != 100 {
		t.Fatalf("预留后 Available()=%d want 100", got)
	}
	st.Release()
	if got := root.Reserved(); got != 0 {
		t.Fatalf("Release 后 root.Reserved()=%d want 0", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("Release 后 scope.Reserved()=%d want 0", got)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("未提交的 Release 不得写入 committed, got %d", got)
	}
	if got := scope.Available(); got != 600 {
		t.Fatalf("Release 后 Available()=%d want 600（额度已归还）", got)
	}
	// 幂等：重复 Release 不再改动账本。
	st.Release()
	st.Release()
	if got := root.Reserved(); got != 0 {
		t.Fatalf("重复 Release 后 root.Reserved()=%d want 0", got)
	}
	// 额度确实可被再次占用（回滚真实生效）。
	again := NewSettlement(scope.Pool())
	if err := again.Reserve(600); err != nil {
		t.Fatalf("Release 后同区间重新预留应成功: %v", err)
	}
}

// TestSettlement_CommitAndReleaseIdempotent 直接锁定两条终态的幂等账本（不重复记账/不重复回滚）。
func TestSettlement_CommitAndReleaseIdempotent(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 1000)
	if err := st.Reserve(200); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	for i := range 3 {
		if err := st.Commit(); err != nil {
			t.Fatalf("第 %d 次 Commit: %v", i, err)
		}
	}
	if got := scope.Usage(); got != 200 {
		t.Fatalf("重复 Commit 后 scope.Usage()=%d want 200（不得重复记账）", got)
	}
	if got := root.Usage(); got != 200 {
		t.Fatalf("重复 Commit 后 root.Usage()=%d want 200", got)
	}
	if got := st.Committed(); got != 200 {
		t.Fatalf("Committed()=%d want 200", got)
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("提交后 Reserved()=%d want 0（预留必须归零，否则重复 Commit 会二次记账）", got)
	}
	// 已提交的 Release 是空操作：已确认占用不得被撤销。
	for range 3 {
		st.Release()
	}
	if got := scope.Usage(); got != 200 {
		t.Fatalf("已提交后 Release 不得撤销占用, got %d", got)
	}
	if got := st.State(); got != StateCommitted {
		t.Fatalf("State()=%v want committed", got)
	}
}

// TestSettlement_NilTargetFailsClosed 未绑定账本时 fail-closed（返回哨兵错误，不 panic）。
func TestSettlement_NilTargetFailsClosed(t *testing.T) {
	t.Parallel()
	st := NewSettlement(nil)
	if err := st.Reserve(1); !errors.Is(err, ErrSettlementNoTarget) {
		t.Fatalf("nil 账本 Reserve err=%v want ErrSettlementNoTarget", err)
	}
	if err := st.Reconcile(1); !errors.Is(err, ErrSettlementNoTarget) {
		t.Fatalf("nil 账本 Reconcile err=%v want ErrSettlementNoTarget", err)
	}
	if err := st.Reconcile(0); err != nil {
		t.Fatalf("nil 账本 Reconcile(0)=%v want nil（空操作）", err)
	}
	if err := st.Commit(); !errors.Is(err, ErrSettlementNoTarget) {
		t.Fatalf("nil 账本 Commit err=%v want ErrSettlementNoTarget", err)
	}
	st.Release()
	if got := st.State(); got != StateReleased {
		t.Fatalf("Release 后 State()=%v want released", got)
	}
	if err := st.Commit(); !errors.Is(err, ErrSettlementReleased) {
		t.Fatalf("Release 后 Commit err=%v want ErrSettlementReleased", err)
	}
}

// TestSettlement_NonPositiveAmountsClampToZero 负数按 0 处理（与 TryReserve/commit 的 nonNeg 口径一致）。
func TestSettlement_NonPositiveAmountsClampToZero(t *testing.T) {
	t.Parallel()
	_, scope, st := settlementNew(t, 1000)
	if err := st.Reserve(-10); err != nil {
		t.Fatalf("Reserve(-10): %v", err)
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("Reserved()=%d want 0（负数钳零）", got)
	}
	if err := st.Reconcile(-10); err != nil {
		t.Fatalf("Reconcile(-10): %v", err)
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("Reconcile 后 Reserved()=%d want 0", got)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("scope.Usage()=%d want 0", got)
	}
}

// TestSettlement_ConcurrentIndependentSettlements 并发独立结算（-race）：
// 每个 goroutine 各自 Reserve→Commit，账本总量必须等于各笔之和且无预留残留。
func TestSettlement_ConcurrentIndependentSettlements(t *testing.T) {
	t.Parallel()
	const (
		workers = 8
		each    = int64(100)
	)
	root, scope, _ := settlementNew(t, 1<<20)
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			st := NewSettlement(scope.Pool())
			if err := st.Reserve(each); err != nil {
				errs[i] = err
				return
			}
			errs[i] = st.Commit()
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}
	want := int64(workers) * each
	if got := scope.Usage(); got != want {
		t.Fatalf("scope.Usage()=%d want %d", got, want)
	}
	if got := root.Usage(); got != want {
		t.Fatalf("root.Usage()=%d want %d", got, want)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("root.Reserved()=%d want 0（无残留预留）", got)
	}
}

// TestSettlement_ConcurrentCommitOnSameHandle 同一句柄并发 Commit 只落账一次（-race）。
func TestSettlement_ConcurrentCommitOnSameHandle(t *testing.T) {
	t.Parallel()
	const (
		workers = 16
		amount  = int64(500)
	)
	root, scope, st := settlementNew(t, 1000)
	if err := st.Reserve(amount); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			errs[i] = st.Commit()
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d Commit: %v", i, err)
		}
	}
	if got := scope.Usage(); got != amount {
		t.Fatalf("并发 Commit 后 scope.Usage()=%d want %d（只落账一次）", got, amount)
	}
	if got := root.Usage(); got != amount {
		t.Fatalf("并发 Commit 后 root.Usage()=%d want %d", got, amount)
	}
	if got := st.Committed(); got != amount {
		t.Fatalf("Committed()=%d want %d", got, amount)
	}
}

// TestSettlement_ConcurrentCommitAndReleaseIsCoherent 同一句柄并发 Commit 与 Release（-race）：
// 无论谁先赢，终态与账本都必须自洽（已提交 ⇒ 占用在；已释放 ⇒ 预留言额已归还），且不 panic。
func TestSettlement_ConcurrentCommitAndReleaseIsCoherent(t *testing.T) {
	t.Parallel()
	const (
		workers = 16
		amount  = int64(500)
	)
	root, scope, st := settlementNew(t, 1000)
	if err := st.Reserve(amount); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := range workers {
		wg.Go(func() {
			if i%2 == 0 {
				errs[i] = st.Commit()
				return
			}
			st.Release()
		})
	}
	wg.Wait()
	releasedErrors := 0
	for i, err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrSettlementReleased):
			releasedErrors++
		default:
			t.Fatalf("goroutine %d 非预期错误: %v", i, err)
		}
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("终态 scope.Reserved()=%d want 0", got)
	}
	switch st.State() {
	case StateCommitted:
		if releasedErrors != 0 {
			t.Fatalf("终态 committed 时不得有 Commit 报 ErrSettlementReleased（got %d 次）", releasedErrors)
		}
		if got := root.Usage(); got != amount {
			t.Fatalf("committed 终态 root.Usage()=%d want %d", got, amount)
		}
	case StateReleased:
		if got := root.Usage(); got != 0 {
			t.Fatalf("released 终态 root.Usage()=%d want 0", got)
		}
		if got := st.Committed(); got != 0 {
			t.Fatalf("released 终态 Committed()=%d want 0", got)
		}
	default:
		t.Fatalf("终态 State()=%v 必须为 committed 或 released", st.State())
	}
}

// settlementOccupy 用另一个结算句柄把 scope 占满（Reserve → Commit）。
// 用于制造「配额临时不足」的前置：ReserveWait 必须排队等释放，而不是立即失败。
func settlementOccupy(t *testing.T, scope *Scope, n int64) {
	t.Helper()
	occ := NewSettlement(scope.Pool())
	if err := occ.Reserve(n); err != nil {
		t.Fatalf("预置占满 Reserve(%d): %v", n, err)
	}
	if err := occ.Commit(); err != nil {
		t.Fatalf("预置占满 Commit: %v", err)
	}
}

// settlementWaitForWaiting 轮询根池等待队列，直到出现等待者（或等待者已提前返回 ⇒ 直接报错）。
// 确定性同步基于 WaitStats().Waiting，不用固定 time.Sleep。
func settlementWaitForWaiting(t *testing.T, root *Pool, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if root.WaitStats().Waiting > 0 {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("等待者提前返回（应排队等待）: %v", err)
		default:
		}
		runtime.Gosched()
	}
	t.Fatalf("等待者未进入排队（Waiting=%d want >0）", root.WaitStats().Waiting)
}

// TestSettlement_ReserveWait_WakesOnRelease：「临时不足」必须排队等释放后成功，并把额度计入
// 本句柄预留（随后 Reconcile/Commit 与 Reserve 完全同语义）。
//
// 变异 ①（去掉排队、直接返回 ErrStorageFull）⇒ 本用例红（等待者提前返回）。
func TestSettlement_ReserveWait_WakesOnRelease(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	settlementOccupy(t, scope, 100)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- st.ReserveWait(ctx, 50, 5*time.Second)
	}()
	settlementWaitForWaiting(t, root, done)
	// 等待期间不得提前记账（预留只在成功返回后计入句柄）。
	if got := st.Reserved(); got != 0 {
		t.Fatalf("等待中 Reserved()=%d want 0", got)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("等待中 root.Reserved()=%d want 0", got)
	}

	scope.ReleaseUsage(100) // 释放 ⇒ 广播唤醒排队者
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("释放后应预留成功: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后未被唤醒（丢唤醒）")
	}
	// 成功：额度计入句柄预留，且排队已出队。
	if got := st.Reserved(); got != 50 {
		t.Fatalf("Reserved()=%d want 50", got)
	}
	if got := scope.Reserved(); got != 50 {
		t.Fatalf("scope.Reserved()=%d want 50", got)
	}
	if got := root.Reserved(); got != 50 {
		t.Fatalf("root.Reserved()=%d want 50（父链聚合）", got)
	}
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("成功后等待者应出队, Waiting=%d", got)
	}
	// 排队取到的额度归 Settlement 所有：可继续收敛并提交。
	if err := st.Reconcile(30); err != nil {
		t.Fatalf("Reconcile(30): %v", err)
	}
	if got := root.Reserved(); got != 30 {
		t.Fatalf("Reconcile 后 root.Reserved()=%d want 30", got)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := scope.Usage(); got != 30 {
		t.Fatalf("scope.Usage()=%d want 30", got)
	}
	if got := root.Usage(); got != 30 {
		t.Fatalf("root.Usage()=%d want 30", got)
	}
}

// TestSettlement_ReserveWait_TimeoutLeavesNothingBehind：超时失败必须「账本/状态/队列」全无残留，
// 且错误仍可归因为容量不足（调用方映射 507）。
//
// 变异 ②（超时后仍继续占用排队/账本）⇒ 末尾 Waiting/Reserved 断言红。
func TestSettlement_ReserveWait_TimeoutLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	settlementOccupy(t, scope, 100)

	const timeout = 50 * time.Millisecond
	// ctx 带有限期：即使实现退化为「超时后重新排队」，用例也会在 3s 内结束并红，不会挂死。
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	err := st.ReserveWait(ctx, 50, timeout)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("无人释放时超时应失败")
	}
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("错误应包装 ErrStorageFull（容量不足语义）: %v", err)
	}
	if !errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("错误应可归因「等待超时」: %v", err)
	}
	if elapsed < timeout {
		t.Fatalf("应在超时窗口后才失败，实际 %s（未真正排队）", elapsed)
	}
	// 失败不改变账本与状态。
	if got := st.Reserved(); got != 0 {
		t.Fatalf("超时后 Reserved()=%d want 0", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("超时后 scope.Reserved()=%d want 0", got)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("超时后 root.Reserved()=%d want 0", got)
	}
	if got := st.State(); got != StateOpen {
		t.Fatalf("超时后 State()=%v want open（句柄可继续使用）", got)
	}
	// 排队无残留：等待者必须已出队。
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("超时后等待者未出队, Waiting=%d", got)
	}
	// 句柄未被失败污染：释放后同一句柄可再次排队并成功。
	scope.ReleaseUsage(100)
	if err := st.ReserveWait(context.Background(), 50, timeout); err != nil {
		t.Fatalf("释放后同一句柄重新排队应成功: %v", err)
	}
	if got := st.Reserved(); got != 50 {
		t.Fatalf("重试成功后 Reserved()=%d want 50", got)
	}
}

// TestSettlement_ReserveWait_CancelLeavesNothingBehind：ctx 取消立即打断等待，同样无账本/队列残留。
func TestSettlement_ReserveWait_CancelLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	settlementOccupy(t, scope, 100)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- st.ReserveWait(ctx, 50, 10*time.Second) }()
	settlementWaitForWaiting(t, root, done)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("应返回 ctx 取消错误, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ctx 取消未打断等待")
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("取消后 Reserved()=%d want 0", got)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("取消后 root.Reserved()=%d want 0", got)
	}
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("取消后等待者未出队, Waiting=%d", got)
	}
}

// TestSettlement_ReserveWait_EnoughRoomSkipsQueue：有空间时不入队（与 WaitReserve 的「快速尝试」一致），
// 也就不可能被排队超时打扰。
func TestSettlement_ReserveWait_EnoughRoomSkipsQueue(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 1000)
	if err := st.ReserveWait(context.Background(), 100, time.Millisecond); err != nil {
		t.Fatalf("有空间应直接预留成功: %v", err)
	}
	if got := st.Reserved(); got != 100 {
		t.Fatalf("Reserved()=%d want 100", got)
	}
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("有空间时不得入队, Waiting=%d", got)
	}
	if got := root.WaitRounds(); got != 0 {
		t.Fatalf("有空间时不得入队（waitRounds=%d want 0）", got)
	}
	if err := st.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if got := scope.Usage(); got != 100 {
		t.Fatalf("scope.Usage()=%d want 100", got)
	}
}

// TestSettlement_ReserveWait_TooLargeFailsFast：即使全部释放也放不下 ⇒ 立即失败、不入队
// （排队等一个永远不会来的额度没有意义）。
func TestSettlement_ReserveWait_TooLargeFailsFast(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	start := time.Now()
	err := st.ReserveWait(context.Background(), 150, 5*time.Second)
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("err=%v want ErrStorageFull", err)
	}
	if errors.Is(err, ErrWaitTimeout) {
		t.Fatalf("不可能容纳的请求不应以「等待超时」收场: %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("不可能容纳应立即失败, 实际耗时 %s", elapsed)
	}
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("不得占用等待队列, Waiting=%d", got)
	}
	if got := st.Reserved(); got != 0 {
		t.Fatalf("失败后 Reserved()=%d want 0", got)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("失败后 scope.Reserved()=%d want 0", got)
	}
}

// TestSettlement_ReserveWait_TerminalOrNoTargetFailsWithoutQueueing：终态句柄与未绑定账本的
// 句柄必须立即返回哨兵错误（不排队、不改账本），零/负字节与 Reserve 一致地钳零直通。
func TestSettlement_ReserveWait_TerminalOrNoTargetFailsWithoutQueueing(t *testing.T) {
	t.Parallel()
	t.Run("committed", func(t *testing.T) {
		t.Parallel()
		root, _, st := settlementNew(t, 1000)
		if err := st.Reserve(10); err != nil {
			t.Fatalf("Reserve: %v", err)
		}
		if err := st.Commit(); err != nil {
			t.Fatalf("Commit: %v", err)
		}
		if err := st.ReserveWait(context.Background(), 10, time.Millisecond); !errors.Is(err, ErrSettlementCommitted) {
			t.Fatalf("err=%v want ErrSettlementCommitted", err)
		}
		if got := root.WaitRounds(); got != 0 {
			t.Fatalf("终态句柄不得入队, waitRounds=%d", got)
		}
		if got := st.Reserved(); got != 0 {
			t.Fatalf("Reserved()=%d want 0", got)
		}
	})
	t.Run("released", func(t *testing.T) {
		t.Parallel()
		root, _, st := settlementNew(t, 1000)
		st.Release()
		if err := st.ReserveWait(context.Background(), 10, time.Millisecond); !errors.Is(err, ErrSettlementReleased) {
			t.Fatalf("err=%v want ErrSettlementReleased", err)
		}
		if got := root.WaitRounds(); got != 0 {
			t.Fatalf("终态句柄不得入队, waitRounds=%d", got)
		}
	})
	t.Run("no target", func(t *testing.T) {
		t.Parallel()
		st := NewSettlement(nil)
		if err := st.ReserveWait(context.Background(), 10, time.Millisecond); !errors.Is(err, ErrSettlementNoTarget) {
			t.Fatalf("err=%v want ErrSettlementNoTarget", err)
		}
		// 与 Reserve 一致：零/负字节钳零后立即成功，无需账本。
		if err := st.ReserveWait(context.Background(), 0, time.Millisecond); err != nil {
			t.Fatalf("零字节应直通成功: %v", err)
		}
		if err := st.ReserveWait(context.Background(), -5, time.Millisecond); err != nil {
			t.Fatalf("负字节钳零应直通成功: %v", err)
		}
	})
}

// TestSettlement_ReserveWait_StateChangeDuringWaitLeavesNoLeak：等待期间句柄被并发 Release
// （终态）⇒ 排队取到的额度必须归还，不得滞留 reserved。
//
// 变异 ③（终态冲突路径不归还刚取到的额度）⇒ 末尾 root.Reserved() 断言红。
func TestSettlement_ReserveWait_StateChangeDuringWaitLeavesNoLeak(t *testing.T) {
	t.Parallel()
	root, scope, st := settlementNew(t, 100)
	settlementOccupy(t, scope, 100)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		done <- st.ReserveWait(ctx, 50, 5*time.Second)
	}()
	settlementWaitForWaiting(t, root, done)
	st.Release() // 等待期间句柄进入终态
	scope.ReleaseUsage(100)
	select {
	case err := <-done:
		if !errors.Is(err, ErrSettlementReleased) {
			t.Fatalf("终态冲突应返回 ErrSettlementReleased, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后未被唤醒")
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("终态冲突后 scope.Reserved()=%d want 0（不得泄漏）", got)
	}
	if got := root.Reserved(); got != 0 {
		t.Fatalf("终态冲突后 root.Reserved()=%d want 0（不得泄漏）", got)
	}
	if got := root.WaitStats().Waiting; got != 0 {
		t.Fatalf("等待者应出队, Waiting=%d", got)
	}
	if got := st.State(); got != StateReleased {
		t.Fatalf("State()=%v want released", got)
	}
}
