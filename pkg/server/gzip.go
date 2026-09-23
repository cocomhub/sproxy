// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"compress/gzip"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *gzipResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.statusCode = statusCode
	if statusCode == http.StatusNoContent ||
		statusCode == http.StatusPartialContent ||
		statusCode == http.StatusNotModified ||
		statusCode >= 400 {
		w.ResponseWriter.WriteHeader(statusCode)
		return
	}
	w.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	w.ResponseWriter.Header().Del("Content-Length")
	w.ResponseWriter.Header().Set("Vary", "Accept-Encoding")
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if w.statusCode >= 400 {
		return w.ResponseWriter.Write(b)
	}
	return w.Writer.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	if w.statusCode < 400 {
		if f, ok := w.Writer.(interface{ Flush() }); ok {
			f.Flush()
		}
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// GzipMiddleware 返回一个 HTTP 中间件，对客户端支持 gzip 的所有响应体进行 gzip 压缩。
// 注意：内容类型不限于文本；对所有 Accept-Encoding 包含 gzip 的请求均压缩。
// 注意：gzipResponseWriter 未实现 http.Hijacker。如果后续需要与支持劫持的 Handler（如隧道/tunnel handler）
// 配合使用，应重写该中间件使其在劫持场景下跳过 gzip 压缩。
// gzipContentTypes 是可自动 gzip 的 Content-Type 白名单（前缀匹配，文本/JSON 类）。
var gzipContentTypes = []string{
	"text/",
	"application/json",
	"application/xml",
	"application/javascript",
	"application/x-javascript",
	"application/wasm",
	"image/svg+xml",
}

// gzipEligible 判断 Content-Type 是否可自动 gzip（前缀白名单）。
func gzipEligible(ct string) bool {
	ct = strings.ToLower(strings.TrimSpace(ct))
	for _, p := range gzipContentTypes {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func GzipMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	log := slogutil.Default(logger)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
				next.ServeHTTP(w, r)
				return
			}
			// WebSocket 升级面跳过（/ws 路径）：gzip writer 吞掉 Hijacker 会致
			// 升级失败（101→501，e2e relay ws 实测）；普通文件下载不受影响。
			if r.URL.Path == "/ws" || strings.HasPrefix(r.URL.Path, "/ws/") {
				next.ServeHTTP(w, r)
				return
			}
			// Content-Type 白名单（按内容类型自动 gzip；非文本类不压缩）。
			if ct := w.Header().Get("Content-Type"); ct != "" && !gzipEligible(ct) {
				next.ServeHTTP(w, r)
				return
			}
			gw, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			gzw := &gzipResponseWriter{Writer: gw, ResponseWriter: w}
			next.ServeHTTP(gzw, r)
			if gzw.statusCode >= 400 {
				return
			}
			if err := gw.Close(); err != nil {
				log.Warn("关闭 gzip writer 失败", "error", err)
			}
		})
	}
}
