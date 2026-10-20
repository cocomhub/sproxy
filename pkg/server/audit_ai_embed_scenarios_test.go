// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_ai_embed_scenarios_test.go 补设计 §9.1 的最后一个动作 `ai.embed`：
// 嵌入 = **外部 API 调用 + 携带用户内容**，必须可追溯（谁、provider/model、条数），
// 但审计 Detail **绝不能包含被嵌入的文本**。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/aiembed"
	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_AIEmbed 经服务端 wiring 取客户端并嵌入一次 ⇒ 一条 ai.embed（不含文本）。
func TestAuditScenario_AIEmbed(t *testing.T) {
	t.Parallel()

	// 假的 OpenAI 兼容 embeddings 端点。
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		gotBody = string(buf)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	h := &Handlers{}
	attachTestAudit(t, h)

	client := h.NewEmbedClient(aiembed.Config{
		Provider: "openai",
		BaseURL:  srv.URL,
		Model:    "text-embedding-3-small",
		Timeout:  5 * time.Second,
	})
	const text = "机密内容：不应出现在审计里"
	vec, err := client.Embed(context.Background(), text)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(vec) == 0 {
		t.Fatal("应返回向量")
	}
	if !strings.Contains(gotBody, "机密内容") {
		t.Fatalf("请求体应包含被嵌入文本（客户端契约）, got %q", gotBody)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: ActionAIEmbed}, 0, false)
	if len(entries) == 0 {
		t.Fatal("缺少 ai.embed 审计行（外部 API 调用 + 用户内容，必须可追溯 ✗）")
	}
	ev := entries[len(entries)-1]
	if ev.Result != "success" {
		t.Errorf("result = %q, want success", ev.Result)
	}
	if !strings.Contains(ev.Detail, "model=text-embedding-3-small") || !strings.Contains(ev.Detail, "n=1") {
		t.Errorf("detail 应含 model 与条数, got %q", ev.Detail)
	}
	// 红线：审计不得包含被嵌入的文本或其片段。
	if strings.Contains(ev.Detail, text) || strings.Contains(ev.Detail, "机密内容") {
		t.Fatalf("ai.embed 审计泄露被嵌入文本 ✗: %q", ev.Detail)
	}
	if ev.Object != "openai" {
		t.Errorf("object = %q, want provider（openai）", ev.Object)
	}
}
