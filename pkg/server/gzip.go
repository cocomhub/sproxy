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

// GzipMiddleware 返回一个 HTTP 中间件，对客户端支持 gzip 的所有响应体进行 gzip 压缩。
// 注意：内容类型不限于文本；对所有 Accept-Encoding 包含 gzip 的请求均压缩。
// 注意：gzipResponseWriter 未实现 http.Hijacker。如果后续需要与支持劫持的 Handler（如隧道/tunnel handler）
// 配合使用，应重写该中间件使其在劫持场景下跳过 gzip 压缩。
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
			w.Header().Del("Content-Length")
			w.Header().Set("Vary", "Accept-Encoding")
			next.ServeHTTP(&gzipResponseWriter{Writer: gw, ResponseWriter: w}, r)
		})
	}
}
