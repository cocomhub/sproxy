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

// signRequest 用给定 AK/SK 给请求打上合法 SproxySig 头（无 body）。
func signRequest(r *http.Request, ak, sk string) {
	now := time.Now()
	h := sproxysig.Header{
		Version: sproxysig.Version, AK: ak,
		TS: now.UnixMilli(), Exp: now.Add(sproxysig.DefaultExpiry).UnixMilli(),
		Nonce:      fmt.Sprintf("test-nonce-%d", now.UnixNano()),
		BodySHA256: sproxysig.EmptyBodyHash(),
	}
	h.Sig = sproxysig.Sign(sk, h, r.Method, r.URL.EscapedPath(), r.URL.RawQuery)
	r.Header.Set("Authorization", formatSigAuth(h))
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
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
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
	cfgPtr.Store(&Config{AccessKeys: []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool()}
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
	cfgPtr.Store(&Config{AccessKeys: []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool()}
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
	cfgPtr.Store(&Config{AccessKeys: []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool()}
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
	cfgPtr.Store(&Config{AccessKeys: []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}})
	h := &Handlers{cfgPtr: cfgPtr, noncePool: sproxysig.NewNoncePool()}
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
