// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// TestWSCustomPath_DialMatches 验证自定义路径两端一致时连通（roadmap §5.3 P1 形态对齐）。
func TestWSCustomPath_DialMatches(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	n := ws.NewHandlerNode()
	n.AddToMux(mux, "/api/v1/stream")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 客户端自定义路径拨号 → 连通（握手成功）。
	conn, err := ws.DialWithOptions(context.Background(), ts.URL, ws.DialOptions{Path: "/api/v1/stream"})
	if err != nil {
		t.Fatalf("自定义路径拨号应成功, got %v", err)
	}
	_ = conn.Close()
}

// TestWSCustomPath_Mismatch404 验证两端路径不一致 → 拨号失败（404 明确报错，禁静默降级）。
func TestWSCustomPath_Mismatch404(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	n := ws.NewHandlerNode()
	n.AddToMux(mux, "/custom")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 客户端用默认 /ws → 服务端只有 /custom → 404。
	conn, err := ws.DialWithOptions(context.Background(), ts.URL, ws.DialOptions{})
	if err == nil {
		_ = conn.Close()
		t.Fatal("路径不一致应拨号失败（服务端 404），却连通了")
	}
}

// TestWSUpgradeHeader_CustomMatch 验证自定义升级头两端一致时连通。
func TestWSUpgradeHeader_CustomMatch(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	n := ws.NewHandlerNode(ws.WithUpgradeHeader("http2"))
	n.AddToMux(mux, "/ws")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conn, err := ws.DialWithOptions(context.Background(), ts.URL, ws.DialOptions{UpgradeHeader: "http2"})
	if err != nil {
		t.Fatalf("自定义升级头一致应连通, got %v", err)
	}
	_ = conn.Close()
}

// TestWSUpgradeHeader_DefaultMismatch 验证服务端自定义升级头 + 客户端默认 → 拒绝（可观测）。
func TestWSUpgradeHeader_DefaultMismatch(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	n := ws.NewHandlerNode(ws.WithUpgradeHeader("http2"))
	n.AddToMux(mux, "/ws")
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// 客户端默认 Upgrade: websocket → 服务端要求 http2 → 拒绝。
	if conn, err := ws.DialWithOptions(context.Background(), ts.URL, ws.DialOptions{}); err == nil {
		_ = conn.Close()
		t.Fatal("升级头不一致应拒绝，却连通了")
	}
}

// TestWSCustomPath_DefaultZeroRegression 验证默认 /ws 零回归（既有 harness 路径仍通）。
func TestWSCustomPath_DefaultZeroRegression(t *testing.T) {
	t.Parallel()
	_, _, cleanup := newWSPair(t)
	defer cleanup()
	// 无固定等待：newWSPair 已同步握手（R18 棘轮：不引入 time.Sleep）。
}
