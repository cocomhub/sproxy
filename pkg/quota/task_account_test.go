// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"testing"
)

// TestTaskAccount_Lifecycle 创建→CommitUp→Release 全周期账本正确（含补留/占位）。
func TestTaskAccount_Lifecycle(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)

	// 占位：estimate<=0 → 1 GiB。
	acc, err := NewTaskAccount(sc, 0)
	if err != nil {
		t.Fatalf("NewTaskAccount(0): %v", err)
	}
	if acc.Reserved() != placeholderReserve || acc.Committed() != 0 {
		t.Fatalf("占位后 reserved=%d committed=%d", acc.Reserved(), acc.Committed())
	}
	// CommitUp 划转 reserved→committed。
	if err := acc.CommitUp(10); err != nil {
		t.Fatalf("CommitUp(10): %v", err)
	}
	if acc.Committed() != 10 || acc.Reserved() != placeholderReserve-10 {
		t.Fatalf("CommitUp 后 committed=%d reserved=%d", acc.Committed(), acc.Reserved())
	}
	// Release 释放全部（committed + reserved）。
	acc.Release()
	if sc.Usage() != 0 || sc.Reserved() != 0 {
		t.Fatalf("Release 后 Scope usage=%d reserved=%d want 0", sc.Usage(), sc.Reserved())
	}
}

// TestTaskAccount_CommitUp_ReserveTopup 写超预留自动补留。
func TestTaskAccount_CommitUp_ReserveTopup(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)
	acc, err := NewTaskAccount(sc, 100)
	if err != nil {
		t.Fatalf("NewTaskAccount(100): %v", err)
	}
	// 补留 200 字节（超预留）。
	if err := acc.CommitUp(200); err != nil {
		t.Fatalf("CommitUp(200): %v", err)
	}
	if acc.Committed() != 200 || acc.Reserved() != 0 {
		t.Fatalf("补留后 committed=%d reserved=%d", acc.Committed(), acc.Reserved())
	}
	acc.Release()
}

// TestTaskAccount_Release_Idempotent Release 幂等（多次调用/与写盘并发归零）。
func TestTaskAccount_Release_Idempotent(t *testing.T) {
	t.Parallel()
	pool := NewPool(0)
	sc := pool.newScope("cloud", 0)
	acc, err := NewTaskAccount(sc, 50)
	if err != nil {
		t.Fatalf("NewTaskAccount: %v", err)
	}
	_ = acc.CommitUp(20)
	acc.Release()
	acc.Release() // 幂等
	acc.ReleaseReserve()
	if sc.Usage() != 0 || sc.Reserved() != 0 {
		t.Fatalf("幂等后 Scope usage=%d reserved=%d want 0", sc.Usage(), sc.Reserved())
	}
}

// TestTaskAccount_CommitUp_ErrStorageFull 补留失败不改状态。
func TestTaskAccount_CommitUp_ErrStorageFull(t *testing.T) {
	t.Parallel()
	pool := NewPool(100) // 上限 100 字节
	sc := pool.newScope("cloud", 100)
	acc, err := NewTaskAccount(sc, 50)
	if err != nil {
		t.Fatalf("NewTaskAccount: %v", err)
	}
	if err := acc.CommitUp(200); err == nil {
		t.Fatal("补留超限应返回 ErrStorageFull")
	}
	// 状态不变：仍 50 预留、0 committed。
	if acc.Reserved() != 50 || acc.Committed() != 0 {
		t.Fatalf("补留失败后 reserved=%d committed=%d want 50/0", acc.Reserved(), acc.Committed())
	}
	acc.Release()
}
