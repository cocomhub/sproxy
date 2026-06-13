// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGzipMiddleware_Compresses(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	handler.ServeHTTP(rec, req)

	// Verify Content-Encoding header
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip, got: %q", enc)
	}

	// Verify Vary header
	found := false
	for _, v := range rec.Header().Values("Vary") {
		if v == "Accept-Encoding" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected Vary: Accept-Encoding header")
	}

	// Verify body is gzip compressed
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to read decompressed body: %v", err)
	}
	if string(body) != "hello" {
		t.Fatalf("expected decompressed body 'hello', got: %q", string(body))
	}
}

func TestGzipMiddleware_NoEncoding(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	// No Accept-Encoding header

	handler.ServeHTTP(rec, req)

	// Verify no Content-Encoding header
	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected no Content-Encoding, got: %q", enc)
	}

	// Verify body is plain text
	body := rec.Body.String()
	if body != "hello" {
		t.Fatalf("expected body 'hello', got: %q", body)
	}

	// Also verify without "gzip" in Accept-Encoding (e.g. "deflate")
	rec2 := httptest.NewRecorder()
	req2 := httptest.NewRequest("GET", "/", nil)
	req2.Header.Set("Accept-Encoding", "deflate")
	handler.ServeHTTP(rec2, req2)

	if enc := rec2.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected no Content-Encoding for deflate-only, got: %q", enc)
	}
	if rec2.Body.String() != "hello" {
		t.Fatalf("expected body 'hello' for deflate-only, got: %q", rec2.Body.String())
	}
}

func TestGzipMiddleware_ContentEncodingNotGzip(t *testing.T) {
	// Test that headers other than Accept-Encoding don't trigger compression
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello"))
	})

	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "identity")

	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected no Content-Encoding for identity, got: %q", enc)
	}
	if rec.Body.String() != "hello" {
		t.Fatalf("expected body 'hello', got: %q", rec.Body.String())
	}
}

func TestGzipMiddleware_MultipleEncoding(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("gzip compression test"))
	})

	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")

	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip, got: %q", enc)
	}

	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()

	body, err := io.ReadAll(gr)
	if err != nil {
		t.Fatalf("failed to read decompressed body: %v", err)
	}
	if !strings.Contains(string(body), "gzip compression") {
		t.Fatalf("expected 'gzip compression' in decompressed body, got: %q", string(body))
	}
}
