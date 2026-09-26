// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/llmgate"
)

func TestAIInsightHandler_Disabled(t *testing.T) {
	t.Parallel()
	// gate=nil → 端点 400「AI 洞察未启用」（零回归：未装配行为明确）
	h := &Handlers{}
	ai := &AIInsight{gate: nil}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/summarize?filename=a.txt", nil)
	rec := httptest.NewRecorder()
	h.handleAISummarize(rec, req, "ownerA", ai)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未启用应 400: %d", rec.Code)
	}
}

func TestAIInsightHandler_MissingFilename(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	ai := &AIInsight{gate: &llmgate.Client{}}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/summarize", nil)
	rec := httptest.NewRecorder()
	h.handleAISummarize(rec, req, "ownerA", ai)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 filename 应 400: %d", rec.Code)
	}
}

func TestAIInsightHandler_PathTraversal(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	ai := &AIInsight{gate: &llmgate.Client{}}
	req := httptest.NewRequest(http.MethodPost, "/api/ai/summarize?filename=../etc/passwd", nil)
	rec := httptest.NewRecorder()
	h.handleAISummarize(rec, req, "ownerA", ai)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("路径穿越应 400: %d", rec.Code)
	}
}
