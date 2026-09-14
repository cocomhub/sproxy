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

// TestTunnelRoute_RejectsBadFrameWithDerivedKey 继承原 TestTunnelHandler_ReturnsHandler 的 400 断言：
// POST /tunnel 通过 SproxySig 验签（authMiddleware 据此派生隧道密钥）但帧体为空 ⇒ 外层帧解密器
// 解析失败 400，与「缺少凭据/签名 ⇒ 401」区分，证明隧道 handler 在路由上确实生效。
//
// 为何不能沿用 withTunnelKeyCtx 直接注入密钥：路由上的 /tunnel 先经 authMiddleware，未配置凭据时
// 它在到达 handler 前就 401（实测），因此本用例走真实签名 + 真实派生链路。
func TestTunnelRoute_RejectsBadFrameWithDerivedKey(t *testing.T) {
	t.Parallel()

	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts)
	})

	req, err := http.NewRequest("POST", url+"/tunnel", nil)
	if err != nil {
		t.Fatal(err)
	}
	signTunnelRequest(req, testAccessKey, testAccessSecret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /tunnel with valid signature but empty frame: expected 400, got %d", resp.StatusCode)
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

// TestTunnelHandler_KeyMismatchRejected 钉住认证驱动隧道的密钥语义：ctx 派生密钥与客户端
// 加密密钥一致则解密成功，不一致则 metadata 认证失败。
//
// 由原 TestUpdateKey 改写而来：原用例在此之上还有一段已删除符号 UpdateKey 的 no-op 断言；
// 这里保留与死代码无关的存活行为断言（withTunnelKeyCtx 在全仓仅此处使用，是 handler 层唯一的
// 密钥不匹配反例，删掉会丢安全相关覆盖）。
func TestTunnelHandler_KeyMismatchRejected(t *testing.T) {
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
