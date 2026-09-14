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
