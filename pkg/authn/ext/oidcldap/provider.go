// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package oidcldap 是 OIDC（Authorization Code + PKCE）与 LDAP（绑定）外部认证的
// **独立 go module**（ext 隔离，roadmap 11.7-⑥）。
//
// 设计要点（docs/designs/2026-09-24-oidc-ldap.md）：
//   - 本 module 是独立 go.mod（与 pkg/tunnel/xfer/ext/* 同款），只依赖
//     golang.org/x/oauth2（x/ 准标准库）与 go-ldap/ldap/v3（唯一第三方，ext 隔离评审
//     已放行）；核心库（pkg/server）零新依赖。
//   - 认证器实现 pkg/authn.Authenticator（G0 契约包，非装配层）——不 import
//     pkg/server（R4 分层：领域包不得导入装配层）。
//   - 装配层（cmd/sproxy）按 cfg.ExternalAuth 构造本包提供者，经
//     RegisterRoutesOpts.Authenticators + ExternalAuthHandlers 宿主注入；未配置
//     （enabled=false）→ 不装配（零回归：无外部端点、认证链默认链不变）。
//   - 外部身份无 SproxySig SK → Principal.Secret 空 → /tunnel 不建立（文档明示：
//     OIDC/LDAP 身份走直连 HTTP 面）。
//   - 会话：服务端签发 HMAC 签名 cookie（pkg/authn.SessionManager），与服务端既有
//     凭据体系完全隔离（不污染 SK Ring）。
package oidcldap

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/url"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/authn"
)

// Config 是外部认证装配配置（与 pkg/server.AuthExternalConfig 同构；独立 module 不
// import server 包，故在装配边界由 cmd/sproxy 做字段拷贝，避免重复依赖）。
type Config struct {
	// OIDC 是 OIDC 提供者配置。
	OIDC OIDCConfig
	// LDAP 是 LDAP 提供者配置。
	LDAP LDAPConfig
	// SessionSecret 是会话 cookie HMAC 密钥（32B；空 = 进程内随机，重启掉线）。
	SessionSecret string
	// SessionTTL 是会话有效期（<=0 默认 24h）。
	SessionTTL int64
	// SecureCookie 为 true 时会话 cookie 仅 HTTPS 传输（装配层按 cfg.TLS.Enabled 传入）。
	SecureCookie bool
	// Logger 是业务日志器（nil 回落 slog.Default）。
	Logger *slog.Logger
}

// OIDCConfig 是 OIDC 提供者配置。
type OIDCConfig struct {
	Enabled       bool
	Issuer        string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	ClaimOwner    string
	AutoProvision bool
}

// LDAPConfig 是 LDAP 提供者配置。
type LDAPConfig struct {
	Enabled      bool
	URL          string
	BindDN       string
	BindPassword string
	BaseDN       string
	UserFilter   string
	UsernameAttr string
}

// Provider 聚合 OIDC 与 LDAP 两个认证器 + 登录面路由 + 会话管理器，
// 供装配层（cmd/sproxy）注入 pkg/server。
type Provider struct {
	mu        sync.Mutex
	oidc      *OIDCAuthenticator
	ldap      *LDAPAuthenticator
	sessions  *authn.SessionManager
	logger    *slog.Logger
	closed    bool
	closeOnce sync.Once
}

// New 按配置构造外部认证提供者。全部 disabled → 返回 nil（装配层不注入，零回归）。
// 任一段 enabled 但关键字段缺失 → 返回错误（装配层 fail-closed，不静默降级）。
func New(cfg Config) (*Provider, error) {
	if !cfg.OIDC.Enabled && !cfg.LDAP.Enabled {
		return nil, nil // 未配置不启用（零回归）
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	sessions, err := newSessionManager(cfg)
	if err != nil {
		return nil, err
	}
	p := &Provider{sessions: sessions, logger: log}
	if cfg.OIDC.Enabled {
		o, oerr := NewOIDCAuthenticator(cfg.OIDC, sessions, log)
		if oerr != nil {
			return nil, oerr
		}
		p.oidc = o
	}
	if cfg.LDAP.Enabled {
		l, lerr := NewLDAPAuthenticator(cfg.LDAP, sessions, log)
		if lerr != nil {
			return nil, lerr
		}
		p.ldap = l
	}
	return p, nil
}

// newSessionManager 从装配配置构造会话管理器（key 解析：base64 32B 或 raw 32B）。
func newSessionManager(cfg Config) (*authn.SessionManager, error) {
	key, err := parseSessionKey(cfg.SessionSecret)
	if err != nil {
		return nil, err
	}
	ttl := cfg.SessionTTL
	if ttl <= 0 {
		ttl = 24 * 3600
	}
	return authn.NewSessionManager(key, time.Duration(ttl)*time.Second, cfg.SecureCookie), nil
}

// parseSessionKey 解析会话密钥：base64（Std/RawURL/URL 三形态）32B 或 raw 32B；空 → nil（回落随机）。
func parseSessionKey(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	if b, err := decodeKey(s); err == nil && len(b) == 32 {
		return b, nil
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, fmt.Errorf("oidcldap: session_secret 需为 32B（base64 或 raw），当前 %d 字符", len(s))
}

// decodeKey 尝试三种 base64 形态解码（Std / RawURL / URL）。
func decodeKey(s string) ([]byte, error) {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return nil, fmt.Errorf("oidcldap: session_secret 非合法 base64")
}

// Authenticators 返回认证链成员（已装配的子认证器；nil = 无）。
func (p *Provider) Authenticators() []authn.Authenticator {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []authn.Authenticator
	if p.oidc != nil {
		out = append(out, p.oidc)
	}
	if p.ldap != nil {
		out = append(out, p.ldap)
	}
	return out
}

// Routes 返回登录面路由（主 mux + localMux 双注册）。
func (p *Provider) Routes() []authn.ExternalAuthRoute {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []authn.ExternalAuthRoute
	if p.oidc != nil {
		out = append(out, p.oidc.Routes()...)
	}
	if p.ldap != nil {
		out = append(out, p.ldap.Routes()...)
	}
	return out
}

// Close 释放提供者资源（幂等）。
func (p *Provider) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.mu.Unlock()
	})
	return nil
}

// IsOIDCEnabled / IsLDAPEnabled 供装配层日志/诊断。
func (p *Provider) IsOIDCEnabled() bool { return p.oidc != nil }
func (p *Provider) IsLDAPEnabled() bool { return p.ldap != nil }

// validateURL 校验 http(s) 且带 host（OIDC issuer 与回调共用）。
func validateURL(s string, what string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("oidcldap: %s %q 非法（应为 http(s)://host[:port]）", what, s)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("oidcldap: %s %q scheme 仅允许 http/https", what, s)
	}
	return nil
}

// logContext 保持 context 引用（供未来扩展请求级超时）。
var _ = context.Background
