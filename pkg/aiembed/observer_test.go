// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package aiembed

// observer_test.go 锁定 `Config.Observer` 的**契约**（pkg 层，不依赖 pkg/server）：
//   - 成功嵌入后**调用一次**，参数是 (provider, model, 条数)；
//   - 参数里**没有文本**（签名本身就不含 ⇒ 编译期保证），调用方无法把用户内容带进审计；
//   - 失败（HTTP 非 200 / 解析失败）**不调用**（失败留痕由调用方按自身语义决定）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestObserver_ReportsMetadataOnly 成功路径：一次调用、参数为元数据、与文本无关。
func TestObserver_ReportsMetadataOnly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1,0,0]},{"embedding":[0,1,0]}]}`))
	}))
	defer srv.Close()

	type call struct {
		provider, model string
		count           int
	}
	var calls []call
	c := New(Config{
		Provider: "openai", BaseURL: srv.URL, Model: "m1", Timeout: 5 * time.Second,
		Observer: func(_ context.Context, provider, model string, count int) {
			calls = append(calls, call{provider, model, count})
		},
	})

	if _, err := c.EmbedBatch(context.Background(), []string{"秘密 A", "秘密 B"}); err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("Observer 应被调用 1 次, got %d", len(calls))
	}
	if calls[0].provider != "openai" || calls[0].model != "m1" || calls[0].count != 2 {
		t.Fatalf("Observer 参数不符: %+v", calls[0])
	}
}

// TestObserver_NotCalledOnFailure 失败路径不调用（HTTP 500）。
func TestObserver_NotCalledOnFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	called := false
	c := New(Config{
		Provider: "openai", BaseURL: srv.URL, Model: "m1", Timeout: 5 * time.Second,
		Observer: func(context.Context, string, string, int) { called = true },
	})
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("HTTP 500 应返回错误")
	}
	if called {
		t.Error("失败时不应调用 Observer（失败留痕由调用方决定）")
	}
}
