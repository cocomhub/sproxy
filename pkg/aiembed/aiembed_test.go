// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aiembed

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestEmbed_OpenAI 验证 OpenAI 兼容 /v1/embeddings 解析（data[i].embedding）。
func TestEmbed_OpenAI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %s, want /embeddings", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("auth = %q, want Bearer sk-test", got)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float64{0.1, 0.2, 0.3}},
			},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "openai", BaseURL: srv.URL, APIKey: "sk-test", Model: "text-embedding-3-small", Timeout: 5 * time.Second})
	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	// [0.1 0.2 0.3] 归一化 → 长度 1（余弦不变）。
	if len(vec) != 3 {
		t.Fatalf("len = %d, want 3", len(vec))
	}
	var sum float64
	for _, f := range vec {
		sum += float64(f) * float64(f)
	}
	if sum < 0.99 || sum > 1.01 {
		t.Fatalf("归一化长度 = %f, want 1.0", sum)
	}
}

// TestEmbedBatch_Ollama 验证 Ollama /api/embed 数组解析 + 归一化。
func TestEmbedBatch_Ollama(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/embed" {
			t.Errorf("path = %s, want /api/embed", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float64{{3, 4}, {0, 5}},
		})
	}))
	defer srv.Close()
	c := New(Config{Provider: "ollama", BaseURL: srv.URL, Model: "nomic-embed-text", Timeout: 5 * time.Second})
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 2 {
		t.Fatalf("len = %d, want 2", len(vecs))
	}
	// [3 4] 归一化 → [0.6 0.8]
	if vecs[0][0] < 0.599 || vecs[0][0] > 0.601 || vecs[0][1] < 0.799 || vecs[0][1] > 0.801 {
		t.Fatalf("vec0 = %v, want [0.6 0.8]", vecs[0])
	}
}

// TestEmbed_Non2xx 验证非 2xx → error（fail-closed）。
func TestEmbed_Non2xx(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := New(Config{Provider: "openai", BaseURL: srv.URL, APIKey: "k", Timeout: 5 * time.Second})
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("非 2xx 应 error")
	}
}

// TestEmbed_EmptyInput 验证空 input → error。
func TestEmbed_EmptyInput(t *testing.T) {
	t.Parallel()
	c := New(Config{Provider: "openai", Timeout: 5 * time.Second})
	if _, err := c.Embed(context.Background(), ""); err == nil {
		t.Fatal("空 input 应 error")
	}
}
