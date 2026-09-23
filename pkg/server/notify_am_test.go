// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// notify_am_test.go 验证 Alertmanager/Grafana 渠道（roadmap P2 Webhook 通用插件残余）：
//  1. Alertmanager：payload 为 webhook v2 alerts 数组（labels/annotations）。
//  2. Grafana：payload 为 annotations API（time/text/tags + bearer auth）。

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

// TestNotifyCenter_AlertmanagerChannel Alertmanager webhook v2 载荷。
func TestNotifyCenter_AlertmanagerChannel(t *testing.T) {
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

	ch := NewAlertmanagerNotifier(srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "告警", Text: "disk 满", Action: "quota", Object: "alice"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	alerts, ok := got["alerts"].([]any)
	if !ok || len(alerts) == 0 {
		t.Fatalf("载荷无 alerts 数组: %v", got)
	}
	first := alerts[0].(map[string]any)
	labels, _ := first["labels"].(map[string]any)
	if labels == nil || labels["alertname"] != "sproxy-quota" {
		t.Fatalf("labels 无 alertname: %v", first["labels"])
	}
	annotations, _ := first["annotations"].(map[string]any)
	if annotations == nil || annotations["summary"] != "告警" {
		t.Fatalf("annotations 无 summary: %v", first["annotations"])
	}
}

// TestNotifyCenter_GrafanaChannel Grafana annotations API 载荷 + bearer auth。
func TestNotifyCenter_GrafanaChannel(t *testing.T) {
	t.Parallel()
	var got map[string]any
	var gotAuth string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		_ = json.Unmarshal(body, &got)
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ch := NewGrafanaNotifier(srv.URL, "test-token", "", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := ch.Send(ctx, NotifyMessage{Title: "标注", Text: "quota 80%", Action: "quota", Object: "alice"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer test-token" {
		t.Fatalf("Authorization = %q, want Bearer test-token", gotAuth)
	}
	text, _ := got["text"].(string)
	if !strings.Contains(text, "quota 80%") {
		t.Fatalf("text 应含 quota: %q", text)
	}
	if _, ok := got["time"]; !ok {
		t.Fatalf("annotations 载荷应有 time: %v", got)
	}
}

// TestNotifyCenter_AlertmanagerNoURL 空 URL fail-closed（变异探针）。
func TestNotifyCenter_AlertmanagerNoURL(t *testing.T) {
	t.Parallel()
	ch := NewAlertmanagerNotifier("")
	if err := ch.Send(context.Background(), NotifyMessage{Title: "x"}); err == nil {
		t.Fatalf("空 URL 应失败")
	}
}
