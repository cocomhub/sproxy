// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// ledger_mutation_gate_test.go 是「**账本变更必须全在唯一临界区**」门禁（R32）。
//
// 触发背景（独立审核 P2，2026-10-18）：`LedgerCounter` 的变更方法各自持 `c.mu` 做「读 total →
// 判定 → Add」，而 `CorrectTo` **漏在锁外**（读 `Total()` 与 `Add()` 之间可被并发变更插入）⇒
// 两个并发 `CorrectTo(同一真值)` 各读 `cur=0`、各加真值 ⇒ **双计**（Used=2×真值）；与 `TryAdd`
// 交错还会让超限判定基于陈旧 total。
//
// 为何需要门禁：这是**逻辑竞态**（各方法内部持锁 ⇒ `-race` 不报 ✗），且交错窗口极短 ⇒
// **无法确定性构造能变红的用例** ✗。⇒ 改为锁定**结构**：直接触碰账本原语
// （`c.l.Add(` / `c.l.Total(`）的方法必须在允许清单内；新增变更方法若绕过 `withLock`
// （读改写不在同一临界区）⇒ 本门禁**必红**。
//
// 判据（结构性、可判定）：扫描 `pkg/volume/capacity` 下非测试源码，凡含 `c.l.Add(` 或
// `c.l.Total(` 的**方法**，其方法名必须在允许清单内（每个都写明理由）。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// ledgerMutationAllowlist 允许直接触碰账本原语的方法（必须写明理由）。
var ledgerMutationAllowlist = map[string]string{
	"withLock":      "**唯一临界区原语**：在 c.mu 内读 total 并交给回调，变更必须经它。",
	"addWithinLock": "在 withLock 回调内执行「上限校验 + 入账 + 提交池预留」（自身不取锁，由 withLock 持有）。",
}

var (
	ledgerMethodRe = regexp.MustCompile(`(?m)^func \(c \*LedgerCounter\) ([A-Za-z0-9_]+)\(`)
	// 只认**变更**原语 `c.l.Add(`（`c.l.Total(` 是读，无需临界区）。
	ledgerPrimRe = regexp.MustCompile(`c\.l\.Add\(`)
)

// TestLedgerMutationsGoThroughWithLock 账本变更方法必须经 withLock（或显式登记理由）。
func TestLedgerMutationsGoThroughWithLock(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	dir := filepath.Join(root, "pkg", "volume", "capacity")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读 %s: %v", dir, err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("读 %s: %v", e.Name(), rerr)
		}
		src := string(b)
		// 切分各方法体（以 `func (c *LedgerCounter) Name(` 为界）。
		idxs := ledgerMethodRe.FindAllStringSubmatchIndex(src, -1)
		for i, m := range idxs {
			name := src[m[2]:m[3]]
			end := len(src)
			if i+1 < len(idxs) {
				end = idxs[i+1][0]
			}
			body := src[m[0]:end]
			if !ledgerPrimRe.MatchString(body) {
				continue
			}
			checked++
			// 合规：方法体内出现 withLock(（变更在该临界区内），或已在允许清单（withLock 自身
			// 与其回调子函数）。仅「直接 c.l.Add( 且不经 withLock」才是缺陷。
			if strings.Contains(body, "withLock(") {
				continue
			}
			if _, ok := ledgerMutationAllowlist[name]; !ok {
				t.Errorf("pkg/volume/capacity/%s 的方法 %s 直接触碰账本原语（c.l.Add/c.l.Total）但不在允许清单内 —— "+
					"变更必须经**唯一临界区** LedgerCounter.withLock（否则「读 total → 判定 → Add」可被并发插入 ⇒ "+
					"双计/超限判定失真；这是 -race 抓不到的逻辑竞态）。请改用 withLock，或登记理由", e.Name(), name)
			}
		}
	}
	if checked == 0 {
		t.Fatal("未扫描到任何触碰账本原语的方法（门禁失效：正则或路径已腐坏）")
	}
}
