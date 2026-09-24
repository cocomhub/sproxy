// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_advisor_test.go 验证告警建议器接线（roadmap 11.9-⑥ 智能运维 LLM）：
//  1. Enabled + key：dispatch 后通知文本含 "【AI 建议】"+建议（mock 渠道断言）。
//  2. No key：enabled=true 但 key 解析为空 → gate=nil → 固定模板原样。
//  3. 网关失败（500）：通知仍发出（固定模板）+ 不吞告警（fail-closed）。
//  4. Disabled：enabled=false → 文本与未装配 AI 时逐字节一致。
//  5. DispatchCallsAdvisor：注入 advisor 桩，fire 恰好调用一次 Advise。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newAIAdvisorForTest 构造 AIAdvisor（mock 网关 URL 注入 BaseURL）。
func newAIAdvisorForTest(t *testing.T, baseURL, apiKey string) *AIAdvisor {
	t.Helper()
	if baseURL == "" {
		return NewAIAdvisor(AIAdvisorConfig{Enabled: true}, testLogger())
	}
	return NewAIAdvisor(AIAdvisorConfig{
		Enabled: true,
		BaseURL: baseURL,
		APIKey:  apiKey,
		Model:   "test-model",
		Timeout: 2 * time.Second,
	}, testLogger())
}

// aiChatReq 是 mock 网关解码请求体的最小形状。
type aiChatReq struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

// mockAICompletions 起 OpenAI 兼容 /chat/completions mock（返回固定建议）。
func mockAICompletions(t *testing.T, advise func(aiChatReq) string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req aiChatReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		content := advise(req)
		_, _ = fmt.Fprintf(w, `{"choices":[{"message":{"role":"assistant","content":%q}}]}`, content)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// httptestNewServer500 起恒 500 的 mock 网关。
func httptestNewServer500(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newAlertEngineWithAI 构造带 AI 建议器的告警引擎（磁盘水位规则 + mock 网关）。
func newAlertEngineWithAI(t *testing.T, baseURL, apiKey string) *AlertEngine {
	t.Helper()
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules:   []AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.SetAdvisor(newAIAdvisorForTest(t, baseURL, apiKey))
	eng.SetDiskUsageReader(func() (used, cap int64) { return 850, 1000 })
	return eng
}

// notifyTextForTest 触发一次磁盘告警并返回渠道收到的正文（advisor 注入可变）。
func notifyTextForTest(t *testing.T, advisor *AIAdvisor) string {
	t.Helper()
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules:   []AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	if advisor != nil {
		eng.SetAdvisor(advisor)
	}
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetDiskUsageReader(func() (used, cap int64) { return 850, 1000 })

	eng.checkDiskWatermark(context.Background())
	select {
	case m := <-ch.msgs:
		_, text, _ := strings.Cut(m, "|")
		return text
	case <-time.After(3 * time.Second):
		t.Fatal("磁盘告警应即时通知")
		return ""
	}
}

// countingAdvisor 是 Advise 调用计数桩。
type countingAdvisor struct {
	advise func(ctx context.Context, key, title, text string) string
}

func (c *countingAdvisor) Advise(ctx context.Context, key, title, text string) string {
	return c.advise(ctx, key, title, text)
}

// TestAIAdvisor_EnabledAppendsAdvice Enabled + mock 网关 → 通知文本含 AI 段。
func TestAIAdvisor_EnabledAppendsAdvice(t *testing.T) {
	t.Parallel()
	srv := mockAICompletions(t, func(r aiChatReq) string {
		return "建议：检查磁盘使用率，扩容或清理大文件。"
	})

	ch := newFakeAlertChannel("wecom")
	eng := newAlertEngineWithAI(t, srv.URL, "sk-test")
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.checkDiskWatermark(context.Background())
	select {
	case m := <-ch.msgs:
		if !strings.Contains(m, "【AI 建议】") {
			t.Fatalf("通知应含 AI 建议段: %s", m)
		}
		if !strings.Contains(m, "建议：检查磁盘使用率") {
			t.Fatalf("通知应含 LLM 建议文本: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("磁盘告警应即时通知")
	}
}

// TestAIAdvisor_NoKeyFallsBackTemplate enabled=true 但 key 空 → gate=nil → 模板原样。
func TestAIAdvisor_NoKeyFallsBackTemplate(t *testing.T) {
	t.Parallel()
	// newAIAdvisorFromConfig 装配路径：APIKeyRef 指向未设环境变量 → Warn + nil（可观测降级）。
	// t.Setenv 禁并行，用直接构造等价断言 gate=nil 路径。
	advisor := NewAIAdvisor(AIAdvisorConfig{Enabled: true, APIKey: ""}, testLogger())
	if advisor.gate != nil {
		t.Fatal("无 key 时 gate 应为 nil（fail-closed 回退模板）")
	}
	if got := advisor.Advise(context.Background(), "k", "t", "磁盘水位 85% ≥ 阈值 80%"); got != "" {
		t.Fatalf("无 key 时 Advise 应返回空串，got %q", got)
	}

	// 全链路：enabled=true 无 key → 通知文本无 AI 段。
	ch := newFakeAlertChannel("wecom")
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules:   []AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.SetAdvisor(NewAIAdvisor(AIAdvisorConfig{Enabled: true}, testLogger()))
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetDiskUsageReader(func() (used, cap int64) { return 850, 1000 })

	eng.checkDiskWatermark(context.Background())
	select {
	case m := <-ch.msgs:
		if strings.Contains(m, "【AI 建议】") {
			t.Fatalf("无 key 时通知不应含 AI 段: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("无 key 时通知仍应发出（固定模板）")
	}
}

// TestAIAdvisor_GatewayFailureStillNotifies mock 网关 500 → 通知仍发出（模板）+ 不吞告警。
func TestAIAdvisor_GatewayFailureStillNotifies(t *testing.T) {
	t.Parallel()
	srv := httptestNewServer500(t)

	ch := newFakeAlertChannel("wecom")
	eng := newAlertEngineWithAI(t, srv.URL, "sk-test")
	eng.Register(ch)
	t.Cleanup(eng.Close)

	eng.checkDiskWatermark(context.Background())
	select {
	case m := <-ch.msgs:
		if strings.Contains(m, "【AI 建议】") {
			t.Fatalf("网关失败时通知不应含 AI 段: %s", m)
		}
		if !strings.Contains(m, "磁盘水位") {
			t.Fatalf("固定模板应照发: %s", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("网关失败不应吞掉告警（fail-closed）")
	}
}

// TestAIAdvisor_DisabledZeroRegression enabled=false → 与未装配 AI 完全一致。
func TestAIAdvisor_DisabledZeroRegression(t *testing.T) {
	t.Parallel()
	baseline := notifyTextForTest(t, nil)
	withDisabled := notifyTextForTest(t, NewAIAdvisor(AIAdvisorConfig{Enabled: false}, testLogger()))
	if baseline != withDisabled {
		t.Fatalf("enabled=false 应逐字节等于未装配 AI:\n---\n%s\n---\n%s", baseline, withDisabled)
	}
}

// TestAlertEngine_DispatchCallsAdvisor 注入 advisor 桩：fire 恰好调用一次 Advise。
func TestAlertEngine_DispatchCallsAdvisor(t *testing.T) {
	t.Parallel()
	var calls int
	eng := NewAlertEngine(AlertConfig{
		Enabled: true,
		Rules:   []AlertRule{{Source: "disk_watermark", Threshold: 80, Channels: []string{"wecom"}}},
	}, testLogger())
	eng.SetAdvisor(&countingAdvisor{advise: func(ctx context.Context, key, title, text string) string {
		calls++
		return "建议"
	}})
	ch := newFakeAlertChannel("wecom")
	eng.Register(ch)
	t.Cleanup(eng.Close)
	eng.SetDiskUsageReader(func() (used, cap int64) { return 850, 1000 })

	eng.checkDiskWatermark(context.Background())
	select {
	case <-ch.msgs:
	case <-time.After(3 * time.Second):
		t.Fatal("应发出告警")
	}
	if calls != 1 {
		t.Fatalf("fire 应恰好调用一次 Advise，got %d", calls)
	}
	// 同水位再触发 → 去抖，不再次调用。
	eng.checkDiskWatermark(context.Background())
	select {
	case <-ch.msgs:
		t.Fatal("同水位二次触发应去抖")
	case <-time.After(100 * time.Millisecond):
	}
	if calls != 1 {
		t.Fatalf("去抖后不应再调用 Advise，got %d", calls)
	}
}

// TestAIAdvisor_ConfigValidate api_key_ref 空 + enabled=true → Validate 允许（运行期降级）。
func TestAIAdvisor_ConfigValidate(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Notify.Enabled = true
	cfg.Notify.AIAdvisor.Enabled = true
	cfg.Notify.AIAdvisor.APIKeyRef = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("api_key_ref 空 + enabled=true 应允许（运行期降级），got %v", err)
	}
	// 非法环境变量名拒绝（防脚枪）。
	cfg.Notify.AIAdvisor.APIKeyRef = "SPROXY AI KEY"
	if err := cfg.Validate(); err == nil {
		t.Fatal("含空格的 api_key_ref 应拒绝")
	}
	cfg.Notify.AIAdvisor.APIKeyRef = "SPROXY_OPENAI_API_KEY"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法 api_key_ref 应允许: %v", err)
	}
}
