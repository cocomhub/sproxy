// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"strconv"
	"strings"
)

// defaultMaxAge 是 CORS 预检请求的默认缓存时间（秒）。
const defaultMaxAge = 86400

// defaultAllowedHeaders 是默认允许的请求头列表。
const defaultAllowedHeaders = "Authorization, Content-Type, X-File-Checksum, X-File-Path, X-File-MTime, Range"

// CORSConfig 定义 CORS 跨域配置。
type CORSConfig struct {
	// AllowedOrigins 允许的跨域来源列表，设置 ["*"] 允许任意来源。
	// 为空时 CORS 中间件直接透传（保持向后兼容）。
	AllowedOrigins []string `yaml:"allowed_origins" mapstructure:"allowed_origins"`
	// AllowedHeaders 允许的请求头列表，为空时使用默认值。
	AllowedHeaders []string `yaml:"allowed_headers" mapstructure:"allowed_headers"`
	// MaxAge 预检请求缓存时间（秒），默认 86400。
	MaxAge int `yaml:"max_age" mapstructure:"max_age"`
}

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

	log := defaultLogger(logger)
	maxAge := cfg.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
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
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			w.Header().Set("Access-Control-Expose-Headers", "X-File-Checksum, X-File-Size, X-File-MTime, X-File-IsDir, Content-Range, Content-Disposition")
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(maxAge))

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
