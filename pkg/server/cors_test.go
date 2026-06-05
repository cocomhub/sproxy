// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSMiddleware_EmptyOrigins_Passthrough(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{}, nil)
	var called bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("next handler should be called")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers should be set for empty origins config")
	}
}

func TestCORSMiddleware_AllowAll(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{AllowedOrigins: []string{"*"}}, nil)
	var called bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("next handler should be called")
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected '*', got %q", got)
	}
}

func TestCORSMiddleware_WhitelistOrigin(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{
		AllowedOrigins: []string{"https://trusted.com"},
		MaxAge:         3600,
	}, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://trusted.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "https://trusted.com" {
		t.Fatalf("expected 'https://trusted.com', got %q", got)
	}
	if got := w.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("expected Vary: Origin, got %q", got)
	}
	if got := w.Header().Get("Access-Control-Max-Age"); got != "3600" {
		t.Fatalf("expected Max-Age 3600, got %q", got)
	}
}

func TestCORSMiddleware_OPTIONS_Preflight(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{AllowedOrigins: []string{"*"}}, nil)
	var called bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("OPTIONS", "/", nil)
	req.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if called {
		t.Fatal("next handler should NOT be called for OPTIONS preflight")
	}
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("expected '*', got %q", got)
	}
}

func TestCORSMiddleware_RejectedOrigin(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{
		AllowedOrigins: []string{"https://trusted.com"},
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	var called bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "https://evil.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("next handler should still be called for rejected origin")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers should be set for rejected origin")
	}
}

func TestCORSMiddleware_MissingOrigin(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{AllowedOrigins: []string{"*"}}, nil)
	var called bool
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	// No Origin header
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !called {
		t.Fatal("next handler should be called")
	}
	if w.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no CORS headers should be set for requests without Origin")
	}
}

func TestCORSMiddleware_MaxAgeZero(t *testing.T) {
	t.Parallel()

	mw := CORSMiddleware(CORSConfig{
		AllowedOrigins: []string{"*"},
		MaxAge:         0,
	}, nil)

	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Origin", "http://example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	// MaxAge 0 should use default 86400
	if got := w.Header().Get("Access-Control-Max-Age"); got != "86400" {
		t.Fatalf("expected default Max-Age 86400, got %q", got)
	}
}
