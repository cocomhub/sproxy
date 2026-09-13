// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestHandlers_Close 验证 Close 幂等安全。
func TestHandlers_Close(t *testing.T) {
	cfgPtr := newTestCfgPtr(t.TempDir())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	h.Close()
	// 再次 Close 不应 panic
	h.Close()
}

func TestTunnelRoute_RejectsMissingKey(t *testing.T) {
	t.Parallel()

	cfgPtr := newTestCfgPtr(t.TempDir())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	defer h.Close()

	// POST /tunnel 未注入派生密钥（未经 authMiddleware 验签）⇒ 外层帧解密器拒绝，401。
	// 取代原先经 h.TunnelHandler() 取 handler 的写法：该访问器已删除（tunnel_key 废除、
	// 无 SIGHUP 热替换消费方），此用例改为直接钉住路由行为，证明路由仍已接线。
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/tunnel", nil)
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST /tunnel without derived key: expected 401, got %d", w.Code)
	}
}

func TestHandler_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	cfgPtr := newTestCfgPtr(t.TempDir())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	defer h.Close()
	handler := h.Handler()
	if handler == nil {
		t.Fatal("Handler() returned nil")
	}
}

func TestHandler_HealthzRoute(t *testing.T) {
	t.Parallel()

	cfgPtr := newTestCfgPtr(t.TempDir())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "OK" {
		t.Errorf("expected body 'OK', got '%s'", string(body))
	}
}

func TestHandler_VersionRoute(t *testing.T) {
	t.Parallel()

	cfgPtr := newTestCfgPtr(t.TempDir())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "v1.0.0",
		BuildAt: "2026-06-13",
		Logger:  testLogger(),
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /version: expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Error("expected non-empty version body")
	}
}

func TestHandler_UploadRouteRequiresAuth(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/upload", "multipart/form-data", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for unauthenticated upload, got %d", resp.StatusCode)
	}
}

func TestUpdateKey(t *testing.T) {
	t.Parallel()

	key1Hex := testKey()
	key1, err := tunnel.ParseKey(key1Hex)
	if err != nil {
		t.Fatal(err)
	}
	key2Hex := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	key2, err := tunnel.ParseKey(key2Hex)
	if err != nil {
		t.Fatal(err)
	}

	tunnelLogger := testLogger()
	th := tunnel.NewLocalHandler(nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}), tunnelLogger)

	// UpdateKey 已废除（认证驱动隧道，无进程级热替换），保留 API 为 no-op，调用不 panic。
	if updater, ok := th.(*tunnel.Handler); ok {
		updater.UpdateKey(key2)
	} else {
		t.Fatal("tunnel handler does not implement UpdateKey")
	}

	req := httptest.NewRequest("GET", "/", nil)

	// 场景 1：ctx 密钥 = client 密钥 → 解密成功。
	srv1 := httptest.NewServer(withTunnelKeyCtx(key1, th))
	defer srv1.Close()
	client1, err := tunnel.NewClient(key1Hex, srv1.URL, time.Second, tunnelLogger)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client1.Do(req)
	if err != nil {
		t.Fatalf("key1 request failed: %v", err)
	}
	resp.Body.Close()

	// 场景 2：ctx 密钥 ≠ client 密钥 → 解密失败（metadata 认证失败）。
	srv2 := httptest.NewServer(withTunnelKeyCtx(key2, th))
	defer srv2.Close()
	client2, err := tunnel.NewClient(key1Hex, srv2.URL, time.Second, tunnelLogger)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client2.Do(req)
	if err == nil {
		t.Error("expected error when ctx key differs from client key, got nil")
	}
}
