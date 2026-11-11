// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_notify_scenarios_test.go 补设计 §9.1「通知渠道自检」审计场景：`POST /api/notify/test`
// 会向真实渠道外发消息（可能触达外部系统）⇒ 必须留下可追溯的业务审计（谁、对哪些渠道）。
// 此前该路由在 R35-b 登记表里长期为 `pending`（有埋点、缺场景证据）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_NotifyTest 渠道自检成功 ⇒ 一条 notify_test 审计（result=success）。
func TestAuditScenario_NotifyTest(t *testing.T) {
	t.Parallel()
	// wecom 渠道 → mock webhook（避免真实外发）。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"errcode":0}`))
	}))
	defer srv.Close()

	url, h := newEventTestServer(t, func(c *Config) {
		c.Notify.Enabled = true
		c.Notify.Rules = []NotifyRule{{Action: "*", Channels: []string{"wecom"}}}
		c.Notify.Channels.Wecom.Webhook = srv.URL
	})
	attachTestAudit(t, h)

	resp, err := testHTTPClient(t).Post(url+"/api/notify/test", contentTypeJSON, nil)
	if err != nil {
		t.Fatalf("POST /api/notify/test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "notify_test"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("通知渠道自检必须留审计（会外发真实消息，必须可追溯 ✗）")
	}
	ev := entries[len(entries)-1]
	if ev.Action != "notify_test" {
		t.Fatalf("action = %q, want notify_test", ev.Action)
	}
	if ev.Result != "success" {
		t.Fatalf("result = %q, want success", ev.Result)
	}
	if ev.ObjectType != "notify" {
		t.Fatalf("object_type = %q, want notify", ev.ObjectType)
	}
}
