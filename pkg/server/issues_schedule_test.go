// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// TestIssueCenter_NilSafePaths 锁定：未装配时各入口都安全返回（零回归）。
func TestIssueCenter_NilSafePaths(t *testing.T) {
	t.Parallel()
	var h *Handlers
	if h.IssueCenter() != nil {
		t.Fatal("nil Handlers 的 IssueCenter 应为 nil")
	}
	if err := h.RegisterIssuesRetryTask(nil); err != nil {
		t.Fatalf("nil Handlers 注册应返回 nil（未启用），got %v", err)
	}

	// 有 Handlers 但未装配 issuesAPI ⇒ 同样安全
	h2 := &Handlers{}
	if h2.IssueCenter() != nil {
		t.Fatal("未装配 issuesAPI 时 IssueCenter 应为 nil")
	}
	if err := h2.RegisterIssuesRetryTask(nil); err != nil {
		t.Fatalf("未装配时应返回 nil，got %v", err)
	}
}

// TestRegisterIssuesRetryTask_NoSchedulerNoop 锁定：调度器未装配 ⇒ no-op（不报错、不 panics）。
func TestRegisterIssuesRetryTask_NoSchedulerNoop(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	h.issuesAPI = NewIssuesAPI(NewIssueCenter(""))
	if err := h.RegisterIssuesRetryTask(map[issues.Kind]issues.RetryFunc{}); err != nil {
		t.Fatalf("无调度器应 no-op，got %v", err)
	}
	if h.IssueCenter() == nil {
		t.Fatal("已装配 issuesAPI 时 IssueCenter 不应为 nil")
	}
}
