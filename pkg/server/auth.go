// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// APIKey 表示一个 API 密钥及其权限。
type APIKey struct {
	Name       string `yaml:"name" mapstructure:"name"`
	Key        string `yaml:"key" mapstructure:"key"`
	Permission string `yaml:"permission" mapstructure:"permission"` // "read" 或 "write"
}

// APIKeyConfig 多用户 API 密钥配置。
type APIKeyConfig struct {
	Enabled bool     `yaml:"enabled" mapstructure:"enabled"`
	Keys    []APIKey `yaml:"keys" mapstructure:"keys"`
}

// permissionAllowed 检查给定的权限是否允许执行所需操作。
// read 权限可执行 GET/HEAD 请求；write 权限可执行所有操作。
func permissionAllowed(permission, method string) bool {
	if permission == "write" {
		return true
	}
	if permission == "read" {
		switch method {
		case http.MethodGet, http.MethodHead:
			return true
		}
		return false
	}
	return false
}

// authMiddleware 验证请求认证和权限。
// 优先匹配多用户 API 密钥（api_keys.enabled=true），
// 其次回退到单用户 auth_token。
func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil {
			next(w, r)
			return
		}

		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			// 无认证令牌时，如果任一认证配置启用则拒绝
			if cfg.AuthToken != "" || (cfg.APIKeys.Enabled && len(cfg.APIKeys.Keys) > 0) {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		// 优先匹配多用户 API 密钥
		if cfg.APIKeys.Enabled {
			for _, key := range cfg.APIKeys.Keys {
				if subtle.ConstantTimeCompare([]byte(token), []byte(key.Key)) == 1 {
					if permissionAllowed(key.Permission, r.Method) {
						next(w, r)
						return
					}
					http.Error(w, "permission denied", http.StatusForbidden)
					return
				}
			}
			// API key 模式启用但 token 不匹配任何 key，仍尝试回退 auth_token
		}

		// 回退到单用户 auth_token
		if cfg.AuthToken != "" {
			if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AuthToken)) == 1 {
				next(w, r)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		// APIKeys 已启用但无匹配 key，且 auth_token 为空
		if cfg.APIKeys.Enabled {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next(w, r)
	}
}
