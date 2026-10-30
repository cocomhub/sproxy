// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// defaultAllowedHeaders 是默认允许的请求头列表。
const defaultAllowedHeaders = "Authorization, Content-Type, X-File-Checksum, X-File-Path, X-File-MTime, Range"

// CORSMiddleware 返回一个 HTTP 中间件，根据配置添加 CORS 头部并处理 OPTIONS 预检请求。
// 支持的方法：GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS。
// 当 AllowedOrigins 为空时直接透传，保持向后兼容。
func CORSMiddleware(cfg CORSConfig, logger *slog.Logger) func(http.Handler) http.Handler {
	if len(cfg.AllowedOrigins) == 0 {
		// 未配置，直接透传（保持向后兼容）
		return func(next http.Handler) http.Handler {
			return next
		}
	}

	log := slogutil.Default(logger)
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}

	allowedHeaders := cfg.AllowedHeaders
	if len(allowedHeaders) == 0 {
		allowedHeaders = strings.Split(defaultAllowedHeaders, ", ")
	}

	// 构建 origin 查找集合，统一转为小写以实现大小写不敏感匹配
	originSet := make(map[string]bool)
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		originSet[strings.ToLower(o)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(corsServe(originSet, allowAll, allowedHeaders, maxAge, log, next))
	}
}

// corsServe 处理单请求的 CORS 头与 OPTIONS 预检逻辑。
// origin 未携带直接透传；不在白名单时拒绝（OPTIONS 回 204，其余 403）；命中白名单
// 则设置允许头并透传。origin 大小写不敏感。
func corsServe(originSet map[string]bool, allowAll bool, allowedHeaders []string, maxAge int, log *slog.Logger, next http.Handler) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// 非浏览器请求，无需 CORS
			next.ServeHTTP(w, r)
			return
		}

		// 判断是否允许该 origin（大小写不敏感）
		switch {
		case allowAll:
			// 通配符 "*"：直接设置，不反射 origin，不设 Allow-Credentials
			w.Header().Set("Access-Control-Allow-Origin", "*")
		case originSet[strings.ToLower(origin)]:
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
		default:
			// origin 不在白名单中
			if r.Method == http.MethodOptions {
				// OPTIONS 预检请求：返回 204，不设 CORS 头（浏览器不会缓存该结果）
				w.WriteHeader(http.StatusNoContent)
				return
			}
			log.Warn("rejected CORS origin", "origin", origin)
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}

		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", strings.Join(allowedHeaders, ", "))
		w.Header().Set("Access-Control-Expose-Headers", "X-File-Checksum, X-File-Size, X-File-MTime, X-File-IsDir, X-Volume, Content-Range, Content-Disposition")
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(maxAge))

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	}
}
