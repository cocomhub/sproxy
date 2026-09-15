// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestReleasePleasePRTitlePatternCarriesVersion 守住 release-please 的**两个** release PR 标题模板
// 都必须显式配置且含 ${version}。
//
// 背景（v0.11.1 实障，2026-09-15）：模板缺版本号（缺省渲染成 `chore: release master`）⇒ 合并后
// release-please 无法把「已合并的 release PR」与版本关联，日志报
//
//	⚠ pullRequestTitlePattern miss the part of '${version}'
//	⚠ There are untagged, merged release PRs outstanding - aborting
//
// ⇒ 既不建 tag 也不建 Release（v0.11.1 只能人工补 tag + 翻转标签才恢复，制品再用
// `gh workflow run release.yml -f tag=v0.11.1` 补发）。
//
// 两个键缺一不可：本仓 `separate-pull-requests: false`（聚合 PR）**实际使用
// `group-pull-request-title-pattern`**，只配 `pull-request-title-pattern` 等于没改
// （2026-09-15 实测：改完前者后日志仍报 `miss the part of '${version}'`）。
// 模板中不要使用 `${component}`：本仓日志实测 `component:` 为空，会渲染出双空格标题。
//
// 同时钉住 RELEASING.md 的两条运行经验（改模板后既有 open release PR 不会被改写标题、需手动改名；
// 以及卡死后的恢复步骤），避免下次重踩。
func TestReleasePleasePRTitlePatternCarriesVersion(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../release-please-config.json")
	if err != nil {
		t.Fatalf("读取 release-please-config.json: %v", err)
	}
	var cfg struct {
		PullRequestTitlePattern      string `json:"pull-request-title-pattern"`
		GroupPullRequestTitlePattern string `json:"group-pull-request-title-pattern"`
		SeparatePullRequests         *bool  `json:"separate-pull-requests"`
	}
	if unmarshalErr := json.Unmarshal(raw, &cfg); unmarshalErr != nil {
		t.Fatalf("解析 release-please-config.json: %v", unmarshalErr)
	}

	for _, p := range []struct{ key, val string }{
		{"pull-request-title-pattern", cfg.PullRequestTitlePattern},
		{"group-pull-request-title-pattern", cfg.GroupPullRequestTitlePattern},
	} {
		if p.val == "" {
			t.Errorf("必须显式配置 %s：缺省模板不含 ${version}，会让已合并的 release PR 无法与 tag "+
				"关联并触发 release-please abort", p.key)
			continue
		}
		if !strings.Contains(p.val, "${version}") {
			t.Errorf("%s 必须含 ${version}（当前 %q）", p.key, p.val)
		}
		if strings.Contains(p.val, "${component}") {
			t.Errorf("%s 不应使用 ${component}：本仓 component 为空，会渲染出双空格标题（当前 %q）", p.key, p.val)
		}
	}

	// 判据前提：聚合 PR 由 group 模板命名。若改为 true（每包一个 PR），需同步复核本测试。
	if cfg.SeparatePullRequests == nil || *cfg.SeparatePullRequests {
		t.Fatalf("本门禁假定 separate-pull-requests=false（聚合 PR），实际=%v；配置变更需同步更新本测试",
			cfg.SeparatePullRequests)
	}

	relRaw, relErr := os.ReadFile("../../RELEASING.md")
	if relErr != nil {
		t.Fatalf("读取 RELEASING.md: %v", relErr)
	}
	relText := string(relRaw)
	for _, anchor := range []string{
		"group-pull-request-title-pattern", // 聚合 PR 用的是这个模板
		"autorelease: tagged",              // 卡死恢复要翻转的标签
		"gh pr edit",                       // 改模板后必须手动改名既有 open release PR
		"gh workflow run release.yml",      // 制品补发入口
	} {
		if !strings.Contains(relText, anchor) {
			t.Errorf("RELEASING.md 缺少 %q：标题模板与卡死恢复的运行经验必须成文（v0.11.1 实测教训）", anchor)
		}
	}
}
