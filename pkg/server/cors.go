// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"net/http"
	"strconv"
)

// CORSConfig 定义 CORS 跨域配置。
type CORSConfig struct {
	// AllowedOrigins 允许的跨域来源列表，设置 ["*"] 允许任意来源。
	// 为空时 CORS 中间件直接透传（保持向后兼容）。
	AllowedOrigins []string `yaml:"allowed_origins" mapstructure:"allowed_origins"`
	// MaxAge 预检请求缓存时间（秒），默认 86400。
	MaxAge int `yaml:"max_age" mapstructure:"max_age"`
}

// CORSMiddleware 返回一个 HTTP 中间件，根据配置添加 CORS 头部并处理 OPTIONS 预检请求。
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
		maxAge = 86400
	}

	// 构建 origin 查找集合
	originSet := make(map[string]bool)
	allowAll := false
	for _, o := range cfg.AllowedOrigins {
		if o == "*" {
			allowAll = true
			break
		}
		originSet[o] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				// 非浏览器请求，无需 CORS
				next.ServeHTTP(w, r)
				return
			}

			// 判断是否允许该 origin
			if allowAll {
				w.Header().Set("Access-Control-Allow-Origin", "*")
			} else if originSet[origin] {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			} else {
				// origin 不在白名单中，不添加 CORS 头（浏览器会阻止请求）
				log.Warn("rejected CORS origin", "origin", origin)
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, HEAD, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-File-Checksum, X-File-Path, X-File-MTime, Range")
			w.Header().Set("Access-Control-Expose-Headers", "X-File-Checksum, X-File-Size, X-File-MTime, X-File-IsDir, Content-Range, Content-Disposition")
			w.Header().Set("Access-Control-Max-Age", strconv.Itoa(maxAge))

			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
