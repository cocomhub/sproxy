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
//     会重新退回逐 commit 手改 CHANGELOG——这正是本门禁要防的漂移。

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
		IncludeVInTag bool `json:"include-v-in-tag"`
		Packages      map[string]struct {
			ChangelogPath string `json:"changelog-path"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
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
