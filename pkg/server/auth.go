// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
)

// APIKey 表示一个 API 密钥及其权限。
type APIKey struct {
	Name       string `yaml:"name" mapstructure:"name"`
	Key        string `yaml:"key" mapstructure:"key"`
	Permission string `yaml:"permission" mapstructure:"permission"` // "read" 或 "write"；空字符串默认按 "write" 处理
}

const (
	// PermissionRead 表示只读权限。
	PermissionRead = "read"
	// PermissionWrite 表示读写权限。
	PermissionWrite = "write"
)

// APIKeyConfig 多用户 API 密钥配置。
type APIKeyConfig struct {
	Enabled bool     `yaml:"enabled" mapstructure:"enabled"`
	Keys    []APIKey `yaml:"keys" mapstructure:"keys"`
}

// AccessKeyConfig 是 SproxySig 请求签名认证的一对 AccessKey/AccessKeySecret
// （替代旧 auth_token 明文 Bearer）。每 mesh 一对；Secret 只存本端用于验签，
// 线上请求只携带 Key + HMAC 签名。
type AccessKeyConfig struct {
	Key    string `yaml:"key" mapstructure:"key"`         // AccessKey（公开标识）
	Secret string `yaml:"secret" mapstructure:"secret"`   // AccessKeySecret（本地密钥）
	MeshID string `yaml:"mesh_id" mapstructure:"mesh_id"` // 所属 mesh（多 mesh 隔离，可选）
}

// authResult 表示 API key 匹配结果。
type authResult int

const (
	authResultOK        authResult = iota // 匹配成功且权限允许
	authResultForbidden                   // 匹配成功但权限不足
	authResultDenied                      // 不匹配任何 key
)

// permissionAllowed 检查给定的权限是否允许执行所需操作。
// PermissionRead 权限可执行 GET/HEAD 请求；PermissionWrite 权限可执行所有操作。
// 空字符串（""）按 PermissionWrite 处理（兼容旧配置）。
func permissionAllowed(permission, method string) bool {
	if permission == PermissionWrite || permission == "" {
		return true
	}
	if permission == PermissionRead {
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
		if key.Key == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(token), []byte(key.Key)) == 1 {
			if permissionAllowed(key.Permission, method) {
				return authResultOK
			}
			return authResultForbidden
		}
	}
	return authResultDenied
}

// handleNoBearerToken 处理缺少 Bearer Authorization 头的情况（仅多用户 APIKeys 场景）。
func handleNoBearerToken(w http.ResponseWriter, r *http.Request, cfg *Config, next http.HandlerFunc) {
	if cfg.APIKeys.Enabled {
		slog.Warn("auth: missing bearer token",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	next(w, r)
}

// authenticateAPIKey 校验多用户 API 密钥（Bearer，独立特性）。
func (h *Handlers) authenticateAPIKey(w http.ResponseWriter, r *http.Request, cfg *Config, token string, next http.HandlerFunc) {
	switch matchAPIKey(token, r.Method, cfg.APIKeys.Keys) {
	case authResultOK:
		next(w, r)
	case authResultForbidden:
		slog.Warn("auth: permission denied",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		http.Error(w, "permission denied", http.StatusForbidden)
	default:
		// authResultDenied: APIKeys 已启用但 token 不匹配任何 key，直接拒绝
		slog.Warn("auth: no matching api key",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
		)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "unauthorized"}, http.StatusUnauthorized)
	}
}

// verifySproxySig 校验 SproxySig 请求签名（AccessKey/AccessKeySecret + HMAC-SHA256）。
// 成功时用 body 哈希校验 reader 包装 r.Body：流式接收、EOF 与声明比对（防 body 篡改；
// 验签已在 body 接收前用声明哈希完成，失败即 401 无回滚）。
func (h *Handlers) verifySproxySig(w http.ResponseWriter, r *http.Request, cfg *Config) bool {
	hdr, err := sproxysig.ParseHeader(r.Header.Get("Authorization"))
	if err != nil {
		slog.Warn("auth: 非法 SproxySig 头",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	var sk string
	for _, ak := range cfg.AccessKeys {
		if subtle.ConstantTimeCompare([]byte(ak.Key), []byte(hdr.AK)) == 1 {
			sk = ak.Secret
			break
		}
	}
	if sk == "" {
		slog.Warn("auth: 未知 AccessKey", "ak", hdr.AK, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	var nonceSeen func(ak, nonce string, expMs int64) bool
	if h.noncePool != nil {
		nonceSeen = h.noncePool.Seen
	}
	if verr := sproxysig.Verify(sk, hdr, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, time.Now(), 0, 0, nonceSeen); verr != nil {
		slog.Warn("auth: SproxySig 校验失败", "ak", hdr.AK, "error", verr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	r.Body = io.NopCloser(sproxysig.NewBodyValidator(r.Body, hdr.BodySHA256))
	return true
}

// authMiddleware 验证请求认证：
//   - api_keys.enabled → 多用户 API 密钥（Bearer，独立特性）；
//   - access_keys 已配置 → SproxySig 请求签名（AK/SK，替代旧 auth_token 明文 Bearer）；
//   - 均未配置 → 放行（启动日志负责无认证告警）。
func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil {
			slog.Error("auth: server configuration not loaded")
			http.Error(w, "server configuration not loaded", http.StatusInternalServerError)
			return
		}

		if cfg.APIKeys.Enabled {
			auth := r.Header.Get("Authorization")
			if !strings.HasPrefix(auth, "Bearer ") {
				handleNoBearerToken(w, r, cfg, next)
				return
			}
			token := strings.TrimPrefix(auth, "Bearer ")
			if token == "" {
				slog.Warn("auth: empty bearer token",
					"remote", r.RemoteAddr,
					"method", r.Method,
					"path", r.URL.Path,
				)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			h.authenticateAPIKey(w, r, cfg, token, next)
			return
		}

		if len(cfg.AccessKeys) > 0 {
			if h.verifySproxySig(w, r, cfg) {
				next(w, r)
			}
			return
		}

		next(w, r) // 无认证配置 → 放行
	}
}
