// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/subtle"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// meshCtxKey 是请求 ctx 中 mesh 的私有 key 类型（避免与其他包/库的 string key 碰撞）。
type meshCtxKey struct{}

// withMesh 把 mesh 写入请求 ctx。
func withMesh(ctx context.Context, mesh string) context.Context {
	return context.WithValue(ctx, meshCtxKey{}, mesh)
}

// MeshFrom 返回请求 ctx 中的 mesh（未设置时返回 ""）。
// authMiddleware 在 SproxySig 验签成功后按命中 AK 派生 mesh 写入 ctx；
// 供 /api/hub/nodes、信令、metrics 按 mesh 过滤。
func MeshFrom(ctx context.Context) string {
	mesh, _ := ctx.Value(meshCtxKey{}).(string)
	return mesh
}

// meshFromRequest 从请求 ctx 读取调用方所属 mesh（无则返回 ""）。
func meshFromRequest(r *http.Request) string {
	return MeshFrom(r.Context())
}

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

// drainAndVerifyBody 强制消费请求体剩余部分，触发 SproxySig bodyValidator 的 EOF 哈希比对（I-3）。
// json.Decoder / ParseMultipartForm 读到自身需要的数据后即返回、不读到 EOF，导致 bodyValidator
// 的哈希比对永不触发；此处兜底读完整个 body——body 被篡改（哈希不匹配）时返回错误，
// 调用方应在响应前拒绝（400）。合法 body 读到 EOF 校验通过，无副作用。
func drainAndVerifyBody(r *http.Request) error {
	_, err := io.Copy(io.Discard, r.Body)
	return err
}

// verifySproxySig 校验 SproxySig 请求签名（AccessKey/AccessKeySecret + HMAC-SHA256）。
// 成功时返回命中的 *AccessKeyConfig（供隧道密钥派生复用，消除二次遍历/TOCTOU，M-10），
// 并用 body 哈希校验 reader 包装 r.Body：流式接收、EOF 与声明比对（防 body 篡改；
// 验签已在 body 接收前用声明哈希完成，失败即 401 无回滚）。
func (h *Handlers) verifySproxySig(w http.ResponseWriter, r *http.Request, cfg *Config) (*AccessKeyConfig, bool) {
	hdr, err := sproxysig.ParseHeader(r.Header.Get("Authorization"))
	if err != nil {
		slog.Warn("auth: 非法 SproxySig 头",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	var matched *AccessKeyConfig
	for i := range cfg.AccessKeys {
		if subtle.ConstantTimeCompare([]byte(cfg.AccessKeys[i].Key), []byte(hdr.AK)) == 1 {
			matched = &cfg.AccessKeys[i]
			break
		}
	}
	if matched == nil {
		slog.Warn("auth: 未知 AccessKey", "ak", hdr.AK, "remote", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	var nonceSeen func(ak, nonce string, expMs int64) bool
	if h.noncePool != nil {
		nonceSeen = h.noncePool.Seen
	}
	if verr := sproxysig.Verify(matched.Secret, hdr, r.Method, r.URL.EscapedPath(), r.URL.RawQuery, time.Now(), 0, 0, nonceSeen); verr != nil {
		slog.Warn("auth: SproxySig 校验失败", "ak", hdr.AK, "error", verr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil, false
	}
	r.Body = io.NopCloser(sproxysig.NewBodyValidator(r.Body, hdr.BodySHA256))
	return matched, true
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
			matched, ok := h.verifySproxySig(w, r, cfg)
			if !ok {
				return
			}
			// M-9：验签成功后按命中 AK 派生 mesh 写入 ctx，供列表/信令/指标按 mesh 过滤。
			r = r.WithContext(withMesh(r.Context(), tunnel.AccessKeyMesh(matched.Key)))
			// I-3：bodyValidator 只在读到 io.EOF 时比对哈希，而 JSON 端点用
			// json.Decoder、上传用 ParseMultipartForm 都不读到 EOF，哈希比对永不触发。
			// handler 完成后强制消费剩余 body 触发 EOF 校验；不匹配记 Warn（响应已发，
			// 无法改状态码，但防篡改意图得以执行并留痕）。
			defer func() {
				if _, derr := io.Copy(io.Discard, r.Body); derr != nil {
					slog.Warn("auth: body 哈希校验失败", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "error", derr)
				}
			}()
			// /tunnel：验签成功后按命中 AK → HKDF 派生隧道密钥放入 ctx，
			// 隧道 handler 用 ctx 密钥解密 metadata 与 body；普通 API 请求走下面分支。
			if r.URL.Path == "/tunnel" {
				sepKey, err := h.tunnelDerivedKey(r, matched)
				if err != nil {
					slog.Warn("auth: 派生隧道密钥失败", "error", err)
					http.Error(w, "隧道密钥派生失败", http.StatusInternalServerError)
					return
				}
				next(w, r.WithContext(tunnel.SetTunnelKey(r.Context(), sepKey)))
				return
			}
			next(w, r)
			return
		}

		next(w, r) // 无认证配置 → 放行
	}
}

// tunnelDerivedKey 用 verifySproxySig 已命中的 AccessKeyConfig 的 SK HKDF 派生隧道密钥。
// v1：AK/SK 对称，SK 即 AES 隧道密钥派生源（golang.org/x/crypto/hkdf）。
// mesh 用共享 tunnel.AccessKeyMesh(ak.Key) 解析（与 sclient 一致，消除配置漂移 I-1）；
// 显式配置的 mesh_id 由 Config.Validate 校验必须与 AK 内嵌 mesh 一致。
func (h *Handlers) tunnelDerivedKey(r *http.Request, ak *AccessKeyConfig) ([]byte, error) {
	return tunnel.DeriveTunnelKey(ak.Secret, tunnel.AccessKeyMesh(ak.Key))
}
