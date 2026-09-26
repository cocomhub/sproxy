// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAIQuotaEndpoint 验证 /api/ai/quota：未启用 400 + 启用返回用量。
func TestAIQuotaEndpoint(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	// 未启用 → 400
	req := httptest.NewRequest(http.MethodGet, "/api/ai/quota", nil)
	rec := httptest.NewRecorder()
	h.handleAIQuota(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("未启用 code = %d, want 400", rec.Code)
	}
	// 启用 → 200 + usage
	h.aiInsight = &AIInsight{quota: NewAIQuota(AIQuotaConfig{Enabled: true, Default: AILimit{DailyCalls: 5}}, slog.Default())}
	_ = h.aiInsight.quota.CheckAndCharge("ownerA", 10)
	req2 := httptest.NewRequest(http.MethodGet, "/api/ai/quota", nil)
	req2 = req2.WithContext(withActor(req2.Context(), "ownerA"))
	rec2 := httptest.NewRecorder()
	h.handleAIQuota(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("启用 code = %d, want 200", rec2.Code)
	}
	body := rec2.Body.String()
	if !contains(body, "ownerA") || !contains(body, "calls") {
		t.Fatalf("quota 响应缺字段: %s", body)
	}
}

// contains 是子串助手。
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
