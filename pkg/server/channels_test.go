// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// channels_test.go 验证邮箱（SMTP）+ Webhook 通用渠道（roadmap P1 邮箱 / P2 Webhook 通用）：
//  1. email 渠道：消息组装（RFC 822 头 + HTML 摘要体）+ sendFunc 注入（不 dial 真实 SMTP）。
//  2. webhook 渠道：通用 POST JSON（httptest mock 校验载荷）。
//  3. config 装配：email/webhook 配置 → 渠道注册。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestEmailNotifier_MessageAssembly 邮箱渠道：消息组装 + sendFunc 注入。
func TestEmailNotifier_MessageAssembly(t *testing.T) {
	t.Parallel()
	var sent struct {
		addr string
		from string
		to   []string
		msg  []byte
	}
	ch := NewEmailNotifier(EmailConfig{
		SMTPHost: "smtp.example.com",
		Port:     465,
		From:     "sproxy@example.com",
		To:       []string{"admin@example.com"},
		Username: "user",
		Password: "pass",
	})
	ch.sendFunc = func(addr string, a smtpAuth, from string, to []string, msg []byte) error {
		sent.addr = addr
		sent.from = from
		sent.to = to
		sent.msg = msg
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "通知", Text: "hello"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sent.addr != "smtp.example.com:465" {
		t.Fatalf("addr = %q", sent.addr)
	}
	if sent.from != "sproxy@example.com" || len(sent.to) != 1 || sent.to[0] != "admin@example.com" {
		t.Fatalf("from/to = %q %v", sent.from, sent.to)
	}
	if !strings.Contains(string(sent.msg), "通知") || !strings.Contains(string(sent.msg), "hello") {
		t.Fatalf("msg 应含标题与正文: %q", sent.msg[:min(len(sent.msg), 200)])
	}
}

// TestWebhookNotifier_PostJSON Webhook 通用渠道：POST JSON 载荷校验。
func TestWebhookNotifier_PostJSON(t *testing.T) {
	t.Parallel()
	var got map[string]any
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewWebhookNotifier(WebhookConfig{URL: srv.URL})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "通知", Text: "hello", Object: "a.txt", Action: "upload"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got["title"] != "通知" || got["text"] != "hello" || got["object"] != "a.txt" || got["action"] != "upload" {
		t.Fatalf("载荷不符: %v", got)
	}
}

// TestNotifyConfig_EmailWebhookRegister config 装配 email/webhook 渠道。
func TestNotifyConfig_EmailWebhookRegister(t *testing.T) {
	t.Parallel()
	nc := newNotifyCenterFromConfig(NotifyConfig{
		Enabled: true,
		Channels: NotifyChannelsConfig{
			Email:   EmailConfig{SMTPHost: "smtp.example.com", From: "a@b.c", To: []string{"d@e.f"}},
			Webhook: WebhookConfig{URL: "https://hooks.example.com/x"},
		},
	}, nil)
	defer nc.Close()
	if !nc.hasChannel("email") || !nc.hasChannel("webhook") {
		t.Fatalf("email/webhook 渠道未注册: %v", nc.channelNames())
	}
}
