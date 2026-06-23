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

// authResult 表示 API key 匹配结果。
type authResult int

const (
	authResultOK        authResult = iota // 匹配成功且权限允许
	authResultForbidden                   // 匹配成功但权限不足
	authResultDenied                      // 不匹配任何 key
)

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

// matchAPIKey 遍历 API 密钥列表，尝试匹配 token。
// 返回 authResultOK — 匹配成功且权限允许；
// 返回 authResultForbidden — 匹配成功但权限不足；
// 返回 authResultDenied — 不匹配任何 key。
func matchAPIKey(token, method string, keys []APIKey) authResult {
	for _, key := range keys {
		if subtle.ConstantTimeCompare([]byte(token), []byte(key.Key)) == 1 {
			if permissionAllowed(key.Permission, method) {
				return authResultOK
			}
			return authResultForbidden
		}
	}
	return authResultDenied
}

// handleNoBearerToken 处理缺少 Bearer Authorization 头的情况。
// 如果任一认证配置启用则拒绝，否则放行。
func handleNoBearerToken(w http.ResponseWriter, r *http.Request, cfg *Config, next http.HandlerFunc) bool {
	if cfg.AuthToken != "" || (cfg.APIKeys.Enabled && len(cfg.APIKeys.Keys) > 0) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	next(w, r)
	return true
}

// authenticateRequest 执行请求认证，返回 true 表示已处理（放行或拒绝），false 表示需要继续（仅用于 APIKeys 没有匹配时的 fallthrough）。
func (h *Handlers) authenticateRequest(w http.ResponseWriter, r *http.Request, cfg *Config, token string, next http.HandlerFunc) bool {
	// 优先匹配多用户 API 密钥
	if cfg.APIKeys.Enabled {
		switch matchAPIKey(token, r.Method, cfg.APIKeys.Keys) {
		case authResultOK:
			next(w, r)
			return true
		case authResultForbidden:
			http.Error(w, "permission denied", http.StatusForbidden)
			return true
		}
		// authResultDenied: 不匹配任何 key，继续回退 auth_token
	}

	// 回退到单用户 auth_token
	if cfg.AuthToken != "" {
		if subtle.ConstantTimeCompare([]byte(token), []byte(cfg.AuthToken)) == 1 {
			next(w, r)
			return true
		}
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return true
	}

	// APIKeys enabled but token didn't match and no auth_token configured
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return true
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
			if handleNoBearerToken(w, r, cfg, next) {
				return
			}
			return
		}
		token := strings.TrimPrefix(auth, "Bearer ")

		h.authenticateRequest(w, r, cfg, token, next)
	}
}
