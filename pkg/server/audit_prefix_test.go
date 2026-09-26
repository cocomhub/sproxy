// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"
)

// TestAuditMatches_AIPrefix 验证 action 以 "." 结尾 → 前缀族匹配。
func TestAuditMatches_AIPrefix(t *testing.T) {
	t.Parallel()
	evt := AuditEvent{Action: "ai.summarize"}
	// 前缀匹配：f.Action = "ai." 匹配 ai.summarize
	if !auditMatches(evt, AuditFilter{Action: "ai."}) {
		t.Fatal("ai. 前缀应匹配 ai.summarize")
	}
	// 精确匹配：f.Action = "ai.summarize" 匹配
	if !auditMatches(evt, AuditFilter{Action: "ai.summarize"}) {
		t.Fatal("精确 ai.summarize 应匹配")
	}
	// 精确不匹配：f.Action = "ai.tag" 不匹配 ai.summarize
	if auditMatches(evt, AuditFilter{Action: "ai.tag"}) {
		t.Fatal("ai.tag 不应匹配 ai.summarize")
	}
	// 空 action = 全部（既有语义不变）
	if !auditMatches(evt, AuditFilter{}) {
		t.Fatal("空 action 应匹配全部")
	}
	// 其它 action 不带点 → 精确匹配（既有语义不变）
	if auditMatches(AuditEvent{Action: "delete"}, AuditFilter{Action: "dele"}) {
		t.Fatal("delete 不应被 dele 前缀匹配")
	}
}

// TestAuditMatches_Time 验证 Since 过滤不变。
func TestAuditMatches_Time(t *testing.T) {
	t.Parallel()
	evt := AuditEvent{Action: "upload", TS: time.Now()}
	if auditMatches(evt, AuditFilter{Action: "upload", Since: time.Now().Add(time.Hour)}) {
		t.Fatal("未来 Since 不应匹配")
	}
}
