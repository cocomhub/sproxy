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
