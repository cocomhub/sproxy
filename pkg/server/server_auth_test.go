// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

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
		{"empty permission denies", "", http.MethodGet, false},
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

func TestAuthMiddleware_AuthTokenMissing(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{AuthToken: "secret"})
	h := &Handlers{cfgPtr: cfgPtr}
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
		t.Error("authMiddleware should block request when no token provided")
	}
}

func TestAuthMiddleware_AuthTokenValid(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{AuthToken: "valid"})
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if !called {
		t.Error("authMiddleware blocked request with valid token")
	}
}

func TestAuthMiddleware_AuthTokenMismatch(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{AuthToken: "secret"})
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	inner := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		called = true
	})
	handler := h.authMiddleware(inner)

	r := httptest.NewRequest("GET", "/upload", nil)
	r.Header.Set("Authorization", "Bearer wrong")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", w.Code)
	}
	if called {
		t.Error("authMiddleware should block request with wrong token")
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
