// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package llmgate

// ai_test.go 验证 LLM 网关客户端（roadmap 11.9-⑥ 智能运维 LLM）：
//  1. 200 正常解析 content（trim）。
//  2. 非 2xx → error（fail-closed）。
//  3. 畸形 JSON → error。
//  4. 超大响应 → 截断（LimitReader 生效，error）。
//  5. 超时（慢桩 + 短 Timeout）→ error。
//  6. 空 content → 空串（无 error）。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chatRespOK 构造 OpenAI 兼容成功响应。
func chatRespOK(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})
	return string(b)
}

// TestLLMGate_AdviseSuccess 200 正常解析 content（trim 空白）。
func TestLLMGate_AdviseSuccess(t *testing.T) {
	t.Parallel()
	var gotPath, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatRespOK("  建议文本\n")))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Model: "m1", Timeout: 2 * time.Second})
	got, err := c.Advise(context.Background(), "system", "user")
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if got != "建议文本" {
		t.Fatalf("content 应 trim 为「建议文本」，got %q", got)
	}
	if gotPath != "/chat/completions" {
		t.Fatalf("请求路径应为 /chat/completions，got %q", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Fatalf("Authorization 应为 Bearer sk-test，got %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("Content-Type 应为 application/json，got %q", gotCT)
	}
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("请求体 JSON 非法: %v", err)
	}
	if req.Model != "m1" {
		t.Fatalf("请求 model 应为 m1，got %q", req.Model)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("请求 messages 应为 [system,user]，got %+v", req.Messages)
	}
	if req.Messages[0].Content != "system" || req.Messages[1].Content != "user" {
		t.Fatalf("请求 messages 内容不符: %+v", req.Messages)
	}
}

// TestLLMGate_Non2xxError 非 2xx → error（fail-closed）。
func TestLLMGate_Non2xxError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"boom"}`))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 2 * time.Second})
	if _, err := c.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("500 应返回 error")
	}
}

// TestLLMGate_MalformedJSON 畸形 JSON → error。
func TestLLMGate_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`not-json`))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 2 * time.Second})
	if _, err := c.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("畸形 JSON 应返回 error")
	}
}

// TestLLMGate_OversizeResponse 超大响应 → 截断（LimitReader 生效 → error，防撑爆内存）。
func TestLLMGate_OversizeResponse(t *testing.T) {
	t.Parallel()
	huge := strings.Repeat("a", maxResponseBytes*3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatRespOK(huge)))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 5 * time.Second})
	if _, err := c.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("超大响应应触发 LimitReader 截断并返回 error")
	}
}

// TestLLMGate_Timeout 慢桩 + 短 Timeout → 超时 error。
func TestLLMGate_Timeout(t *testing.T) {
	t.Parallel()
	// handler 阻塞直到测试收尾显式放开——避免固定 time.Sleep（R14 棘轮）且
	// 保证 httptest.Server.Close() 不因在途连接卡住。
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 50 * time.Millisecond})
	if _, err := c.Advise(context.Background(), "s", "u"); err == nil {
		t.Fatal("慢桩应触发超时 error")
	}
}

// TestLLMGate_EmptyContent 空 content → 空串（无 error）。
func TestLLMGate_EmptyContent(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatRespOK("")))
	}))
	t.Cleanup(srv.Close)

	c := New(Config{BaseURL: srv.URL, APIKey: "sk-test", Timeout: 2 * time.Second})
	got, err := c.Advise(context.Background(), "s", "u")
	if err != nil {
		t.Fatalf("空 content 不应报错: %v", err)
	}
	if got != "" {
		t.Fatalf("空 content 应返回空串，got %q", got)
	}
}

// TestLLMGate_DefaultModelAndURL Provider 空 → openai 默认 URL/模型。
func TestLLMGate_DefaultModelAndURL(t *testing.T) {
	t.Parallel()
	c := New(Config{APIKey: "k"})
	if c.cfg.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("默认 BaseURL 应为 openai v1，got %q", c.cfg.BaseURL)
	}
	if c.cfg.Model == "" {
		t.Fatal("默认 Model 不应为空")
	}
}
