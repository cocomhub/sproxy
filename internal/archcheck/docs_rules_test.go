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
	"strconv"
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

// TestAgentsHardRulesStructure AGENTS.md「协作与流程硬规则」结构门禁（R9 扩展，2026-09-15）：
//
// 为何设：2026-09-15 一次修复把 4 条新规则（R18 并发注册/本地先行/PR 复用/禁止 amend）
// **插错章节**（落进「常用命令」并造成编号割裂），且第 12 条 CHANGELOG 说明在配置改动后
// 未同步（旧文："chore/docs/test 不进 changelog"）。这类漂移只在人读文档时才暴露——
// 用机器断言把它变成可验证约束：
//  1. 硬规则列表编号从 1 连续到 N（无缺号、无重号、无越章节错位）；
//  2. 必含关键条款锚点（R18 并发注册门禁、测试网络客户端隔离、本地先过后 push）；
//  3. 不得出现已过时的表述（`hidden` 移除后「六类不进 changelog」已不成立）。
func TestAgentsHardRulesStructure(t *testing.T) {
	// 纯文档解析（只读 md 文件，无共享可变状态）⇒ 直接并发。
	t.Parallel()
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil {
		t.Fatalf("读取 AGENTS.md: %v", err)
	}
	doc := string(b)

	// 规则章节切片：从「协作与流程硬规则」到下一个二级标题。
	start := strings.Index(doc, "## 协作与流程硬规则")
	if start < 0 {
		t.Fatal("AGENTS.md 缺少「## 协作与流程硬规则」章节")
	}
	rest := doc[start:]
	if end := strings.Index(rest[1:], "\n## "); end >= 0 {
		rest = rest[:end+1]
	}
	// 编号连续性（每条以 `N. **` 开头）
	nums := []int{}
	for line := range strings.SplitSeq(rest, "\n") {
		if i := strings.Index(line, ". **"); i > 0 {
			if n, convErr := strconv.Atoi(strings.TrimSpace(line[:i])); convErr == nil {
				nums = append(nums, n)
			}
		}
	}
	if len(nums) < 10 {
		t.Fatalf("未解析到硬规则编号列表（%d 条）：格式须为 `N. **...`，本次解析: %v", len(nums), nums)
	}
	for i, n := range nums {
		if n != i+1 {
			t.Fatalf("硬规则编号不连续：第 %d 条应为 %d，实际 %d（编号 %v）——新增规则必须追加到列表末尾并保持 1..N 连续", i+1, i+1, n, nums)
		}
	}
	// 关键条款锚点
	for _, anchor := range []string{"sproxy:serial:", "serial_budgets.tsv", "本地全绿后才 push", "http.DefaultTransport", "推送走 https 或 SSH", "提交信息与 PR 描述原则"} {
		if !strings.Contains(doc, anchor) {
			t.Fatalf("AGENTS.md 缺少关键条款锚点 %q（新增/重写规则时不得删除这些硬约束）", anchor)
		}
	}
	// 过期表述禁入（changelog 全类型可见后，这三句都已不成立）
	for _, stale := range []string{"默认**不进** changelog", "不产生 release PR", "SSH 不可用"} {
		if strings.Contains(doc, stale) {
			t.Fatalf("AGENTS.md 含已过时表述 %q：release-please 已改为全类型可见（### Changed 段）", stale)
		}
	}
	// 同款表述不得在镜像文档/发布文档里复活（R12 的文档侧补充）。
	for _, f := range []string{"CLAUDE.md", "RELEASING.md"} {
		b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if rerr != nil {
			continue
		}
		for _, stale := range []string{"默认**不进** changelog", "不产生 release PR", "**不会**出现"} {
			if strings.Contains(string(b), stale) {
				t.Fatalf("%s 含已过时表述 %q：changelog 已改为全类型可见（### Changed 段）", f, stale)
			}
		}
	}
	// 「已知的技术债务」节已整节移除（2026-09-15：条目经核实全部过时/已消除——
	// 信号 goroutine 泄漏有 defer close、findModuleRoot/parseDuration 零残留、
	// rename TOCTOU 已上 FileLocks、UpdateConfig 已接线、cloud O(n) 已审计）。
	// 防死灰复燃：权威文档不得再出现该章节标题（避免过时清单继续干扰后来的 agent）。
	for _, b := range [][]byte{
		mustRead(t, root, "AGENTS.md"),
		mustRead(t, root, "CLAUDE.md"),
	} {
		if strings.Contains(string(b), "已知的技术债务") {
			t.Fatal("权威文档不得再出现「已知的技术债务」节：现存条目经核实均过时/已消除，" +
				"重新列出会再次误导后续 agent——如确有新债，先修掉或写到 docs/archive/ 归档经验")
		}
	}
	// 已移除的配置项不得再被呈现为「可配置」（`max_upload_bytes` 现为硬编码 1 GiB 上限）。
	for _, f := range []string{"AGENTS.md", "CLAUDE.md", "docs/config.md", "docs/api.md"} {
		b, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if rerr != nil {
			continue
		}
		for line := range strings.SplitSeq(string(b), "\n") {
			if strings.Contains(line, "max_upload_bytes") && strings.Contains(line, "int64") {
				t.Fatalf("%s 仍把已移除的 `max_upload_bytes` 呈现为可配置项（现为硬编码 1 GiB 上限）: %s", f, strings.TrimSpace(line))
			}
		}
	}
}

// TestAuthoritativeDocsHaveNoRemovedArtifacts 权威文档不得引用「已移除」的路由/文件（R9 扩展，2026-09-15）。
//
// 为何设：文档最典型、最容易发生的腐烂是**引用已删掉的东西**——例如
// `PUT /api/storage/config`（已由 `PUT /api/config` 取代）与 `xferhttp`/`pkg/tunnel/xfer/http.go`
// （内置传输早已换成 TCP）。这类漂移人读文档时才会发现，故把它变成机器约束。
//
// 范围：只覆盖「权威文档」（根 README/AGENTS/CLAUDE + docs/*.md + docs/testing/*.md）。
// 注意：docs/archive/** 不参与本断言（归档历史允许保留旧名）；docs/superpowers/learnings 仅
// 保留 4 份被外部硬引用的规则文档，同样不参与。
func TestAuthoritativeDocsHaveNoRemovedArtifacts(t *testing.T) {
	// 纯文档解析（只读 md 文件，无共享可变状态）⇒ 直接并发。
	t.Parallel()
	root := moduleRoot(t)
	files := []string{"README.md", "AGENTS.md", "CLAUDE.md"}
	for _, g := range []string{"docs/*.md", "docs/testing/*.md"} {
		m, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(g)))
		if err != nil {
			t.Fatalf("glob %s: %v", g, err)
		}
		for _, p := range m {
			rel, rerr := filepath.Rel(root, p)
			if rerr == nil {
				files = append(files, filepath.ToSlash(rel))
			}
		}
	}
	// 已移除的产物 → 替代说法（供错误信息提示）
	removed := map[string]string{
		"/api/storage/config": "PUT /api/config（运行时配置，含 max_storage_bytes）",
		"xferhttp":            "TCP 内置传输（pkg/tunnel/xfer/internal/tcp）",
		"xfer/http.go":        "pkg/tunnel/xfer/internal/tcp/",
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		body := string(b)
		for bad, replacement := range removed {
			if strings.Contains(body, bad) {
				t.Fatalf("权威文档 %s 引用了已移除的 %q —— 请改为 %s（文档必须反映当前实现）", f, bad, replacement)
			}
		}
	}
}

// mustRead 读取仓库内文件，失败即 Fatal。
func mustRead(t *testing.T, root, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s: %v", rel, err)
	}
	return b
}
