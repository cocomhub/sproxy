// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// gate_index_gate_test.go 是门禁 **R56**：`docs/gates.md` 索引必须覆盖全部门禁文件。
//
// 为什么需要：`internal/archcheck/` 现有 70+ 个门禁文件，`docs/gates.md` 是它们唯一的
// 人类可读索引。该索引曾在一次集中整理里**手工回填**到 74/74——手工回填过的清单没有
// 机制约束，下一个人加门禁时不会记得补索引，半年后索引就只剩一半（与 R55 文档索引同一
// 腐化模式）。本门禁把「加门禁必须登记索引」变成 CI 判据。
//
// 口径：`internal/archcheck/*_test.go` 中，凡**不是纯测试辅助**的文件，其文件名必须
// 出现在 `docs/gates.md`（按文件名精确匹配，允许索引里写 `foo_test.go`）。
//
// 变异：删除 `docs/gates.md` 里任意一行门禁条目 → 本门禁红。

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gateIndexNonGateTests 是不承担门禁职责的测试辅助文件（枚举而非前缀匹配：
// 新增辅助文件必须显式登记，避免用命名习惯绕过门禁）。
var gateIndexNonGateTests = map[string]bool{
	"arch_test.go":                      true,  // 导入图/模块根等公共辅助
	"helpers_test.go":                   true,  // 通用断言辅助
	"route_classification_gate_test.go": false, // 占位：确保 map 非空且格式正确
}

// TestGateIndexCoversAllGates 断言每个门禁文件都在 docs/gates.md 中登记。
func TestGateIndexCoversAllGates(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	docs, err := os.ReadFile(filepath.Join(root, "docs", "gates.md"))
	if err != nil {
		t.Fatalf("读取 docs/gates.md: %v", err)
	}
	index := string(docs)

	entries, err := os.ReadDir(filepath.Join(root, "internal", "archcheck"))
	if err != nil {
		t.Fatalf("读取 internal/archcheck: %v", err)
	}
	var gates []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		if gateIndexNonGateTests[name] {
			continue
		}
		gates = append(gates, name)
	}
	// 扫描面自证：门禁文件过少说明目录读取/过滤失效（门禁假绿）。
	if len(gates) < 60 {
		t.Fatalf("识别到 %d 个门禁文件，少于下限 60 —— 扫描面失效（门禁假绿）", len(gates))
	}
	var missing []string
	for _, name := range gates {
		if !strings.Contains(index, name) {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("以下门禁文件未在 docs/gates.md 登记（R56：新增门禁必须同步索引）：\n  %s",
			strings.Join(missing, "\n  "))
	}
}
