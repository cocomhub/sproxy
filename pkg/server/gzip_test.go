// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestGzipMiddleware_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		acceptEncoding string
		body           string
		wantGzip       bool
	}{
		{
			name:           "gzip compression",
			acceptEncoding: "gzip",
			body:           "hello",
			wantGzip:       true,
		},
		{
			name:           "no accept-encoding",
			acceptEncoding: "",
			body:           "hello",
			wantGzip:       false,
		},
		{
			name:           "deflate only",
			acceptEncoding: "deflate",
			body:           "hello",
			wantGzip:       false,
		},
		{
			name:           "identity",
			acceptEncoding: "identity",
			body:           "hello",
			wantGzip:       false,
		},
		{
			name:           "multiple encodings with gzip",
			acceptEncoding: "gzip, deflate, br",
			body:           "gzip compression test",
			wantGzip:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tt.body))
			})

			mw := GzipMiddleware(slog.Default())
			handler := mw(inner)

			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/", nil)
			if tt.acceptEncoding != "" {
				req.Header.Set("Accept-Encoding", tt.acceptEncoding)
			}

			handler.ServeHTTP(rec, req)

			if tt.wantGzip {
				if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
					t.Fatalf("expected Content-Encoding: gzip, got: %q", enc)
				}
				found := slices.Contains(rec.Header().Values("Vary"), "Accept-Encoding")
				if !found {
					t.Fatal("expected Vary: Accept-Encoding header")
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
				if string(body) != tt.body {
					t.Fatalf("expected decompressed body %q, got: %q", tt.body, string(body))
				}
			} else {
				if enc := rec.Header().Get("Content-Encoding"); enc != "" {
					t.Fatalf("expected no Content-Encoding, got: %q", enc)
				}
				if rec.Body.String() != tt.body {
					t.Fatalf("expected body %q, got: %q", tt.body, rec.Body.String())
				}
			}
		})
	}
}

// 保留 WriteHeader+status 的独立测试（无法 table-driven 化）
func TestGzipMiddleware_WriteHeaderAndFlush(t *testing.T) {
	t.Parallel()
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("not found"))
	})

	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}

	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to create gzip reader: %v", err)
	}
	defer gr.Close()
	body, _ := io.ReadAll(gr)
	if string(body) != "not found" {
		t.Fatalf("expected 'not found', got: %q", string(body))
	}
}

func TestGzipMiddleware_NilLogger(t *testing.T) {
	t.Parallel()
	// GzipMiddleware(nil) should not panic
	mw := GzipMiddleware(nil)
	handler := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("expected Content-Encoding: gzip, got: %q", enc)
	}
	gr, _ := gzip.NewReader(rec.Body)
	defer gr.Close()
	body, _ := io.ReadAll(gr)
	if string(body) != "ok" {
		t.Fatalf("expected 'ok', got: %q", string(body))
	}
}

func TestGzipMiddleware_NoAcceptEncoding(t *testing.T) {
	t.Parallel()
	// 验证在不传 Accept-Encoding 时 Content-Encoding 为空且 body 为原文
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("plain text"))
	})
	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)

	if enc := rec.Header().Get("Content-Encoding"); enc != "" {
		t.Fatalf("expected no Content-Encoding, got: %q", enc)
	}
	if !strings.Contains(rec.Body.String(), "plain") {
		t.Fatalf("expected 'plain' in body, got: %q", rec.Body.String())
	}
}
