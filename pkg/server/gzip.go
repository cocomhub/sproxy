// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	statusCode int
}

func (w *gzipResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	if f, ok := w.Writer.(interface{ Flush() }); ok {
		f.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// GzipMiddleware 返回一个 HTTP 中间件，当客户端支持 gzip 且响应为文本/JSON 时，
// 透明地对响应体进行 gzip 压缩。
func GzipMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	log := defaultLogger(logger)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			gw, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
			if err != nil {
				// fallback: 不压缩
				next.ServeHTTP(w, r)
				return
			}
			defer func() {
				if err := gw.Close(); err != nil {
					log.Warn("关闭 gzip writer 失败", "error", err)
				}
			}()
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			next.ServeHTTP(&gzipResponseWriter{Writer: gw, ResponseWriter: w}, r)
		})
	}
}
