// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// TestReleasePleasePRTitlePatternCarriesVersion 守住 release-please 的 release PR
// 标题模板必须显式配置且含 ${version}。
//
// 背景（v0.11.1 实障，2026-09-15）：仓库用的是 release-please 缺省标题模板，
// 渲染出的标题不含版本号（`chore: release master`）。合并该 release PR 后，
// release-please 再次运行时无法把「已合并的 release PR」与版本关联，日志出现
//
//	⚠ pullRequestTitlePattern miss the part of '${version}'
//	⚠ There are untagged, merged release PRs outstanding - aborting
//
// 于是既不打 tag 也不建 Release，发布链静默断在 "manifest 已 0.11.1、tag 不存在"。
// 显式写死含 ${version} 的模板即可消除该卡点（并顺带带上 scope 与组件名）。
func TestReleasePleasePRTitlePatternCarriesVersion(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../release-please-config.json")
	if err != nil {
		t.Fatalf("读取 release-please-config.json: %v", err)
	}
	var cfg struct {
		PullRequestTitlePattern string `json:"pull-request-title-pattern"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("解析 release-please-config.json: %v", err)
	}
	if cfg.PullRequestTitlePattern == "" {
		t.Fatal("必须显式配置 pull-request-title-pattern：缺省模板不含 ${version}，" +
			"会让已合并的 release PR 无法与 tag 关联并触发 release-please abort")
	}
	if !strings.Contains(cfg.PullRequestTitlePattern, "${version}") {
		t.Errorf("pull-request-title-pattern 必须含 ${version}（当前 %q）", cfg.PullRequestTitlePattern)
	}
	if !strings.Contains(cfg.PullRequestTitlePattern, "${component}") {
		t.Errorf("pull-request-title-pattern 建议含 ${component} 以区分多包（当前 %q）", cfg.PullRequestTitlePattern)
	}
}
