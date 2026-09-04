// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// testAccessKey / testAccessSecret 是 SproxySig 测试密钥对。
const (
	testAccessKey    = "sk-test-mesh-aabbcc"
	testAccessSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// formatSigAuth 把 Header 序列化为 Authorization 头。
func formatSigAuth(h sproxysig.Header) string {
	return sproxysig.Scheme + " v=" + h.Version + " ak=" + h.AK +
		" ts=" + strconv.FormatInt(h.TS, 10) + " exp=" + strconv.FormatInt(h.Exp, 10) +
		" nonce=" + h.Nonce + " body_sha256=" + h.BodySHA256 + " sig=" + h.Sig
}

// testNonceCounter 为测试签名生成全局唯一 nonce 的递增序号。
var testNonceCounter atomic.Uint64

// testNonce 生成全局唯一 nonce（UnixNano + 递增序号）：纯 UnixNano 在紧凑的连续
// 签名调用间可能相同（时钟 tick 未前进），服务端防重放池会把第二个请求判为 replay
// 返回 401（实测 flaky）。加序号后同一进程内恒唯一。
func testNonce() string {
	return fmt.Sprintf("test-nonce-%d-%d", time.Now().UnixNano(), testNonceCounter.Add(1))
}

// signRequest 用给定 AK/SK 给请求打上合法 SproxySig 头（无 body）。
func signRequest(r *http.Request, ak, sk string) {
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      testNonce(),
		BodySHA256: sproxysig.EmptyBodyHash(),
	}
	h.Sig = sproxysig.Sign(sk, h, r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	r.Header.Set("Authorization", formatSigAuth(h))
}

// signTunnelRequest 给隧道外层请求打 UNSIGNED 签名头（流式加密 body 无法整体哈希，
// 与 pkg/client.sigRoundTripper 一致）。服务端 authMiddleware 验签后派生隧道密钥。
func signTunnelRequest(r *http.Request, ak, sk string) {
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      testNonce(),
		BodySHA256: sproxysig.UnsignedBody,
	}
	h.Sig = sproxysig.Sign(sk, h, r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	r.Header.Set("Authorization", formatSigAuth(h))
}

// tunnelSignTransport 给每个 /tunnel 外层请求注入 UNSIGNED SproxySig 签名头，
// 模拟 pkg/client.sigRoundTripper 的行为（本 SDK 无 http.RoundTripperFunc）。
type tunnelSignTransport struct {
	base   http.RoundTripper
	ak, sk string
}

func (t *tunnelSignTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	signTunnelRequest(req, t.ak, t.sk)
	return t.base.RoundTrip(req)
}

func TestPermissionAllowed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		permission string
		method     string
		want       bool
	}{
		{"write allows POST", "write", http.MethodPost, true},
		{"write allows GET", "write", http.MethodGet, true},
		{"write allows DELETE", "write", http.MethodDelete, true},
		{"read allows GET", "read", http.MethodGet, true},
		{"read allows HEAD", "read", http.MethodHead, true},
		{"read denies POST", "read", http.MethodPost, false},
		{"read denies DELETE", "read", http.MethodDelete, false},
		{"read denies PUT", "read", http.MethodPut, false},
		{"empty permission allows GET", "", http.MethodGet, true},
		{"unknown permission denies", "admin", http.MethodGet, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := permissionAllowed(tt.permission, tt.method)
			if got != tt.want {
				t.Errorf("permissionAllowed(%q, %q) = %v, want %v",
					tt.permission, tt.method, got, tt.want)
			}
		})
	}
}

func TestAuthMiddleware_NoAuthConfigured(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, credentialRing: emptyTestRing(), allowInsecureLoopback: true}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	r.RemoteAddr = "127.0.0.1:1234" // loopback：allow_insecure_loopback 兜底放行
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if !called {
		t.Error("authMiddleware should pass through when no auth configured")
	}
	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
}

func TestAuthMiddleware_SproxySigMissing(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool(), credentialRing: ringForTestCreds()}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("authMiddleware should block request when no SproxySig provided")
	}
}

func TestAuthMiddleware_SproxySigValid(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool(), credentialRing: ringForTestCreds()}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	signRequest(r, testAccessKey, testAccessSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("authMiddleware blocked request with valid SproxySig")
	}
}

func TestAuthMiddleware_SproxySigBadSignature(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool(), credentialRing: ringForTestCreds()}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	// 用错误的 SK 签名 → 401。
	r := httptest.NewRequest("GET", "/upload", nil)
	signRequest(r, testAccessKey, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("authMiddleware should block request with bad signature")
	}
}

func TestAuthMiddleware_SproxySigUnknownKey(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool(), credentialRing: ringForTestCreds()}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	// 未配置的 AK → 401。
	r := httptest.NewRequest("GET", "/upload", nil)
	signRequest(r, "sk-unknown", testAccessSecret)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("authMiddleware should block request with unknown AccessKey")
	}
}

func TestAuthMiddleware_APIKeyValid(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{
		APIKeys: APIKeyConfig{
			Enabled: true,
			Keys: []APIKey{
				{Key: "mykey", Permission: "write"},
			},
		},
	})
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("POST", "/upload", nil)
	r.Header.Set("Authorization", "Bearer mykey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("authMiddleware blocked request with valid API key")
	}
}

func TestAuthMiddleware_APIKeyInsufficientPermission(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{
		APIKeys: APIKeyConfig{
			Enabled: true,
			Keys: []APIKey{
				{Key: "readonly", Permission: "read"},
			},
		},
	})
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("POST", "/upload", nil)
	r.Header.Set("Authorization", "Bearer readonly")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusForbidden {
		t.Errorf("expected 403, got %d", w.Code)
	}
	if called {
		t.Error("authMiddleware should block POST with read-only key")
	}
}
