// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_insight_handler_paths_test.go 钉住 /api/ai/summarize 与 /api/ai/tag 的**完整分支面**：
// 未启用 / 缺参 / 路径穿越 / 非文本 / 不存在 / 缓存命中 / 配额超限 / 网关失败 / 成功落缓存。
//
// 注：装配真实 AIInsight 需要 api_key_ref 环境变量（生产契约），故本用例用 t.Setenv，
// 按 R18 豁免 ① 串行（不进 serial_budgets.tsv；不写 t.Parallel——环境变量是进程级状态）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/llmgate"
	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// aiTestEnv 构造「真实租户文件 + 假 LLM 网关」的洞察装配。
func aiTestEnv(t *testing.T) (*Handlers, *AIInsight, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"alpha,beta"}}]}`))
	}))
	t.Cleanup(srv.Close)

	t.Setenv("SPROXY_TEST_AI_KEY", "test-key")
	ai := NewAIInsightFromConfig(config.AIInsightConfig{
		Enabled:   true,
		Provider:  "openai",
		BaseURL:   srv.URL,
		APIKeyRef: "SPROXY_TEST_AI_KEY",
		Model:     "test-model",
		Timeout:   5 * time.Second,
	}, testutil.MkdirTemp(t, "ai-cache-"), testLogger())
	if ai == nil {
		t.Fatal("AIInsight 装配失败（enabled + key 应返回实例）")
	}

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot}}
	h := buildVolSetHandlers(t, cfg)

	tnt := h.tenants.TenantFor("anonymous")
	if tnt == nil {
		t.Fatal("anonymous 租户应可用")
	}
	if err := tnt.Root().MkdirAll("user", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeTenantFile(t, h, "user/text.txt", "这是一段用于摘要抽样的文本。")
	writeTenantFile(t, h, "user/empty.txt", "")
	return h, ai, &calls
}

// writeTenantFile 经 storage.Root 写用户文件（R25：测试也不直连卷根 os 路径）。
func writeTenantFile(t *testing.T, h *Handlers, rel, content string) {
	t.Helper()
	root := h.tenants.TenantFor("anonymous").Root()
	if dir := path.Dir(rel); dir != "." && dir != "" {
		if merr := root.MkdirAll(dir, 0o755); merr != nil {
			t.Fatalf("MkdirAll(%s): %v", dir, merr)
		}
	}
	f, err := root.OpenFile(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		t.Fatalf("OpenFile(%s): %v", rel, err)
	}
	if _, werr := f.WriteString(content); werr != nil {
		_ = f.Close()
		t.Fatalf("WriteString(%s): %v", rel, werr)
	}
	if cerr := f.Close(); cerr != nil {
		t.Fatalf("Close(%s): %v", rel, cerr)
	}
}

// TestAIInsightHandler_SummarizeAndTagPaths 覆盖两个端点的全部可裁决分支。
func TestAIInsightHandler_SummarizeAndTagPaths(t *testing.T) {
	h, ai, calls := aiTestEnv(t)

	// ① 未启用：tag 端点同样 400（与 summarize 一致的零回归语义）。
	rec := httptest.NewRecorder()
	h.handleAITag(rec, httptest.NewRequest(http.MethodPost, "/api/ai/tag?filename=x.txt", nil), "anonymous", NewAIInsightWithGate(nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tag 未启用应 400, got %d", rec.Code)
	}

	// ② 缺 filename / 路径穿越 / 非文本 / 不存在。
	for _, tc := range []struct {
		name, target string
		want         int
	}{
		{"缺参", "/api/ai/tag", http.StatusBadRequest},
		{"路径穿越", "/api/ai/tag?filename=../etc/passwd", http.StatusBadRequest},
		{"非文本", "/api/ai/tag?filename=user/empty.txt", http.StatusBadRequest},
		{"不存在", "/api/ai/tag?filename=user/none.txt", http.StatusNotFound},
	} {
		innerRec := httptest.NewRecorder()
		h.handleAITag(innerRec, httptest.NewRequest(http.MethodPost, tc.target, nil), "anonymous", ai)
		if innerRec.Code != tc.want {
			t.Fatalf("tag %s 应 %d, got %d", tc.name, tc.want, innerRec.Code)
		}
	}

	// ③ tag 成功 + 二次调用命中缓存（不再调网关）。
	rec = httptest.NewRecorder()
	h.handleAITag(rec, httptest.NewRequest(http.MethodPost, "/api/ai/tag?filename=user/text.txt", nil), "anonymous", ai)
	if rec.Code != http.StatusOK {
		t.Fatalf("tag 成功应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var tagResp struct {
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &tagResp); err != nil {
		t.Fatalf("tag 响应不是合法 JSON: %v (%s)", err, rec.Body.String())
	}
	if len(tagResp.Tags) != 2 || tagResp.Tags[0] != "alpha" || tagResp.Tags[1] != "beta" {
		t.Fatalf("标签规范化结果不符: %v", tagResp.Tags)
	}
	firstCalls := calls.Load()

	rec = httptest.NewRecorder()
	h.handleAITag(rec, httptest.NewRequest(http.MethodPost, "/api/ai/tag?filename=user/text.txt", nil), "anonymous", ai)
	if rec.Code != http.StatusOK {
		t.Fatalf("缓存命中应 200, got %d", rec.Code)
	}
	if got := calls.Load(); got != firstCalls {
		t.Fatalf("缓存命中不得再调网关: calls %d → %d", firstCalls, got)
	}
	if rec.Body.String() != `{"tags":["alpha","beta"]}` {
		t.Fatalf("缓存体应与首次一致, got %s", rec.Body.String())
	}

	// ④ summarize 成功（含 volume 提示分支：不存在的卷 ⇒ 404，不泄存在性）。
	rec = httptest.NewRecorder()
	h.handleAISummarize(rec, httptest.NewRequest(http.MethodPost, "/api/ai/summarize?filename=user/text.txt", nil), "anonymous", ai)
	if rec.Code != http.StatusOK {
		t.Fatalf("summarize 成功应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	var sumResp struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sumResp); err != nil || sumResp.Summary == "" {
		t.Fatalf("摘要响应异常: %v (%s)", err, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	h.handleAISummarize(rec, httptest.NewRequest(http.MethodPost, "/api/ai/summarize?filename=user/text.txt&volume=nope", nil), "anonymous", ai)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知卷应 404, got %d", rec.Code)
	}
}

// TestAIInsightHandler_QuotaAndGatewayFailure 钉住「配额超限 429 不调网关」与
// 「网关失败 502」两态（成本边界：超限/失败都不得写入缓存）。
func TestAIInsightHandler_QuotaAndGatewayFailure(t *testing.T) {
	h, ai, calls := aiTestEnv(t)
	writeTenantFile(t, h, "user/second.txt", "另一段文本用于配额与失败路径。")

	// 网关失败 ⇒ 502（5xx ⇒ Advise 报错）。
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(failSrv.Close)
	t.Setenv("SPROXY_TEST_AI_KEY_FAIL", "k")
	failing := NewAIInsightFromConfig(config.AIInsightConfig{
		Enabled: true, BaseURL: failSrv.URL, APIKeyRef: "SPROXY_TEST_AI_KEY_FAIL", Timeout: 5 * time.Second,
	}, testutil.MkdirTemp(t, "ai-fail-cache-"), testLogger())
	rec := httptest.NewRecorder()
	h.handleAISummarize(rec, httptest.NewRequest(http.MethodPost, "/api/ai/summarize?filename=user/text.txt", nil), "anonymous", failing)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("网关失败应 502, got %d (%s)", rec.Code, rec.Body.String())
	}

	// 配额：每日 1 次 ⇒ 第 2 个（未缓存的）文件 429，且不再调网关。
	ai.SetQuota(NewAIQuota(config.AIQuotaConfig{Enabled: true, Default: config.AILimit{DailyCalls: 1}}, testLogger()))
	rec = httptest.NewRecorder()
	h.handleAITag(rec, httptest.NewRequest(http.MethodPost, "/api/ai/tag?filename=user/text.txt", nil), "anonymous", ai)
	if rec.Code != http.StatusOK {
		t.Fatalf("首次调用应 200, got %d (%s)", rec.Code, rec.Body.String())
	}
	before := calls.Load()
	rec = httptest.NewRecorder()
	h.handleAITag(rec, httptest.NewRequest(http.MethodPost, "/api/ai/tag?filename=user/second.txt", nil), "anonymous", ai)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("配额超限应 429, got %d (%s)", rec.Code, rec.Body.String())
	}
	if got := calls.Load(); got != before {
		t.Fatalf("配额超限不得调网关: calls %d → %d", before, got)
	}
}

// TestAIInsightAdvise_DisabledFailsClosed 钉住未装配网关时 Advise fail-closed（不得假成功）。
func TestAIInsightAdvise_DisabledFailsClosed(t *testing.T) {
	t.Parallel()
	var nilInsight *AIInsight
	if _, err := nilInsight.Advise(t.Context(), "s", "u"); err == nil {
		t.Fatal("nil 洞察器必须返回错误")
	}
	disabled := NewAIInsightWithGate((*llmgate.Client)(nil))
	if disabled.Enabled() {
		t.Fatal("nil 网关应报告未启用")
	}
	if _, err := disabled.Advise(t.Context(), "s", "u"); err == nil {
		t.Fatal("未启用时必须返回错误（fail-closed）")
	}
}
