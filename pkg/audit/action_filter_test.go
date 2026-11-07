// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"testing"
	"time"
)

// action 过滤的前缀族契约（由 pkg/server 的 AuditFilter 迁入 —— 语义唯一实现在本包）。
func TestActionMatches_PrefixFamily(t *testing.T) {
	t.Parallel()
	cases := []struct {
		action, filter string
		want           bool
	}{
		{"ai.summarize", "ai.", true},          // 前缀族
		{"ai.tag", "ai.", true},                // 前缀族
		{"ai.summarize", "ai.summarize", true}, // 精确
		{"ai.tag", "ai.summarize", false},
		{"delete", "dele", false}, // 不以 "." 结尾 ⇒ 精确（不得前缀命中）
		{"", "ai.", false},
	}
	for _, tc := range cases {
		if got := actionMatches(tc.action, tc.filter); got != tc.want {
			t.Errorf("actionMatches(%q, %q) = %v, want %v", tc.action, tc.filter, got, tc.want)
		}
	}
}

func TestEntryFilter_ActionPrefixAndMesh(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0)
	e := Entry{Action: "ai.summarize", Mesh: "mesh-1", TS: now}
	if !(EntryFilter{Action: "ai."}).Match(e) {
		t.Errorf("前缀族过滤应命中 ai.summarize")
	}
	if (EntryFilter{Action: "ai.tag"}).Match(e) {
		t.Errorf("精确过滤不应命中 ai.summarize")
	}
	if !(EntryFilter{Mesh: "mesh-1"}).Match(e) {
		t.Errorf("mesh 过滤应命中")
	}
	if (EntryFilter{Mesh: "mesh-2"}).Match(e) {
		t.Errorf("mesh 不匹配不应命中")
	}
	if (EntryFilter{Since: now.Add(time.Hour)}).Match(e) {
		t.Errorf("Since 之后的条目才应命中")
	}
}
