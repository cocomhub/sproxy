// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// quota_capability_wiring_gate_test.go 是「**决策驱动的配额能力必须有生产接线**」门禁（R31）。
//
// 触发背景（独立审核 P2，2026-10-18）：`Scope/Pool.WaitReserve`（配额不足**排队重试**）与
// `Pool.SetOperationalReserve`（**运维余量**）实现完整、注释明写「2026-10-14 用户裁定」——
// 但**生产调用点为零** ✗ ⇒ 静默 no-op ⇒ 用户裁定的「真正放得下的文件最终一定成功」与
// 「绝不磁盘打满」**全部落空**，且没有任何测试/工具报错。
//
// 为何 deadcode 工具抓不到：两个符号都是**导出**的（导出即「非死」），而它们缺的是**装配层
// 调用点**。⇒ 需要本门禁：把「决策驱动的能力」显式登记，并强制「已接线」有生产调用点。
//
// 判据（结构性、可判定）：
//  1. `quota_capability_wiring.tsv` 登记「决策驱动的能力」符号；
//  2. status=wired ⇒ 必须在 **pkg/quota 之外的非测试源码**里存在调用点（`Sym(` 形式）；
//  3. status=unwired ⇒ 必须写明理由（显式决定，不得静默）；wired 条目不得再留理由。
//
// 新增此类能力（注释里写「用户裁定/决策」的导出 API）时必须同步登记 ⇒ 不可能再静默落空。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// quotaCapabilityRow 是登记表一行。
type quotaCapabilityRow struct {
	symbol string
	status string // wired | unwired
	reason string
}

// readQuotaCapabilityWiring 解析登记表。
func readQuotaCapabilityWiring(t *testing.T, root string) []quotaCapabilityRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "archcheck", "quota_capability_wiring.tsv"))
	if err != nil {
		t.Fatalf("读 quota_capability_wiring.tsv: %v", err)
	}
	var rows []quotaCapabilityRow
	for ln := range strings.SplitSeq(string(b), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" || strings.HasPrefix(ln, "#") {
			continue
		}
		parts := strings.SplitN(ln, "\t", 3)
		if len(parts) < 2 {
			t.Fatalf("登记表行格式错误（需 symbol\\tstatus[\\t理由]）: %q", ln)
		}
		row := quotaCapabilityRow{symbol: strings.TrimSpace(parts[0]), status: strings.TrimSpace(parts[1])}
		if len(parts) == 3 {
			row.reason = strings.TrimSpace(parts[2])
		}
		if row.status != "wired" && row.status != "unwired" {
			t.Fatalf("status 必须为 wired/unwired: %q", ln)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		t.Fatal("登记表为空（本门禁无意义）")
	}
	return rows
}

// scanQuotaCallSites 返回 pkg/quota 之外、非测试源码中调用 symbol 的文件（仓库相对路径）。
func scanQuotaCallSites(t *testing.T, root, symbol string) []string {
	t.Helper()
	// 调用形态：方法调用 `.Sym(` 或函数调用 `Sym(`——必须**词边界**匹配，否则
	// `SetOperationalReserve(` 会误命中 `OperationalReserve(`（前缀包含）⇒ 假 wired。
	re := regexp.MustCompile(`(^|[^A-Za-z0-9_])` + regexp.QuoteMeta(symbol) + `\(`)
	var out []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "build", "node_modules", "web", ".worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "pkg/quota/") {
			return nil // 定义处不算接线
		}
		b, rerr2 := os.ReadFile(path)
		if rerr2 != nil {
			return rerr2
		}
		if re.Match(b) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描 %s 调用点: %v", symbol, err)
	}
	return out
}

// TestQuotaCapabilityWiring 「决策驱动的配额能力」必须已接线（或有显式未接线理由）。
func TestQuotaCapabilityWiring(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	for _, row := range readQuotaCapabilityWiring(t, root) {
		sites := scanQuotaCallSites(t, root, row.symbol)
		switch row.status {
		case "wired":
			if len(sites) == 0 {
				t.Errorf("登记为 wired 的 %s 在 pkg/quota 之外**没有任何生产调用点** —— "+
					"这正是「能力空洞」缺陷形态（领域层实现了、装配层从未接线 ⇒ 用户裁定静默落空）。"+
					"请接线，或改为 unwired 并写明理由", row.symbol)
			}
			if row.reason != "" {
				t.Errorf("wired 条目 %s 不应带理由（请清空或改为 unwired）", row.symbol)
			}
		case "unwired":
			if row.reason == "" {
				t.Errorf("unwired 条目 %s 必须写明理由（显式决定，不得静默）", row.symbol)
			}
			if len(sites) > 0 {
				t.Errorf("登记为 unwired 的 %s 实际已有调用点 %v ⇒ 请更新登记为 wired", row.symbol, sites)
			}
		}
	}
}
