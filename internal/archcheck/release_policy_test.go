// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// release_policy_test.go 是「**CHANGELOG 单一事实源不得被回退成手工维护**」的门禁（R12）。
//
// 背景（2026-09-14）：`CHANGELOG.md` 与版本号改为由 release-please 生成（用户决策）。此前
// 「每次 commit 手工同步 CHANGELOG」的做法与之冲突，且实测造成真实损失——写在 `[Unreleased]`
// 的 4 条 `### Removed`（对外 API 删除）**没有**进入 0.11.1 的版本段（release-please 不消费
// `[Unreleased]`），即「手工维护」不但冗余，还会静默丢失信息。
//
// 判据（结构性，不锁死正文）：
//  1. `release-please-config.json` 存在，且根包 `.` 声明 `changelog-path: CHANGELOG.md`、
//     `include-v-in-tag: true`（tag 形如 `vX.Y.Z`，与既有 tag 及 GoReleaser 触发方式一致）；
//  2. 策略必须同时写在 `AGENTS.md` 与 `CLAUDE.md`（两镜像一致），否则后人只看到旧摘要，
//     会重新退回逐 commit 手改 CHANGELOG——这正是本门禁要防的漂移；
//  3. `CHANGELOG.md` **不得含 `## [Unreleased]` 段**：release-please 以「第一个版本标题」
//     （`DEFAULT_VERSION_HEADER_REGEX = '\n###? v?[0-9[]'`）作插入锚点，而 `## [Unreleased]`
//     因 `[` 恰好命中 ⇒ 新版本段会被插到它**上面**，且它从不被消费/清理（写进去的内容
//     永远不会进入任何版本）；
//  4. `changelog-sections` 必须含 `remove` → `Removed`（删除对外 API 用提交类型表达，
//     免人工补条目 + 免被 release-please 重建时覆盖）；
//  5. `RELEASING.md` 必须存在且写明 release-please 流程与嵌套模块 tag 步骤（发布流程的唯一事实源）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// releasePleaseConfigRel 是 CHANGELOG/版本号的单一事实源配置（workflow 的 config-file 指向它）。
const releasePleaseConfigRel = "release-please-config.json"

// TestReleasePleaseIsChangelogSingleSource 断言 release-please 配置是 CHANGELOG 的落点，
// 且该策略在 AGENTS.md / CLAUDE.md 两处硬规则中都有记载。
func TestReleasePleaseIsChangelogSingleSource(t *testing.T) {
	root := moduleRoot(t)

	cfgPath := filepath.Join(root, releasePleaseConfigRel)
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("release-please 配置缺失（%s）: %v\n"+
			"CHANGELOG.md 与版本号由它生成；若确需改名，请同步 .github/workflows/release-please.yml 的 config-file。",
			releasePleaseConfigRel, err)
	}

	var cfg struct {
		IncludeVInTag     bool `json:"include-v-in-tag"`
		ChangelogSections []struct {
			Type    string `json:"type"`
			Section string `json:"section"`
			Hidden  bool   `json:"hidden"`
		} `json:"changelog-sections"`
		Packages map[string]struct {
			ChangelogPath string `json:"changelog-path"`
		} `json:"packages"`
	}
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("解析 %s 失败: %v", releasePleaseConfigRel, err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("%s 必须声明根包 \".\"（CHANGELOG 与版本号的落点）", releasePleaseConfigRel)
	}
	if pkg.ChangelogPath != "CHANGELOG.md" {
		t.Fatalf("根包 changelog-path = %q，必须为 CHANGELOG.md（单一事实源）", pkg.ChangelogPath)
	}
	if !cfg.IncludeVInTag {
		t.Fatalf("%s 的 include-v-in-tag 必须为 true（tag 形如 vX.Y.Z，与既有 tag 及 GoReleaser 触发的 tag 模式一致）",
			releasePleaseConfigRel)
	}

	// 删除对外 API 必须能用提交类型表达（remove → Removed），否则只能人工补条目，
	// 而人工补的内容会在 release-please 重建 release PR 时被覆盖（已于 0.11.1 踩到）。
	hasRemove := false
	for _, s := range cfg.ChangelogSections {
		// 全类型可见（用户明示 2026-09-16）：docs/chore/ci/test/build/style 不得再 hidden，
		// 否则这些提交在 CHANGELOG 里凭空消失（与 AGENTS.md/RELEASING.md 的约定冲突）。
		if s.Hidden {
			t.Fatalf("changelog-sections 的 %q 不得设 hidden: true——本仓约定「所有提交都要在 CHANGELOG 体现」", s.Type)
		}
		if s.Type == "remove" && s.Section == "Removed" {
			hasRemove = true
		}
	}
	if !hasRemove {
		t.Fatal("changelog-sections 必须含 {\"type\":\"remove\",\"section\":\"Removed\"}：删除对外 API 用 `remove(...)` 提交类型表达" +
			"（免人工补条目，也免被 release-please 重建 release PR 时覆盖）")
	}

	// CHANGELOG.md 不得含 `## [Unreleased]`：release-please 以第一个版本标题为插入锚点，
	// [Unreleased] 会命中该正则 ⇒ 新段被插到它上面，且它从不被消费/清理。
	cb, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("读取 CHANGELOG.md 失败: %v", err)
	}
	if strings.Contains(string(cb), "\n## [Unreleased]") {
		t.Fatal("CHANGELOG.md 不得包含 `## [Unreleased]` 段：release-please 不消费它，" +
			"还会把每个新版本段插到它上面（写进去的内容永远不会进入任何版本）。见 RELEASING.md")
	}

	// 发布流程文档必须在：release PR 审校 / 嵌套 tag 步骤只有它写（AGENTS/CLAUDE 只给摘要）。
	rel, err := os.ReadFile(filepath.Join(root, "RELEASING.md"))
	if err != nil {
		t.Fatalf("RELEASING.md 缺失: %v（发布流程：release PR 审校 → 合并 → 补嵌套 tag → 验制品）", err)
	}
	for _, anchor := range []string{"release-please", "嵌套"} {
		if !strings.Contains(string(rel), anchor) {
			t.Fatalf("RELEASING.md 缺少关键锚点 %q（防被删/清空成空壳）", anchor)
		}
	}

	// 策略必须落在两处镜像硬规则里（只写一处会让另一处继续教「手工维护 CHANGELOG」）。
	for _, rel := range []string{"AGENTS.md", "CLAUDE.md"} {
		ab, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", rel, err)
		}
		if !strings.Contains(string(ab), "release-please") {
			t.Fatalf("%s 的硬规则必须写明「CHANGELOG 由 release-please 生成，不再手工维护」"+
				"（否则后人会退回逐 commit 手改 CHANGELOG，而 release-please 不消费 [Unreleased]，会造成静默丢失）", rel)
		}
	}
}

// TestReleasePRChangelogEntriesHaveScope CHANGELOG 中 release-please 生成的条目必须带 scope。
//
// 背景（2026-09-15，0.11.1 发布审校实证）：PR #275 的分支提交 subject 为 `docs: 全量刷新...`
// （类型后直接冒号、无 scope），squash 后 release-please 产出无 scope 条目
// （`* 全量刷新 md ...`），与同段 `* **docs:** ...` 形式不一致。
//
// 判据（精确、不误伤历史手写段）：仅校验「release-please 生成」的条目——识别特征是
// 条目行同时满足 ① 以 `* ` 开头；② 含本仓 commit 链接。历史手写段用 `-` 且无链接，
// 不参与校验。若仓库尚无 release-please 段（首个 release 之前）则记录并跳过。
func TestReleasePRChangelogEntriesHaveScope(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "CHANGELOG.md"))
	if err != nil {
		t.Fatalf("读取 CHANGELOG.md: %v", err)
	}
	scanned := 0
	for ln := range strings.SplitSeq(string(b), "\n") {
		if !strings.HasPrefix(ln, "* ") {
			continue
		}
		if !strings.Contains(ln, "github.com/cocomhub/sproxy/commit/") {
			continue
		}
		scanned++
		rest := strings.TrimPrefix(ln, "* ")
		if strings.HasPrefix(rest, "**") && strings.Contains(rest, ":**") {
			continue
		}
		t.Fatalf("发现 release-please 生成的**无 scope 条目**（%q）。"+
			"本仓 squash 合并按分支提交信息生成 CHANGELOG，提交 subject 必须是 `type(scope): 描述`；"+
			"预防见 `.githooks/commit-msg`（scope 必填），补救则在 release PR 中补 `**scope:**`", ln)
	}
	if scanned == 0 {
		t.Log("CHANGELOG 尚无 release-please 段落（首个 release 前），本门禁暂不生效；" +
			"提交信息仍由 .githooks/commit-msg 强制 scope")
	}
}

// TestCommitMsgHookRequiresScope 门禁：提交信息 scope 强制必须"有执行点 + 有文档"。
//
// 背景（2026-09-15）：PR #275 的分支提交 `docs: 全量刷新...` 缺 scope，squash 后污染
// 0.11.1 段。根因在**提交时**，故预防必须落在 commit-msg 钩子（事后再改 PR 标题无效）。
// 本门禁确保：钩子存在、含 scope 必填正则、Makefile 安装目标接线、RELEASING.md 有说明
// 与「何时 minor 提升」的机制表——任一处丢失即失败（防有人删钩子/删文档）。
func TestCommitMsgHookRequiresScope(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	hookPath := filepath.Join(root, ".githooks", "commit-msg")
	hb, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("缺少 %s：本仓要求提交信息 `type(scope): 描述`（scope 必填）——"+
			"缺钩子会让无 scope 提交进入 CHANGELOG（0.11.1 曾发生）", hookPath)
	}
	hook := string(hb)
	if !strings.Contains(hook, `\([a-z0-9_,.-]+\)!?: `) {
		t.Fatal(".githooks/commit-msg 必须校验 `type(scope): subject`（scope 必填）——正则锚点丢失")
	}
	if !strings.Contains(hook, "squash") {
		t.Fatal(".githooks/commit-msg 应说明本仓 squash 按分支提交信息生成 CHANGELOG 的原因")
	}

	mk, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}
	if !strings.Contains(string(mk), "git config core.hooksPath .githooks") {
		t.Fatal("Makefile 的 githooks 目标必须安装 core.hooksPath .githooks（否则 commit-msg 不生效）")
	}

	rel, err := os.ReadFile(filepath.Join(root, "RELEASING.md"))
	if err != nil {
		t.Fatalf("读取 RELEASING.md: %v", err)
	}
	doc := string(rel)
	for _, anchor := range []string{"scope 必填", "squash", "v0.11", "0.12.0", "bump-minor-pre-major"} {
		if !strings.Contains(doc, anchor) {
			t.Fatalf("RELEASING.md 缺少机制说明锚点 %q（提交规范/版本提升规则必须成文，否则后人只能靠猜）", anchor)
		}
	}
}
