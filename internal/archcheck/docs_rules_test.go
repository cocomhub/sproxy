// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// docs_rules_test.go 是「**协作与实施规则文档不得腐烂**」的门禁（R9）。
//
// 判据（语义化，不锁死正文）：
//  1. `docs/superpowers/learnings/2026-09-13-agent-operating-rules.md` 必须存在，且**不是占位**
//     （含 §1/§2/§3 三节标题 + 正文长度下限）；
//  2. 本仓 `AGENTS.md` 必须**指向**该文档（否则 agent 读不到完整规则，只看到摘要）；
//  3. 文档必须保留「硬规则」关键锚点（如「等 CI 全绿再合并」「Benchmark」）——防被清空成空壳。
//
// 为什么值得设门禁：本项目大量协作约定（TDD/变异验证/合并流程/踩坑记录）**只在文档里**，
// 而文档极易在「改文件名/大扫除/重构」中被删或改名，导致：
//   - AGENTS.md 的引用悬空（后人点进去 404，以为没有规则）；
//   - 规则被悄悄清空成标题（比没有更危险：看起来有规则，实际无内容）。
// 本门禁把「文档存在 + 被引用 + 有内容」变成机器可验证的约束。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// operatingRulesDocRel 是规则文档相对仓库根的路径（AGENTS.md 必须引用它）。
const operatingRulesDocRel = "docs/superpowers/learnings/2026-09-13-agent-operating-rules.md"

// TestOperatingRulesDocExistsAndReferenced 断言规则文档存在、被 AGENTS.md 引用、且内容非空壳。
func TestOperatingRulesDocExistsAndReferenced(t *testing.T) {
	root := moduleRoot(t)

	docPath := filepath.Join(root, filepath.FromSlash(operatingRulesDocRel))
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatalf("协作规则文档不存在或不可读（%s）: %v\n"+
			"该文档承载用户明示的操作要求与已确认的设计决策；若确需改名，请同步更新 AGENTS.md 的引用。",
			operatingRulesDocRel, err)
	}
	doc := string(b)

	// 正探针①：正文长度下限（防被清空成标题）。
	if len(doc) < 3000 {
		t.Fatalf("规则文档过短（%d 字节 < 3000）：疑似被清空成占位，规则文档必须保留实质内容", len(doc))
	}
	// 正探针②：三节结构必须在（§1 操作要求 / §2 设计决策 / §3 实施经验）。
	for _, marker := range []string{"## 1.", "## 2.", "## 3."} {
		if !strings.Contains(doc, marker) {
			t.Fatalf("规则文档缺少结构标记 %q（§1 操作要求 / §2 设计决策 / §3 实施经验 三节不得缺失）", marker)
		}
	}
	// 正探针③：硬规则关键锚点仍在内（防替换为无关内容）。
	for _, anchor := range []string{"TDD", "Benchmark", "删分支", "变异"} {
		if !strings.Contains(doc, anchor) {
			t.Fatalf("规则文档缺少关键锚点 %q：文档可能被替换为无关内容", anchor)
		}
	}

	// AGENTS.md 必须指向该文档（否则后续 agent 只看得到摘要，读不到完整规则）。
	agentsPath := filepath.Join(root, "AGENTS.md")
	ab, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("读取 AGENTS.md 失败: %v", err)
	}
	if !strings.Contains(string(ab), operatingRulesDocRel) {
		t.Fatalf("AGENTS.md 必须引用 %s（完整规则只在该文档里；摘要不足以保证后续 agent 遵循）",
			operatingRulesDocRel)
	}
}
