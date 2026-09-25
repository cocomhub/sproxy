// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/go-ldap/ldap/v3"
)

// ldapLoginPath 是 LDAP 绑定登录端点（POST {username, password}）。
const ldapLoginPath = "/auth/ldap/login"

// LDAPAuthenticator 是 LDAP 绑定认证器。
//
// 数据面（设计文档）：
//   - POST /auth/ldap/login {username, password}：服务账号 bind（BindDN 可空 = 匿名
//     search）→ 按 UserFilter 搜用户 DN → 用户 DN bind 验证口令 → 成功映射 owner →
//     发会话 cookie。
//   - Authenticate：会话 cookie 校验（登录成功后）→ Principal。
//
// 失败语义：bind/search 失败 → 401（统一文案，不泄露账号存在性）；ldaps 证书校验
// 失败 → 拒绝（不提供 InsecureSkipVerify）。
type LDAPAuthenticator struct {
	mu           sync.Mutex
	url          string
	bindDN       string
	bindPass     string
	baseDN       string
	userFilter   string
	usernameAttr string
	sess         *authn.SessionManager
	logger       *slog.Logger
	// dial 是可注入的 LDAP 连接构造器（测试 mock；nil = go-ldap DialURL 默认）。
	dial func(url string) (ldapConn, error)
}

// ldapConn 是 LDAP 连接的窄接口（go-ldap *ldap.Conn 满足；测试可 mock）。
type ldapConn interface {
	Bind(username, password string) error
	Search(req *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// NewLDAPAuthenticator 构造 LDAP 认证器。cfg.URL 必填（ldap:// 或 ldaps://）。
func NewLDAPAuthenticator(cfg LDAPConfig, sess *authn.SessionManager, logger *slog.Logger) (*LDAPAuthenticator, error) {
	if cfg.URL == "" {
		return nil, fmt.Errorf("oidcldap: ldap.url 不能为空（enabled=true）")
	}
	if !strings.HasPrefix(cfg.URL, "ldap://") && !strings.HasPrefix(cfg.URL, "ldaps://") {
		return nil, fmt.Errorf("oidcldap: ldap.url %q 仅支持 ldap:// 或 ldaps://", cfg.URL)
	}
	if cfg.BaseDN == "" {
		return nil, fmt.Errorf("oidcldap: ldap.base_dn 不能为空（enabled=true）")
	}
	filter := cfg.UserFilter
	if filter == "" {
		filter = "(&(objectClass=inetOrgPerson)(uid={{username}}))"
	}
	attr := cfg.UsernameAttr
	if attr == "" {
		attr = "uid"
	}
	return &LDAPAuthenticator{
		url:          cfg.URL,
		bindDN:       cfg.BindDN,
		bindPass:     cfg.BindPassword,
		baseDN:       cfg.BaseDN,
		userFilter:   filter,
		usernameAttr: attr,
		sess:         sess,
		logger:       logger,
	}, nil
}

// Name 返回认证器名称。
func (a *LDAPAuthenticator) Name() string { return "ldap" }

// Routes 返回 LDAP 登录面路由（主 mux + localMux 双注册）。
func (a *LDAPAuthenticator) Routes() []authn.ExternalAuthRoute {
	return []authn.ExternalAuthRoute{
		{Method: http.MethodPost, Pattern: ldapLoginPath, Handler: http.HandlerFunc(a.handleLogin)},
	}
}

// Authenticate 校验请求 LDAP 身份（会话 cookie）。
func (a *LDAPAuthenticator) Authenticate(_ context.Context, r *http.Request) (*authn.Principal, error) {
	if ak, owner, ok := a.sess.Lookup(r); ok {
		return &authn.Principal{AK: ak, Owner: owner, Role: "user"}, nil
	}
	return nil, fmt.Errorf("oidcldap: ldap 无会话")
}

// dialConn 返回 LDAP 连接（注入 dial 优先；否则 go-ldap DialURL）。
func (a *LDAPAuthenticator) dialConn(url string) (ldapConn, error) {
	if a.dial != nil {
		return a.dial(url)
	}
	conn, err := ldap.DialURL(url)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// handleLogin 处理 POST /auth/ldap/login {username, password}。
func (a *LDAPAuthenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSONBody(r, &req); err != nil {
		http.Error(w, "登录失败", http.StatusBadRequest)
		return
	}
	if req.Username == "" || req.Password == "" {
		http.Error(w, "登录失败", http.StatusBadRequest)
		return
	}
	owner, ak, err := a.bindUser(r.Context(), req.Username, req.Password)
	if err != nil {
		a.logger.Warn("ldap: 登录失败", "error", err.Error())
		// 统一文案（不泄露账号存在性，与 TOTP login 同款）。
		http.Error(w, "登录失败", http.StatusUnauthorized)
		return
	}
	if err := a.sess.Issue(w, ak, owner); err != nil {
		http.Error(w, "登录失败", http.StatusInternalServerError)
		return
	}
	a.logger.Info("ldap: 登录成功", "ak", ak, "owner", owner)
	sendJSON(w, map[string]any{"ok": true})
}

// bindUser 执行 LDAP 绑定认证，返回 (owner, ak, error)。
// 服务账号 bind → 搜索用户 DN → 用户 DN bind 验证口令。
func (a *LDAPAuthenticator) bindUser(ctx context.Context, username, password string) (string, string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	conn, err := a.dialConn(a.url)
	if err != nil {
		return "", "", fmt.Errorf("ldap 连接失败: %w", err)
	}
	defer conn.Close()
	_ = ctx // 连接级超时由 go-ldap DialURL 的默认配置承担（未来可注入拨号 ctx）

	// 1. 服务账号 bind（可空 = 匿名）。
	if a.bindDN != "" {
		if berr := conn.Bind(a.bindDN, a.bindPass); berr != nil {
			return "", "", fmt.Errorf("ldap 服务账号 bind 失败: %w", berr)
		}
	} else {
		if berr := conn.Bind("", ""); berr != nil {
			return "", "", fmt.Errorf("ldap 匿名 bind 失败: %w", berr)
		}
	}
	// 2. 搜索用户 DN。
	filter := strings.ReplaceAll(a.userFilter, "{{username}}", ldap.EscapeFilter(username))
	req := ldap.NewSearchRequest(
		a.baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 0, 0, false,
		filter,
		[]string{"dn", a.usernameAttr},
		nil,
	)
	res, err := conn.Search(req)
	if err != nil {
		return "", "", fmt.Errorf("ldap 搜索失败: %w", err)
	}
	if len(res.Entries) == 0 {
		return "", "", fmt.Errorf("ldap 用户不存在")
	}
	dn := res.Entries[0].DN
	// 3. 用户 DN bind 验证口令。
	if err := conn.Bind(dn, password); err != nil {
		return "", "", fmt.Errorf("ldap 用户 bind 失败: %w", err)
	}
	owner := username
	if v := res.Entries[0].GetAttributeValue(a.usernameAttr); v != "" {
		owner = v
	}
	ak := ldapAK(username)
	return owner, ak, nil
}

// ldapAK 由用户名派生稳定 AK（合法段名，跨重启稳定）。
func ldapAK(username string) string {
	sum := sha256.Sum256([]byte("ldap:" + username))
	return "ldap-" + fmt.Sprintf("%x", sum[:8])
}

// decodeJSONBody 解码 JSON 请求体（限长 1KB）。
func decodeJSONBody(r *http.Request, v any) error {
	return jsonDecode(r, v)
}

// sendJSON 写 JSON 响应。
func sendJSON(w http.ResponseWriter, v any) {
	writeJSON(w, v)
}

// _ 保持 base64 引用（LDAP 属性值可能 base64 编码）。
var _ = base64.StdEncoding
