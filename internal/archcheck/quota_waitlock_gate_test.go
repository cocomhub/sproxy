// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// quota_waitlock_gate_test.go 是**门禁 R34**：释放/放宽可用额度的公开入口必须在根等待锁
// （`Pool.withWaitLock`）内执行 —— 「丢唤醒」的结构性修复（独立复审 P2，2026-10-18）。
//
// 为何用结构门禁而非运行时用例：该竞态的交错窗口过窄（变异验证显示恢复旧实现后并发用例仍绿）
// ⇒ 运行时用例**无判别力**、会制造假信心。结构门禁是确定性的（同 R31/R32/R33 纪律）。

import (
	"os"
	"strings"
	"testing"
)

func TestQuotaReleasePathsHoldWaitLock(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("quota_waitlock.tsv")
	if err != nil {
		t.Fatalf("读取登记表失败: %v", err)
	}
	src, err := os.ReadFile("../../pkg/quota/quota.go")
	if err != nil {
		t.Fatalf("读取 pkg/quota/quota.go 失败: %v", err)
	}
	alloc, err := os.ReadFile("../../pkg/quota/allocation.go")
	if err != nil {
		t.Fatalf("读取 pkg/quota/allocation.go 失败: %v", err)
	}
	files := []string{string(src), string(alloc)}

	checked := 0
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		sig := parts[0]
		reason := ""
		if len(parts) > 1 {
			reason = parts[1]
		}
		body, ok := methodBody(files, sig)
		if !ok {
			t.Fatalf("登记表条目在源码中找不到：%q（理由：%s）", sig, reason)
		}
		if !strings.Contains(body, "withWaitLock(") {
			t.Errorf("未在根等待锁内执行：%s（%s）\n"+
				"  ⇒ 该入口释放/放宽了可用额度，必须走 Pool.withWaitLock（否则等待者可能丢唤醒，"+
				"睡到 WaitReserve 超时才重试）。", sig, reason)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("登记表为空（门禁失效）")
	}
	t.Logf("已核对 %d 个释放/放宽入口均持有根等待锁", checked)
}

// methodBody 返回以 sig 开头的方法体（到下一个**顶层** func 为止）。
func methodBody(files []string, sig string) (string, bool) {
	for _, src := range files {
		idx := strings.Index(src, sig)
		if idx < 0 {
			continue
		}
		rest := src[idx:]
		// 到下一个顶层 func（行首 "func "）为止。
		if next := strings.Index(rest[1:], "\nfunc "); next >= 0 {
			return rest[:next+1], true
		}
		return rest, true
	}
	return "", false
}

// TestQuotaWaitQueueWiringIsSingle 锁定 waitq 收敛后的**关键接线点**（设计 §3.1 I1）：
//
//  1. 等待侧必须用 `waitq.AcquireUnder` 并传入**根等待锁**（`&root.waitMu`）—— 这是丢唤醒
//     窗口的唯一闭合点（「查 + 入队」原子于释放侧的状态变更）；用 `Acquire`（无锁协作）会让
//     等待者睡到超时（可用性退化，且运行时用例无判别力 ⇒ 必须结构锁死）；
//  2. 释放侧唤醒必须由 `Pool.withWaitLock` **唯一收口**（含 NotifyAll），且**在释放 waitMu
//     之后**（fn() 内可能持 node.mu，而唤醒判据要读账本 ⇒ 持锁唤醒会自死锁，RWMutex 不可重入）；
//  3. 不得残留旧式等待实现（sync.Cond / broadcastLocked）—— 已收敛到 pkg/waitq。
func TestQuotaWaitQueueWiringIsSingle(t *testing.T) {
	t.Parallel()
	waitSrc, err := os.ReadFile("../../pkg/quota/wait.go")
	if err != nil {
		t.Fatalf("读取 wait.go 失败: %v", err)
	}
	quotaSrc, err := os.ReadFile("../../pkg/quota/quota.go")
	if err != nil {
		t.Fatalf("读取 quota.go 失败: %v", err)
	}
	wait, quota := string(waitSrc), string(quotaSrc)

	// 1) 等待侧：AcquireUnder + 根等待锁。
	if !strings.Contains(wait, "AcquireUnder(") {
		t.Error("等待侧必须用 waitq.AcquireUnder（锁协作形式）；Acquire 无协作 ⇒ 丢唤醒窗口未闭合")
	}
	if !strings.Contains(wait, "&root.waitMu") {
		t.Error("AcquireUnder 必须传入**根等待锁** &root.waitMu（与释放侧 withWaitLock 同一把）")
	}

	// 2) 释放侧：withWaitLock 唯一收口 + 唤醒在释放 waitMu 之后。
	body, ok := methodBody([]string{quota}, "func (p *Pool) withWaitLock(")
	if !ok {
		t.Fatal("找不到 withWaitLock 方法体")
	}
	if !strings.Contains(body, "NotifyAll()") {
		t.Error("withWaitLock 必须含 queue.NotifyAll()（释放侧唤醒的唯一收口）")
	}
	unlock := strings.Index(body, "root.waitMu.Unlock()")
	notify := strings.Index(body, "NotifyAll()")
	if unlock < 0 || notify < 0 || notify < unlock {
		t.Error("唤醒必须在释放 waitMu **之后**（否则持 node.mu 读账本 ⇒ 自死锁）")
	}

	// 3) 不得残留旧式等待实现。
	for _, bad := range []string{"sync.NewCond", "cond.Broadcast", "broadcastLocked"} {
		if strings.Contains(quota, bad) {
			t.Errorf("残留旧式等待实现 %q —— 应已收敛到 pkg/waitq（唯一实现）", bad)
		}
	}
}
