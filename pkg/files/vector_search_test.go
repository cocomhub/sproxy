// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSemanticSearch_TopK_ScoreOrder 验证余弦 top-k 排序（假向量）。
func TestSemanticSearch_TopK_ScoreOrder(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	vs := NewVectorStore("")
	// q = [1,0]；a 同向分最高，b 垂直分 0，c 反向分最低。
	vs.Put("ownerA", "a.txt", []float32{1, 0}, 10, "alpha")
	vs.Put("ownerA", "b.txt", []float32{0, 1}, 20, "beta")
	vs.Put("ownerA", "c.txt", []float32{-1, 0}, 30, "gamma")
	svc.vector = vs
	res, err := svc.SemanticSearch(SearchQuery{Owner: "ownerA", Query: "query"}, []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	if len(res.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(res.Results))
	}
	if res.Results[0].File.Name != "a.txt" || res.Results[0].Score < 0.99 {
		t.Fatalf("top1 = %+v, want a.txt ~1.0", res.Results[0])
	}
	if res.Results[2].File.Name != "c.txt" {
		t.Fatalf("top3 = %+v, want c.txt（反向最低）", res.Results[2])
	}
}

// TestSemanticSearch_OwnerIsolation 验证跨 owner 不可见。
func TestSemanticSearch_OwnerIsolation(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	vs := NewVectorStore("")
	vs.Put("ownerA", "a.txt", []float32{1, 0}, 10, "alpha")
	vs.Put("ownerB", "secret.txt", []float32{1, 0}, 20, "secret")
	svc.vector = vs
	res, err := svc.SemanticSearch(SearchQuery{Owner: "ownerA", Query: "q"}, []float32{1, 0}, 10)
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	for _, r := range res.Results {
		if strings.Contains(r.File.Name, "secret") {
			t.Fatalf("跨 owner 泄漏: %+v", r.File)
		}
	}
}

// TestSemanticSearch_NotConfigured 验证未装配 → errNoVector。
func TestSemanticSearch_NotConfigured(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	_, err := svc.SemanticSearch(SearchQuery{Owner: "o", Query: "q"}, []float32{1}, 10)
	if err == nil || err != errNoVector {
		t.Fatalf("err = %v, want errNoVector", err)
	}
}

// TestSemanticSearchHTTP_KeywordFallback 验证端点：未装配 → 404。
func TestSemanticSearchHTTP_NotConfigured404(t *testing.T) {
	t.Parallel()
	svc := &Service{}
	req := httptest.NewRequest(http.MethodGet, "/api/search/semantic?q=hello", nil)
	rec := httptest.NewRecorder()
	svc.SemanticSearchHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}
