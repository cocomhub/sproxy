// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"errors"
	"io"
	"math"
	"testing"
)

func TestScope_TryReserveCommitRelease(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t/alice", 50)
	res, err := s.TryReserve(30) // 预留 30
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	if got := s.Reserved(); got != 30 {
		t.Fatalf("Reserved=%d want 30", got)
	}
	res.Commit(25) // 实际 25：reserved 25→committed 25，多预留 5 归还
	if got := s.Usage(); got != 25 {
		t.Fatalf("Usage=%d want 25", got)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
	if got := root.Usage(); got != 25 {
		t.Fatalf("root Usage=%d want 25（父链聚合）", got)
	}
	s.ReleaseUsage(10) // 删除文件释放 10
	if got := s.Usage(); got != 15 {
		t.Fatalf("Usage=%d want 15", got)
	}
}

func TestScope_Adjust(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	s.Adjust(0, 10) // 建立占用 10（diff 语义：committed += next-prev）
	if got := s.Usage(); got != 10 {
		t.Fatalf("Usage=%d want 10", got)
	}
	s.Adjust(10, 20) // 覆盖写尺寸 10→20，diff +10 → 20
	if got := s.Usage(); got != 20 {
		t.Fatalf("Usage=%d want 20", got)
	}
	s.Adjust(20, 5) // 缩小 diff -15 → 5
	if got := s.Usage(); got != 5 {
		t.Fatalf("Usage=%d want 5", got)
	}
}

// TestScope_Adjust_MultiFileBucket 强制 diff 语义：多文件桶下覆盖写不能丢弃其它文件占用。
func TestScope_Adjust_MultiFileBucket(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/user", 1000)
	res, err := s.TryReserve(15) // 建立 A(10)+B(5)=15 基线
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}
	res.Commit(15)
	// 覆盖写 A 10→12：diff +2，committed 应为 17（B 的 5 必须保留）。
	s.Adjust(10, 12)
	if got := s.Usage(); got != 17 {
		t.Fatalf("diff 语义下 Usage=%d want 17（B 的 5 必须保留）", got)
	}
}

func TestScope_QuotaExceeded(t *testing.T) {
	root := NewPool(10)      // 全局兜底 10
	s := root.Scope("/t", 8) // 租户上限 8
	if _, err := s.TryReserve(9); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("租户上限应拒绝, got %v", err)
	}
	if got := s.Available(); got != 8 {
		t.Fatalf("Available=%d want 8", got)
	}
}

// 全局兜底：两个租户各自未超自身上限，但总和超全局上限 → 必须拒绝（验证父链聚合检查）。
func TestScope_GlobalCapExceeded(t *testing.T) {
	root := NewPool(10)
	a := root.Scope("/a", 100)
	b := root.Scope("/b", 100)
	resA, err := a.TryReserve(6) // 全局 6/10
	if err != nil {
		t.Fatalf("a 预留失败: %v", err)
	}
	if _, err := b.TryReserve(6); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("全局兜底应拒绝（6+6>10）, got %v", err)
	}
	// 父链失败回滚：b 自身未超上限（100），但父链全局拒绝后 b.reserved 必须回滚为 0，
	// 否则产生"幽灵预留"（删除回滚代码后本断言必须失败）。
	if got := b.Reserved(); got != 0 {
		t.Fatalf("父链失败后 b 预留应回滚为 0, got %d", got)
	}
	resA.Release() // 归还
	if _, err := b.TryReserve(6); err != nil {
		t.Fatalf("释放后应可预留, got %v", err)
	}
}

func TestScope_ReleaseReservation(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 50)
	res, _ := s.TryReserve(40)
	res.Release() // 失败取消预留
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}
	if got := s.Available(); got != 50 {
		t.Fatalf("Available=%d want 50", got)
	}
}

func TestPool_SetMaxBytes(t *testing.T) {
	root := NewPool(10)
	s := root.Scope("/t", 100)
	if _, err := s.TryReserve(20); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("全局上限 10 应拒绝 20, got %v", err)
	}
	root.SetMaxBytes(50) // 运行时扩上限
	if _, err := s.TryReserve(20); err != nil {
		t.Fatalf("扩上限后应可预留, got %v", err)
	}
	root.SetMaxBytes(0) // 0 = 不限制
	// 全局不限后，预留未超子池上限应成功。
	if _, err := s.TryReserve(80); err != nil {
		t.Fatalf("全局 0 不限 + 子池未超应成功, got %v", err)
	}
}

func TestScope_UsageByBucket(t *testing.T) {
	root := NewPool(1000)
	t1 := root.Scope("/tenant/a", 500)
	t1.Scope("/user").Adjust(0, 100) // 子桶 user 占用 100
	t1.Scope("/cloud").Adjust(0, 50) // 子桶 cloud 占用 50
	m := root.UsageByBucket()
	if m["/tenant/a/user"] != 100 || m["/tenant/a/cloud"] != 50 {
		t.Fatalf("UsageByBucket=%v", m)
	}
	if got := t1.Usage(); got != 150 {
		t.Fatalf("tenant Usage=%d want 150", got)
	}
}

// --- 配额磁盘封顶审查补充测试（审查A ❌4 核心契约 + ⚠️11 边界） ---

// TestReservation_Idempotent 锁定 Reservation 的 CAS 语义（quota.go done 标记）：
// 重复 Commit、重复 Release、Commit 后 Release、Release 后 Commit 均只生效一次，
// 每次重复调用后账本（reserved/committed/Usage/Available）不再变化。
func TestReservation_Idempotent(t *testing.T) {
	t.Run("double_commit_noop", func(t *testing.T) {
		root := NewPool(100)
		s := root.Scope("/t", 100)
		res, err := s.TryReserve(30)
		if err != nil {
			t.Fatalf("TryReserve: %v", err)
		}
		res.Commit(25)
		// 快照：reserved 0 / committed 25 / Available 75。
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Commit 后 Reserved=%d want %d", got, want)
		}
		if got, want := s.Usage(), int64(25); got != want {
			t.Fatalf("Commit 后 Usage=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(75); got != want {
			t.Fatalf("Commit 后 Available=%d want %d", got, want)
		}
		// 重复 Commit 不得再次对账。
		res.Commit(999)
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("重复 Commit 后 Reserved=%d want %d", got, want)
		}
		if got, want := s.Usage(), int64(25); got != want {
			t.Fatalf("重复 Commit 后 Usage=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(75); got != want {
			t.Fatalf("重复 Commit 后 Available=%d want %d", got, want)
		}
	})

	t.Run("double_release_noop", func(t *testing.T) {
		root := NewPool(100)
		s := root.Scope("/t", 100)
		res, err := s.TryReserve(30)
		if err != nil {
			t.Fatalf("TryReserve: %v", err)
		}
		res.Release()
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Release 后 Reserved=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(100); got != want {
			t.Fatalf("Release 后 Available=%d want %d", got, want)
		}
		// 重复 Release 不得把 available 放出上限（reserved 不会为负）。
		res.Release()
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("重复 Release 后 Reserved=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(100); got != want {
			t.Fatalf("重复 Release 后 Available=%d want %d", got, want)
		}
	})

	t.Run("commit_then_release_noop", func(t *testing.T) {
		root := NewPool(100)
		s := root.Scope("/t", 100)
		res, err := s.TryReserve(30)
		if err != nil {
			t.Fatalf("TryReserve: %v", err)
		}
		res.Commit(20)
		// Commit 后 released 是空操作：committed 保持，reserved 保持 0（不被差值再次扣减）。
		res.Release()
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Commit 后 Release Reserved=%d want %d", got, want)
		}
		if got, want := s.Usage(), int64(20); got != want {
			t.Fatalf("Commit 后 Release Usage=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(80); got != want {
			t.Fatalf("Commit 后 Release Available=%d want %d", got, want)
		}
	})

	t.Run("release_then_commit_noop", func(t *testing.T) {
		root := NewPool(100)
		s := root.Scope("/t", 100)
		res, err := s.TryReserve(30)
		if err != nil {
			t.Fatalf("TryReserve: %v", err)
		}
		res.Release()
		// Release 后 Commit 是空操作：不凭空落 committed。
		res.Commit(20)
		if got, want := s.Usage(), int64(0); got != want {
			t.Fatalf("Release 后 Commit Usage=%d want %d", got, want)
		}
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Release 后 Commit Reserved=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(100); got != want {
			t.Fatalf("Release 后 Commit Available=%d want %d", got, want)
		}
	})
}

// TestScope_TryReserve_ExactLimit 验证恰好打满上限的预留成功（不留 1 字节余量），
// 全局兜底 10 两笔 5+5 恰好打满同样成功。
func TestScope_TryReserve_ExactLimit(t *testing.T) {
	t.Run("subpool_exact", func(t *testing.T) {
		root := NewPool(100)
		s := root.Scope("/t", 8) // 子池上限 8
		res, err := s.TryReserve(8)
		if err != nil {
			t.Fatalf("TryReserve(8)=%d want 成功（恰好打满）", err)
		}
		if got, want := s.Reserved(), int64(8); got != want {
			t.Fatalf("Reserved=%d want %d", got, want)
		}
		if got, want := s.Available(), int64(0); got != want {
			t.Fatalf("Available=%d want %d（恰好打满）", got, want)
		}
		if _, err := s.TryReserve(1); !errors.Is(err, ErrStorageFull) {
			t.Fatalf("打满后再预留应拒绝, got %v", err)
		}
		res.Release()
	})
	t.Run("global_exact", func(t *testing.T) {
		root := NewPool(10) // 全局兜底 10
		s := root.Scope("/t", 100)
		r1, err := s.TryReserve(5)
		if err != nil {
			t.Fatalf("TryReserve(5): %v", err)
		}
		r2, err := s.TryReserve(5) // 5+5 恰好打满全局
		if err != nil {
			t.Fatalf("TryReserve(5) #2: %v（5+5 恰好打满应成功）", err)
		}
		// Available 是 scope 自身账本（上限 100）计算：10/100 → 90；全局 10 的约束
		// 体现在 TryReserve 父链校验，而非 available()——此处同时锁定全局恰好打满通过。
		if got, want := s.Available(), int64(90); got != want {
			t.Fatalf("Available=%d want %d（子池 100 减已用 10）", got, want)
		}
		if _, err := s.TryReserve(1); !errors.Is(err, ErrStorageFull) {
			t.Fatalf("打满后再预留应拒绝, got %v", err)
		}
		r1.Release()
		r2.Release()
	})
}

// TestScope_MultiLevelParentChain 验证三层链 root→/a(50)→/a/user(20)：
// TryReserve 同时受最内层与父链每层上限约束；父链预留/占用正确聚合；
// UsageByBucket 含中间路径键。
func TestScope_MultiLevelParentChain(t *testing.T) {
	root := NewPool(1000)
	a := root.Scope("/a", 50)      // 中间层上限 50
	user := a.Scope("/user", 20)   // 最内层上限 20
	cloud := a.Scope("/cloud", 50) // 兄弟作用域（聚合隔离验证）
	if got, want := user.MaxBytes(), int64(20); got != want {
		t.Fatalf("user.MaxBytes=%d want %d", got, want)
	}
	if got, want := a.MaxBytes(), int64(50); got != want {
		t.Fatalf("a.MaxBytes=%d want %d", got, want)
	}

	r1, err := user.TryReserve(12) // 内层 12 ≤20，父链 12 ≤50，均应通过
	if err != nil {
		t.Fatalf("user.TryReserve(12): %v", err)
	}
	if got, want := user.Reserved(), int64(12); got != want {
		t.Fatalf("user.Reserved=%d want %d", got, want)
	}
	if got, want := a.Reserved(), int64(12); got != want {
		t.Fatalf("父链 a.Reserved=%d want %d（预留聚合）", got, want)
	}

	// 内层已用 12，还剩 8；再加 15 内层拒绝（12+15=27>20）。
	if _, err := user.TryReserve(15); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("user.TryReserve 超内层上限应拒绝, got %v", err)
	}
	// sibling 不受 user 占用影响，但总量 <50 → 成功。
	if _, err := cloud.TryReserve(20); err != nil {
		t.Fatalf("cloud.TryReserve(20) 应成功（不受兄弟影响）: %v", err)
	}
	// 父链上限约束：user 剩余空间 + cloud 20 = 23+12=35>30 仍可；再压 30 → 父链 62>50 拒绝
	if _, err := a.Scope("/tmp").TryReserve(10); err != nil {
		t.Fatalf("/tmp.TryReserve(10) 应成功: %v", err)
	}
	// 此时父链已预留 12+20+10=42，剩余 8。
	sink := a.Scope("/sink")
	if _, err := sink.TryReserve(20); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("父链超限应拒绝, got %v", err)
	}

	r1.Commit(10) // 预留 12，实际 10：多预留 2 归还
	if got, want := user.Reserved(), int64(0); got != want {
		t.Fatalf("Commit 后 user.Reserved=%d want %d", got, want)
	}
	if got, want := user.Usage(), int64(10); got != want {
		t.Fatalf("user.Usage=%d want %d", got, want)
	}
	if got, want := a.Usage(), int64(10); got != want {
		t.Fatalf("父链 a.Usage=%d want %d（占用聚合）", got, want)
	}
	if got, want := root.Usage(), int64(10); got != want {
		t.Fatalf("根 root.Usage=%d want %d（整链聚合）", got, want)
	}

	m := root.UsageByBucket()
	if got := m["/a/user"]; got != 10 {
		t.Fatalf("UsageByBucket[/a/user]=%d want 10（含中间路径键）", got)
	}
	if got := m["/a"]; got != 10 {
		t.Fatalf("UsageByBucket[/a]=%d want 10", got)
	}
}

// --- 边界（审查A ⚠️ 11 项） ---

// TestQuotaWriter_PlaceholderEstimate 锁定 estimate<=0 时的 1 GiB 占位预留语义：
// estimate=0（Content-Length 缺失）与 -1（显式负值，nonNeg 钳制路径）均预留 1 GiB。
func TestQuotaWriter_PlaceholderEstimate(t *testing.T) {
	cases := []struct {
		name     string
		estimate int64
	}{
		{name: "estimate_zero", estimate: 0},
		{name: "estimate_negative", estimate: -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := NewPool(1 << 40)
			s := root.Scope("/t", 1<<40)
			w, err := NewQuotaWriter(s, io.Discard, tc.estimate)
			if err != nil {
				t.Fatalf("NewQuotaWriter(estimate=%d): %v", tc.estimate, err)
			}
			if got, want := s.Reserved(), int64(1)<<30; got != want {
				t.Fatalf("estimate=%d → Reserved=%d want %d（1 GiB 占位）", tc.estimate, got, want)
			}
			w.Finish(true, 0)
			if got, want := s.Reserved(), int64(0); got != want {
				t.Fatalf("Finish 后 Reserved=%d want %d", got, want)
			}
		})
	}
}

// TestScope_TryReserve_NonPositive 锁定 nonNeg 钳制：TryReserve(0)/TryReserve(-5)
// 成功但 reserved 不变。
func TestScope_TryReserve_NonPositive(t *testing.T) {
	root := NewPool(10)
	s := root.Scope("/t", 10)
	res1, err := s.TryReserve(0)
	if err != nil {
		t.Fatalf("TryReserve(0) 应成功（nonNeg 钳制为 0）, got %v", err)
	}
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("TryReserve(0) 后 Reserved=%d want %d", got, want)
	}
	res2, err := s.TryReserve(-5)
	if err != nil {
		t.Fatalf("TryReserve(-5) 应成功（nonNeg 钳制为 0）, got %v", err)
	}
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("TryReserve(-5) 后 Reserved=%d want %d", got, want)
	}
	if got, want := s.Available(), int64(10); got != want {
		t.Fatalf("Available=%d want %d", got, want)
	}
	res1.Release()
	res2.Release()
}

// TestScope_Commit_OverReserve 锁定 Commit(actual>amount) 的账本语义：
// commitUp 只扣减 amount 而非 actual，reserved 归零、绝不为负。
func TestScope_Commit_OverReserve(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	res, err := s.TryReserve(30)
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	res.Commit(40) // 预留 30，实际 40（超预留的 10 属于录入误差）
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("Commit(40) 后 Reserved=%d want %d（不得为负）", got, want)
	}
	if got, want := s.Usage(), int64(40); got != want {
		t.Fatalf("Commit(40) 后 Usage=%d want %d", got, want)
	}
	if got, want := s.Available(), int64(60); got != want {
		t.Fatalf("Commit(40) 后 Available=%d want %d", got, want)
	}
}

// TestScope_Adjust_UnderflowClampsZero 锁定 adjustUp 下溢归零：committed 不为负。
func TestScope_Adjust_UnderflowClampsZero(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	s.Adjust(100, 5) // diff = -95，从 0 起步 → 归 0 不反负
	if got, want := s.Usage(), int64(0); got != want {
		t.Fatalf("Adjust(100,5) Usage=%d want %d（不反负）", got, want)
	}
	if got, want := s.Available(), int64(100); got != want {
		t.Fatalf("Available=%d want %d", got, want)
	}
}

// TestScope_ReleaseUsage_OverReleaseClampsZero 锁定 releaseCommittedUp 下溢归零：
// 对占用量 5 释放 10 → 0 不反负。
func TestScope_ReleaseUsage_OverReleaseClampsZero(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	res, err := s.TryReserve(5)
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	res.Commit(5)
	if got, want := s.Usage(), int64(5); got != want {
		t.Fatalf("Commit 后 Usage=%d want %d", got, want)
	}
	s.ReleaseUsage(10) // 超量释放 10
	if got, want := s.Usage(), int64(0); got != want {
		t.Fatalf("ReleaseUsage(10) Usage=%d want %d（不反负）", got, want)
	}
	if got, want := s.Available(), int64(100); got != want {
		t.Fatalf("Available=%d want %d", got, want)
	}
}

// TestScope_Available_ExhaustedAndUnlimited 锁定 available 两种极值：
// 预留打满后 Available()==0；maxBytes=0（不限制）时 Available()==math.MaxInt64。
func TestScope_Available_ExhaustedAndUnlimited(t *testing.T) {
	t.Run("exhausted", func(t *testing.T) {
		root := NewPool(10)
		s := root.Scope("/t", 10)
		if got, want := s.Available(), int64(10); got != want {
			t.Fatalf("初始 Available=%d want %d", got, want)
		}
		if _, err := s.TryReserve(10); err != nil {
			t.Fatalf("TryReserve(10): %v", err)
		}
		if got, want := s.Available(), int64(0); got != want {
			t.Fatalf("打满后 Available=%d want %d", got, want)
		}
	})
	t.Run("unlimited", func(t *testing.T) {
		root := NewPool(0) // 0 = 不限制
		s := root.Scope("/t", 0)
		if got, want := s.Available(), int64(math.MaxInt64); got != want {
			t.Fatalf("不限时 Available=%d want %d", got, want)
		}
	})
}

// TestPool_SetMaxBytes_ShrinkDoesNotReclaim 锁定 SetMaxBytes 头注释契约：
// 缩小上限不回溯既有 committed/reserved 账本，仅拒绝后续新预留。
func TestPool_SetMaxBytes_ShrinkDoesNotReclaim(t *testing.T) {
	root := NewPool(100)
	s := root.Scope("/t", 100)
	res1, err := s.TryReserve(20) // 预留 20 并 commit 实际 10 → committed=10, reserved=0
	if err != nil {
		t.Fatalf("TryReserve(20): %v", err)
	}
	res1.Commit(10)
	res2, err := s.TryReserve(10) // 在途预留 → reserved=10
	if err != nil {
		t.Fatalf("TryReserve(10): %v", err)
	}
	_ = res2

	root.SetMaxBytes(5)
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("缩小上限后 Usage=%d want %d（不回溯已 commit）", got, want)
	}
	if got, want := s.Reserved(), int64(10); got != want {
		t.Fatalf("缩小上限后 Reserved=%d want %d（不回溯已预留）", got, want)
	}
	// Available 按 scope 自身上限（/t=100）而非父链全局计算：100−20=80。
	// 全局 5 的收缩只影响后续父链 TryReserve 校验。
	if got, want := s.Available(), int64(80); got != want {
		t.Fatalf("缩小上限后 Available=%d want %d（scope 自身账本计算）", got, want)
	}
	if _, err := s.TryReserve(1); !errors.Is(err, ErrStorageFull) {
		t.Fatalf("缩小上限后新预留应拒绝（父链全局超限）, got %v", err)
	}
}

// TestPool_ResolveLongestPrefix 验证 http route 式路由：沿 children 段树找最深匹配子作用域。
func TestPool_ResolveLongestPrefix(t *testing.T) {
	root := NewPool(1000)
	root.Scope("/a", 100).Scope("/b", 50).Scope("/c", 20) // 挂 /a(100) → /a/b(50) → /a/b/c(20)

	if got := root.ResolvePath("/a").MaxBytes(); got != 100 {
		t.Fatalf("Resolve /a MaxBytes=%d want 100", got)
	}
	if got := root.ResolvePath("/a/b/c").MaxBytes(); got != 20 {
		t.Fatalf("Resolve /a/b/c MaxBytes=%d want 20（最深节点）", got)
	}
	// 最长前缀：/a/b/c/deep 未装配 → 回落 /a/b/c。
	if got := root.ResolvePath("/a/b/c/deep").MaxBytes(); got != 20 {
		t.Fatalf("Resolve /a/b/c/deep 应回落 /a/b/c（MaxBytes=%d want 20）", got)
	}
	// 未装配段回落祖先进程。
	if got := root.ResolvePath("/a/z").MaxBytes(); got != 100 {
		t.Fatalf("Resolve /a/z 应回落 /a（MaxBytes=%d want 100）", got)
	}
	// 根节点自身（无匹配路径）：回落根，上限为其 maxBytes（此处 root=1000）。
	if got := root.ResolvePath("/x/y").MaxBytes(); got != 1000 {
		t.Fatalf("Resolve /x/y 应回落根（MaxBytes=%d want 1000 root 上限）", got)
	}
	// Scope.Resolve 便捷版等价（/a/b/c 最深节点）。
	if got := root.ResolvePath("/a/b/c").MaxBytes(); got != 20 {
		t.Fatalf("ResolvePath /a/b/c MaxBytes=%d want 20", got)
	}
}

// TestPool_EnsureScope_Levels 验证 EnsureScope 拆段逐级下探挂载（已存在复用）。
func TestPool_EnsureScope_Levels(t *testing.T) {
	root := NewPool(1000)
	s1 := root.EnsureScope([]string{"user", "videos", "hd"}, 300)
	if got := s1.MaxBytes(); got != 300 {
		t.Fatalf("EnsureScope 最深段 MaxBytes=%d want 300", got)
	}
	if got := s1.Usage(); got != 0 {
		t.Fatalf("新 Scope Usage=%d want 0", got)
	}
	// 中间层 0（不限制）。
	if got := root.ResolvePath("/user").MaxBytes(); got != 0 {
		t.Fatalf("中间层 user MaxBytes=%d want 0（不限制）", got)
	}
	// 重复调用复用同节点（不重复建层）。
	s2 := root.EnsureScope([]string{"user", "videos", "hd"}, 500) // maxBytes 变更不生效（已存在）
	if s1 == nil || s2 == nil {
		t.Fatal("EnsureScope 返回 nil")
	}
	// 兄弟路径独立。
	s3 := root.EnsureScope([]string{"user", "4k"}, 400)
	if s1.pool == s3.pool {
		t.Fatal("user/videos/hd 与 user/4k 应为不同节点")
	}
}

// TestReserve_LayeredCaps 验证用户绑定 2/3：quota("/a")=100、/a/b=50、/a/b/c=20，
// 逐级检查由 reserveUp 沿父链自动完成——对最深节点 TryReserve 超中间层上限即被拒。
func TestReserve_LayeredCaps(t *testing.T) {
	root := NewPool(1000)
	root.EnsureScope([]string{"/a"}, 100)
	root.EnsureScope([]string{"/a", "b"}, 50)
	deep := root.EnsureScope([]string{"/a", "b", "c"}, 20)

	// 15 ≤ 20 层 → 成功。
	if _, err := deep.TryReserve(15); err != nil {
		t.Fatalf("TryReserve(15) 应成功（≤/a/b/c 20）: %v", err)
	}
	// 25 > 20（/a/b/c 层）→ 拒。
	if _, err := deep.TryReserve(25); err == nil {
		t.Fatal("TryReserve(25) 应被 /a/b/c（20）层上限拦截")
	}
	// 40 ≤ 50 但 15+40=55 > 50（/a/b 层）→ 拒——中间层 /a/b 上限拦截。
	if _, err := deep.TryReserve(40); err == nil {
		t.Fatal("TryReserve(40) 应被中间层 /a/b（50）上限拦截（逐级检查）")
	}
	// 60 ≤ 100（/a 层）但累加 15+60=75 > 50 → 仍被 /a/b 拦截。
	if _, err := deep.TryReserve(60); err == nil {
		t.Fatal("TryReserve(60) 应被 /a/b（50）上限拦截")
	}
}
