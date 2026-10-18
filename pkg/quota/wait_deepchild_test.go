// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

// wait_deepchild_test.go 钉住「**深层子节点的释放必须唤醒根队列上的等待者**」
// （独立复审 P2「丢唤醒」根因修复的行为面；结构性保证见门禁 R34）。
//
// 与既有 `TestWaitReserve_CrossLevelWake` 互补：那条覆盖「等待者挂子节点、失败点在父节点」；
// 本条覆盖**相反方向**——等待者挂**根**、释放来自**深层子节点**（正是丢唤醒会命中的方向：
// 队列在根、释放发生在子层）。
//
// 根因：等待侧在 root.cond.L（= root.waitMu）下 double-check + Wait，而释放侧此前在**不同锁**下
// 广播 ⇒ 广播可能落在「已判定不足 → 尚未 Wait」窗口内被丢弃 ⇒ 等待者睡到超时才重试。

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestWaitReserve_WokenByDeepChildRelease(t *testing.T) {
	t.Parallel()
	root := NewPool(100)
	mid := root.Scope("a", 0)
	leaf := mid.Pool().Scope("b", 0)

	// 深层子节点占满根额度（走 committed ⇒ 沿父链可见）。
	res, err := leaf.TryReserve(100)
	if err != nil {
		t.Fatalf("leaf.TryReserve: %v", err)
	}
	res.Commit(100)

	// 等待者挂在**根**上，超时给 30s（若被正确唤醒应毫秒级返回）。
	done := make(chan error, 1)
	go func() {
		_, werr := root.WaitReserve(context.Background(), 50, 30*time.Second)
		done <- werr
	}()

	// **确定性同步**：轮询根节点 waitRounds 直到等待者确实进入等待（不依赖 time.Sleep）。
	enter := time.Now().Add(3 * time.Second)
	for time.Now().Before(enter) {
		if root.WaitRounds() >= 1 {
			break
		}
		select {
		case werr := <-done:
			t.Fatalf("等待者提前返回（应排队等待）: %v", werr)
		default:
		}
		runtime.Gosched()
	}
	if root.WaitRounds() < 1 {
		t.Fatal("前置失败：等待者未进入等待")
	}

	// 从**深层子节点**释放 ⇒ 必须唤醒根队列上的等待者（而非等 30s 超时）。
	start := time.Now()
	leaf.ReleaseUsage(100)
	select {
	case werr := <-done:
		if werr != nil {
			t.Fatalf("释放后等待者应能成功预留，实际 %v", werr)
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("唤醒耗时 %v（应毫秒级；未修必红：释放侧未持根等待锁 ⇒ 丢唤醒 ⇒ 睡到超时）", el)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("等待者未被唤醒（释放侧广播丢失）")
	}
}
