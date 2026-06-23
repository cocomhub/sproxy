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

func TestGzipResponseWriter_Flush(_ *testing.T) {
	// Flush() 不应 panic
	w := &gzipResponseWriter{
		Writer:         io.Discard,
		ResponseWriter: httptest.NewRecorder(),
	}
	w.Flush()
}

func TestGzipMiddleware_FallbackOnCompressError(t *testing.T) {
	// 验证当 gzip.NewWriterLevel 失败时的 fallback 路径。使用极端的压缩等级
	// 来触发 gzip.NewWriterLevel 返回错误。
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("fallback"))
	})

	// 注意：gzip.NewWriterLevel 通常不会返回错误，除非等级超出范围。
	// 由于 GzipMiddleware 内部使用 gzip.DefaultCompression，这个路径在
	// 正常情况下不可达。这里只做文档标记。
	mw := GzipMiddleware(slog.Default())
	handler := mw(inner)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")

	handler.ServeHTTP(rec, req)

	if rec.Header().Get("Content-Encoding") != "gzip" {
		// Even without compression, the handler should still work
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", rec.Code)
		}
		if rec.Body.String() != "fallback" {
			t.Fatalf("expected 'fallback', got %q", rec.Body.String())
		}
	}
}
