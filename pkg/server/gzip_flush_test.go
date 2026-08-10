// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGzipResponseWriter_Flush(t *testing.T) {
	rec := httptest.NewRecorder()
	w := &gzipResponseWriter{
		Writer:         rec,
		ResponseWriter: rec,
	}
	w.Flush()
	// 验证 Flush 后底层 ResponseWriter 状态正常
	_, err := w.Write([]byte("test"))
	if err != nil {
		t.Fatalf("write after flush: %v", err)
	}
	if rec.Body.String() != "test" {
		t.Errorf("expected 'test', got %q", rec.Body.String())
	}
}

func TestGzipMiddleware_CompressAndSetHeaders(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("compressed"))
	})
	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	handler.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("expected gzip encoding")
	}
	if rec.Header().Get("Vary") != "Accept-Encoding" {
		t.Fatalf("expected Vary: Accept-Encoding, got %q", rec.Header().Get("Vary"))
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
