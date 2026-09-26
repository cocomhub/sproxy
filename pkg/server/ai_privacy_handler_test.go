// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestAIPrivacyEndpoint_Disabled 验证 enabled=false → 400 零回归。
func TestAIPrivacyEndpoint_Disabled(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	req := httptest.NewRequest(http.MethodGet, "/api/ai/privacy", nil)
	rec := httptest.NewRecorder()
	h.handleAIPrivacy(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("disabled code = %d, want 400", rec.Code)
	}
}

// TestAIPrivacyEndpoint_List 验证启用 + 加密卷 → 200 清单。
func TestAIPrivacyEndpoint_List(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	h := &Handlers{}
	h.aiPrivacy = NewAIPrivacy(true, rt, slog.Default())
	_ = h.aiPrivacy.StoreAI(rt, "vectors", "anonymous", "a.txt", []byte("v1"))
	req := httptest.NewRequest(http.MethodGet, "/api/ai/privacy", nil)
	req = req.WithContext(withActor(req.Context(), ""))
	rec := httptest.NewRecorder()
	h.handleAIPrivacy(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "vectors") || !contains(rec.Body.String(), "a.txt") {
		t.Fatalf("响应缺字段: %s", rec.Body.String())
	}
}

// TestAIPrivacyEndpoint_Purge 验证 purge 删除 + 审计。
func TestAIPrivacyEndpoint_Purge(t *testing.T) {
	t.Parallel()
	rt := encTestRoot(t)
	h := &Handlers{}
	h.auditRing = NewAuditRing(100)
	h.aiPrivacy = NewAIPrivacy(true, rt, slog.Default())
	_ = h.aiPrivacy.StoreAI(rt, "vectors", "anonymous", "a.txt", []byte("v1"))
	req := httptest.NewRequest(http.MethodPost, "/api/ai/privacy/purge", nil)
	req = req.WithContext(withActor(req.Context(), ""))
	rec := httptest.NewRecorder()
	h.handleAIPrivacyPurge(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	if !contains(rec.Body.String(), "deleted") {
		t.Fatalf("响应缺 deleted: %s", rec.Body.String())
	}
	evts := h.auditRing.Recent(10, AuditFilter{})
	if len(evts) != 1 || evts[0].Action != "ai.privacy_purge" {
		t.Fatalf("应记 1 条 ai.privacy_purge: %+v", evts)
	}
	if got := h.aiPrivacy.List(rt, "anonymous"); len(got) != 0 {
		t.Fatalf("purge 后应空: %+v", got)
	}
}
