// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ai

// advisor_test.go 验证告警建议器（AIAdvisor）：
//  1. 未启用/无 key → gate=nil（恒空，零回归）；
//  2. 网关成功 → 建议文本（system/user 提示正确送达）；
//  3. 网关 500 / 空响应 / 超时 → ""（fail-closed 回退固定模板）；
//  4. 超长建议 → 截断 ≤ maxAdviseRunes；
//  5. prompt 构造与 AppendAdvice 拼接语义。

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
)

// testAILogger 返回丢弃输出的测试 logger（与既有包内测试同口径）。
func testAILogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// chatCompletionBody 构造 OpenAI 兼容成功响应体。
func chatCompletionBody(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []map[string]any{{
			"message": map[string]any{"role": "assistant", "content": content},
		}},
	})
	return string(b)
}

// chatRequestPayload 是断言提示送达用的请求体视图。
type chatRequestPayload struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// newAdvisorMockServer 起 mock 网关，返回服务器与请求体捕获通道。
func newAdvisorMockServer(t *testing.T, status int, content string) (*httptest.Server, chan chatRequestPayload) {
	t.Helper()
	reqs := make(chan chatRequestPayload, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload chatRequestPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		reqs <- payload
		if status != http.StatusOK {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(chatCompletionBody(content)))
	}))
	t.Cleanup(srv.Close)
	return srv, reqs
}

// TestAIAdvisor_Disabled 未启用 → gate=nil、Advise 恒空。
func TestAIAdvisor_Disabled(t *testing.T) {
	t.Parallel()
	a := NewAIAdvisor(config.AIAdvisorConfig{}, testAILogger())
	if a.Enabled() {
		t.Fatal("enabled=false 不应装配网关")
	}
	if got := a.Advise(context.Background(), "disk", "磁盘告警", "正文"); got != "" {
		t.Fatalf("未启用应恒空，got %q", got)
	}
}

// TestAIAdvisor_EnabledWithoutAPIKey 启用但 key 为空 → gate=nil（fail-closed）。
func TestAIAdvisor_EnabledWithoutAPIKey(t *testing.T) {
	t.Parallel()
	a := NewAIAdvisor(config.AIAdvisorConfig{Enabled: true}, nil) // nil logger → 默认 logger
	if a.Enabled() {
		t.Fatal("无 api_key 不应装配网关（fail-closed）")
	}
	if got := a.Advise(context.Background(), "k", "t", "x"); got != "" {
		t.Fatalf("无 key 应恒空，got %q", got)
	}
}

// TestAIAdvisor_AdviseSuccess 网关成功 → 返回建议正文，提示按 system/user 两段送达。
func TestAIAdvisor_AdviseSuccess(t *testing.T) {
	t.Parallel()
	srv, reqs := newAdvisorMockServer(t, http.StatusOK, "  建议正文\n")
	a := NewAIAdvisor(config.AIAdvisorConfig{
		Enabled: true,
		BaseURL: srv.URL,
		APIKey:  "test-key",
		Model:   "test-model",
		Timeout: 2 * time.Second,
	}, testAILogger())
	if !a.Enabled() {
		t.Fatal("enabled + key 应装配网关")
	}
	got := a.Advise(context.Background(), "disk", "磁盘告警", "剩余 3%")
	if got != "建议正文" {
		t.Fatalf("建议正文不符: %q", got)
	}
	select {
	case payload := <-reqs:
		if payload.Model != "test-model" {
			t.Fatalf("model 应为 test-model, got %q", payload.Model)
		}
		if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" || payload.Messages[1].Role != "user" {
			t.Fatalf("messages 应为 [system,user]: %+v", payload.Messages)
		}
		if payload.Messages[0].Content != promptSystem {
			t.Fatalf("system 提示不符: %q", payload.Messages[0].Content)
		}
		wantUser := buildUserPrompt("disk", "磁盘告警", "剩余 3%")
		if payload.Messages[1].Content != wantUser {
			t.Fatalf("user 提示不符: %q != %q", payload.Messages[1].Content, wantUser)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("mock 网关未收到请求")
	}
}

// TestAIAdvisor_AdviseGatewayError 网关 500 → ""（fail-closed，不静默编造建议）。
func TestAIAdvisor_AdviseGatewayError(t *testing.T) {
	t.Parallel()
	srv, _ := newAdvisorMockServer(t, http.StatusInternalServerError, "")
	a := NewAIAdvisor(config.AIAdvisorConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k", Timeout: 2 * time.Second,
	}, testAILogger())
	if got := a.Advise(context.Background(), "k", "t", "x"); got != "" {
		t.Fatalf("网关失败应回退空串，got %q", got)
	}
}

// TestAIAdvisor_AdviseEmptyResponse 网关 200 + 空 content → ""。
func TestAIAdvisor_AdviseEmptyResponse(t *testing.T) {
	t.Parallel()
	srv, _ := newAdvisorMockServer(t, http.StatusOK, "")
	a := NewAIAdvisor(config.AIAdvisorConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k", Timeout: 2 * time.Second,
	}, testAILogger())
	if got := a.Advise(context.Background(), "k", "t", "x"); got != "" {
		t.Fatalf("空响应应返回空串，got %q", got)
	}
}

// TestAIAdvisor_AdviseTruncates 超长建议 → 截断到 maxAdviseRunes 个字符。
func TestAIAdvisor_AdviseTruncates(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("建", maxAdviseRunes+200)
	srv, _ := newAdvisorMockServer(t, http.StatusOK, long)
	a := NewAIAdvisor(config.AIAdvisorConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k", Timeout: 2 * time.Second,
	}, testAILogger())
	got := a.Advise(context.Background(), "k", "t", "x")
	if got != strings.Repeat("建", maxAdviseRunes) {
		t.Fatalf("建议应截断到 %d 字符，got %d", maxAdviseRunes, len([]rune(got)))
	}
}

// TestAIAdvisor_AdviseTimeout 慢网关 + 短 Timeout → ""（超时降级）。
func TestAIAdvisor_AdviseTimeout(t *testing.T) {
	t.Parallel()
	// handler 阻塞到测试收尾显式放开（避免固定 sleep + 避免 Close 卡在途连接）。
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	}))
	t.Cleanup(func() {
		close(block)
		srv.Close()
	})
	a := NewAIAdvisor(config.AIAdvisorConfig{
		Enabled: true, BaseURL: srv.URL, APIKey: "k", Timeout: 50 * time.Millisecond,
	}, testAILogger())
	start := time.Now()
	if got := a.Advise(context.Background(), "k", "t", "x"); got != "" {
		t.Fatalf("超时应回退空串，got %q", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("超时未生效，耗时 %v", elapsed)
	}
}

// TestBuildUserPrompt 纯函数：告警来源/标题/正文按行送入。
func TestBuildUserPrompt(t *testing.T) {
	t.Parallel()
	got := buildUserPrompt("disk", "磁盘告警", "剩余 3%")
	for _, want := range []string{"告警来源: disk", "标题: 磁盘告警", "剩余 3%"} {
		if !strings.Contains(got, want) {
			t.Fatalf("user 提示应含 %q: %q", want, got)
		}
	}
}

// TestAppendAdvice 空建议原样返回；非空建议按固定段标题追加。
func TestAppendAdvice(t *testing.T) {
	t.Parallel()
	if got := AppendAdvice("模板正文", ""); got != "模板正文" {
		t.Fatalf("空建议不应改变正文: %q", got)
	}
	got := AppendAdvice("模板正文", "建议")
	if want := "模板正文" + aiAdviseSuffix + "建议"; got != want {
		t.Fatalf("追加格式不符: %q != %q", got, want)
	}
	if !strings.Contains(aiAdviseSuffix, "AI 建议") {
		t.Fatalf("建议段标题应含「AI 建议」: %q", aiAdviseSuffix)
	}
}

// TestAIAdvisor_EnabledNilReceiver nil 建议器恒未启用（Enabled 是 nil 安全查询）。
func TestAIAdvisor_EnabledNilReceiver(t *testing.T) {
	t.Parallel()
	var a *AIAdvisor
	if a.Enabled() {
		t.Fatal("nil advisor 应报告未启用")
	}
}
