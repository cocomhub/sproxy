// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestRelayHandlerMissingTarget(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	body := `{"target":"", "method":"GET", "path":"/api/files"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	var resp RelayResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", resp.Status)
	}
}

func TestRelayHandlerUnknownTarget(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	body := `{"target":"unknown", "method":"GET", "path":"/api/files"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRelayHandlerBadJSON(t *testing.T) {
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, slog.Default())

	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader("{bad json"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestRelayHandlerRoundTrip(t *testing.T) {
	// 端到端测试：创建一个中继目标节点（模拟 Node），
	// 通过 relay handler 转发请求到该节点
	pipeA, pipeB := xfertest.Pipe()
	targetMux := mux.New(pipeA, mux.RoleDialer)
	relayMux := mux.New(pipeB, mux.RoleListener)

	// 创建 RouteTable 并注册目标节点
	rt := hub.NewRouteTable()
	rt.Add("test-node", targetMux)
	h := NewRelayHandler(rt, slog.Default())

	// 启动服务端 goroutine（模拟目标节点的处理能力）
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		tunB := tunnel.NewTunnel(relayMux, nil)
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Custom", "hello")
			w.Write([]byte("relay ok"))
		}))
	}()
	time.Sleep(50 * time.Millisecond)

	// 发送中继请求
	body := `{"target":"test-node", "method":"GET", "path":"/api/test"}`
	req := httptest.NewRequest(http.MethodPost, "/api/relay", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp RelayResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("expected relay status 200, got %d", resp.Status)
	}
	if resp.BodyBase64 != "relay ok" {
		t.Fatalf("expected relay body 'relay ok', got %q", resp.BodyBase64)
	}
	cancel()
	<-srvErr
}

func TestBodyToString(t *testing.T) {
	tests := []struct {
		name string
		body []byte
		want string
	}{
		{"empty", []byte{}, ""},
		{"small", []byte("hello"), "hello"},
		{"ascii", []byte{0x48, 0x65, 0x6c, 0x6c, 0x6f}, "Hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bodyToString(tt.body)
			if got != string(tt.body) {
				t.Fatalf("expected %q, got %q", string(tt.body), got)
			}
		})
	}
}
