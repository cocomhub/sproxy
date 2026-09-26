// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestSemanticSearchHTTP_SemanticMode 验证 ctx 注入向量 → 语义命中 + mode=semantic。
func TestSemanticSearchHTTP_SemanticMode(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	vs := NewVectorStore("")
	vs.Put("ownerA", "report.txt", []float32{1, 0}, 10, "alpha")
	svc.vector = vs
	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=query&topk=5", nil)
	ctx := context.WithValue(req.Context(), semanticQueryVecKey{}, []float32{1, 0})
	ctx = context.WithValue(ctx, semanticOwnerKey{}, "ownerA")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	svc.SemanticSearchHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var res SemanticResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Mode != "semantic" {
		t.Fatalf("mode = %s, want semantic", res.Mode)
	}
	if len(res.Results) != 1 || res.Results[0].File.Name != "report.txt" {
		t.Fatalf("results = %+v, want [report.txt]", res.Results)
	}
}

// TestSemanticSearchHTTP_KeywordFallback 验证未注入向量 → 关键词回退 + mode=keyword。
func TestSemanticSearchHTTP_KeywordFallback(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	vs := NewVectorStore("")
	vs.Put("ownerA", "report.txt", []float32{1, 0}, 10, "alpha")
	svc.vector = vs
	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=nonexistent", nil)
	rec := httptest.NewRecorder()
	svc.SemanticSearchHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200", rec.Code)
	}
	var res SemanticResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Mode != "keyword" {
		t.Fatalf("mode = %s, want keyword（回退可观测）", res.Mode)
	}
}

// TestSemanticSearchHTTP_EmptyQ 验证 q 空 → 400。
func TestSemanticSearchHTTP_EmptyQ(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	svc.vector = NewVectorStore("")
	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic", nil)
	rec := httptest.NewRecorder()
	svc.SemanticSearchHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}
