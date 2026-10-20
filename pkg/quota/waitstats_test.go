// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// waitstats_test.go 锁定 S4「等待可观测」的取数口径：
//   - nil 池 / 未配置队列 ⇒ 零值（不 panic）；
//   - 根池的统计来自其 waitq 队列（**行为验证**：超容量的请求立即被拒并计入 TooLarge，
//     说明 WaitStats 读的确实是同一条队列，而不是另建的一条）；
//   - 子作用域请���继承根池的统计（排队能力只有根池一份）。

import (
	"context"
	"testing"
	"time"
)

// TestPool_WaitStats 读取等待队列统计（S4）。
func TestPool_WaitStats(t *testing.T) {
	t.Parallel()
	var nilPool *Pool
	if got := nilPool.WaitStats(); got.Waiting != 0 || got.Acquired != 0 || got.TooLarge != 0 {
		t.Fatalf("nil 池应返回零值, got %+v", got)
	}

	root := NewPool(1 << 20) // 1 MiB 容量
	if got := root.WaitStats(); got.Waiting != 0 || got.MaxWaiting != 0 {
		t.Fatalf("新池应无等待, got %+v", got)
	}

	// 超过容量 ⇒ **立即失败**（不排队）⇒ waitq 的 TooLarge 计数递增。
	// 这条断言的意义：证明 WaitStats() 读的是「真正被使用的那条队列」。
	if _, err := root.WaitReserve(context.Background(), 4<<20, 20*time.Millisecond); err == nil {
		t.Fatal("超容量请求应立即失败（ErrTooLarge 语义），不应排队成功")
	}
	if got := root.WaitStats(); got.TooLarge == 0 {
		t.Fatalf("超容量拒绝应计入 TooLarge（等待可观测接线错误）, got %+v", got)
	}

	// 子作用域的统计与根池同源（排队能力只有根池一份）。
	child := root.Scope("/tenant/alice", 1<<20)
	if child == nil {
		t.Fatal("Scope 不应为 nil")
	}
	cp := child.Pool()
	if cp == nil {
		t.Fatal("作用域应挂有池")
	}
	if cw := cp.WaitStats(); cw.TooLarge != root.WaitStats().TooLarge {
		t.Fatalf("子池统计应与根池一致（同一条队列）: child=%+v root=%+v", cw, root.WaitStats())
	}
}
