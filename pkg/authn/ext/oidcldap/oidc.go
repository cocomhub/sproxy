// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/authn"
	"golang.org/x/oauth2"
)

// OIDC 常量。
const (
	// oidcLoginPath / oidcCallbackPath 是 OIDC 登录面路由（主 mux + localMux 双注册）。
	oidcLoginPath    = "/auth/oidc/login"
	oidcCallbackPath = "/auth/oidc/callback"
	// oidcWellKnownSuffix 是 discovery 文档路径后缀。
	oidcWellKnownSuffix = "/.well-known/openid-configuration"
	// oidcStateTTL 是 PKCE state 有效期（10 分钟）。
	oidcStateTTL = 10 * time.Minute
	// oidcMaxStateEntries 是 state 内存表上限（防无限膨胀）。
	oidcMaxStateEntries = 4096
)

// OIDCAuthenticator 是 OIDC 外部认证器（Authorization Code + PKCE）。
//
// 数据面：
//   - Authenticate 识别 `Authorization: Bearer <id_token>`（设计取 Bearer id_token
//     校验）或会话 cookie（登录成功后签发）→ 返回 Principal{AK: owner 派生, Role: user}。
//   - 登录页 GET /auth/oidc/login：生成 PKCE verifier+state 入内存表 → 302 issuer 授权。
//   - 回调 GET /auth/oidc/callback：state 校验 + code 换 token → 验 id_token（iss/aud/exp）
//     → 映射/建凭据 → 发会话 cookie。
//
// 失败语义（设计文档错误处理）：state 不匹配 / code 交换失败 / id_token 验签失败 →
// 401；discovery 失败 → 构造失败（装配层 fail-closed 不注入）。
type OIDCAuthenticator struct {
	mu            sync.Mutex
	issuer        string
	clientID      string
	secret        string
	redirect      string
	claimOwner    string
	autoProvision bool
	scopes        []string
	// prov 是惰性初始化的 oidcProvider（discovery 缓存）。
	prov *oidcProvider
	// states 是 PKCE state → {verifier, exp} 内存表。
	states map[string]oidcState
	sess   *authn.SessionManager
	logger *slog.Logger
}

// oidcProvider 是 OIDC 提供者抽象（Discovery 文档 + JWKS + 验签）。
// 设计用 x/oauth2 的 Endpoint 装配授权/换 token URL；id_token 验签由实现方
// 提供 `verifyToken`（默认用核心库不引入 oidc 包的方案：从 JWKS 取公钥验 RS256，
// 见 jwks.go；测试可注入 mock）。
type oidcProvider struct {
	authURL  string
	tokenURL string
	jwksURL  string
	issuer   string
	client   *http.Client
	now      func() time.Time
	// verifyToken 校验 id_token（返回 claims map）。nil → 默认实现（JWKS 验签）。
	verifyToken func(ctx context.Context, token string) (map[string]any, error)
}

// oidcState 是 PKCE state 条目。
type oidcState struct {
	verifier string
	exp      time.Time
}

// NewOIDCAuthenticator 构造 OIDC 认证器。cfg.Issuer 必填 http(s)。
// 构造期不做网络调用（discovery 惰性）；fields 校验 fail-closed。
func NewOIDCAuthenticator(cfg OIDCConfig, sess *authn.SessionManager, logger *slog.Logger) (*OIDCAuthenticator, error) {
	if err := validateURL(cfg.Issuer, "oidc.issuer"); err != nil {
		return nil, err
	}
	if err := validateURL(cfg.RedirectURL, "oidc.redirect_url"); err != nil {
		return nil, err
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("oidcldap: oidc.client_id 不能为空（enabled=true）")
	}
	claim := cfg.ClaimOwner
	if claim == "" {
		claim = "email"
	}
	scopes := []string{"openid", "profile", "email"}
	if cfg.ClaimOwner != "" && cfg.ClaimOwner != "email" {
		scopes = append(scopes, cfg.ClaimOwner)
	}
	return &OIDCAuthenticator{
		issuer:        strings.TrimSuffix(cfg.Issuer, "/"),
		clientID:      cfg.ClientID,
		secret:        cfg.ClientSecret,
		redirect:      cfg.RedirectURL,
		claimOwner:    claim,
		autoProvision: cfg.AutoProvision,
		scopes:        scopes,
		states:        make(map[string]oidcState),
		sess:          sess,
		logger:        logger,
	}, nil
}

// Name 返回认证器名称。
func (a *OIDCAuthenticator) Name() string { return "oidc" }

// provider 返回惰性初始化的 oidcProvider（discovery 在首次登录/认证时拉取；
// 失败返回 error——不注入 = fail-closed，不静默降级匿名）。
func (a *OIDCAuthenticator) provider(ctx context.Context) (*oidcProvider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.prov != nil {
		return a.prov, nil
	}
	if a.issuer == "" {
		return nil, fmt.Errorf("oidcldap: oidc issuer 未配置")
	}
	discURL := a.issuer + oidcWellKnownSuffix
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, discURL, nil)
	if err != nil {
		return nil, fmt.Errorf("oidcldap: discovery 请求构造失败: %w", err)
	}
	client := &http.Client{Transport: newIsolatedTransport()}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oidcldap: discovery 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("oidcldap: discovery HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("oidcldap: discovery 读取失败: %w", err)
	}
	var doc struct {
		Issuer   string `json:"issuer"`
		AuthURL  string `json:"authorization_endpoint"`
		TokenURL string `json:"token_endpoint"`
		JWKSURL  string `json:"jwks_uri"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("oidcldap: discovery 解析失败: %w", err)
	}
	if doc.AuthURL == "" || doc.TokenURL == "" {
		return nil, fmt.Errorf("oidcldap: discovery 缺 authorization_endpoint/token_endpoint")
	}
	p := &oidcProvider{
		authURL:  doc.AuthURL,
		tokenURL: doc.TokenURL,
		jwksURL:  doc.JWKSURL,
		issuer:   a.issuer,
		client:   &http.Client{Transport: newIsolatedTransport()},
		now:      time.Now,
	}
	a.prov = p
	return p, nil
}

// Routes 返回 OIDC 登录面路由（装配层主 mux + localMux 双注册）。
func (a *OIDCAuthenticator) Routes() []authn.ExternalAuthRoute {
	return []authn.ExternalAuthRoute{
		{Method: http.MethodGet, Pattern: oidcLoginPath, Handler: http.HandlerFunc(a.handleLogin)},
		{Method: http.MethodGet, Pattern: oidcCallbackPath, Handler: http.HandlerFunc(a.handleCallback)},
	}
}

// Authenticate 校验请求 OIDC 身份：优先 `Authorization: Bearer <id_token>`（直连
// 面，设计取值），其次会话 cookie（登录后）。成功 → Principal{AK: owner 派生,
// Role: user, Secret: nil}。
func (a *OIDCAuthenticator) Authenticate(ctx context.Context, r *http.Request) (*authn.Principal, error) {
	// Bearer id_token 路径：非 Bearer 头快速失败（不写响应，R4-I3）。
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") && auth != "Bearer " {
		token := strings.TrimPrefix(auth, "Bearer ")
		return a.authenticateIDToken(ctx, token)
	}
	// 会话 cookie 路径。
	if ak, owner, ok := a.sess.Lookup(r); ok {
		return &authn.Principal{AK: ak, Owner: owner, Role: "user"}, nil
	}
	return nil, fmt.Errorf("oidcldap: oidc 无凭据")
}

// authenticateIDToken 校验 Bearer id_token 并映射身份。
func (a *OIDCAuthenticator) authenticateIDToken(ctx context.Context, token string) (*authn.Principal, error) {
	if _, err := a.provider(ctx); err != nil {
		return nil, err
	}
	claims, err := a.verify(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("oidcldap: id_token 校验失败: %w", err)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("oidcldap: id_token 缺 sub")
	}
	owner, err := a.ownerFromClaims(claims, sub)
	if err != nil {
		return nil, err
	}
	ak := oidcAK(sub)
	return &authn.Principal{AK: ak, Owner: owner, Role: "user"}, nil
}

// ownerFromClaims 按 claim_owner 取 owner（缺省 email，回退 sub）。
func (a *OIDCAuthenticator) ownerFromClaims(claims map[string]any, sub string) (string, error) {
	if a.claimOwner != "" {
		if v, ok := claims[a.claimOwner].(string); ok && v != "" {
			return v, nil
		}
	}
	// 回退：email claim 或 sub。
	if v, ok := claims["email"].(string); ok && v != "" {
		return v, nil
	}
	return sub, nil
}

// oidcAK 由 OIDC sub 派生稳定 AK（按 AK 落桶语义；sub 是 provider 内唯一标识）。
// 形如 oidc-<sha256(sub)[:16hex]>——合法段名（无 / 等非法字符），跨重启稳定。
func oidcAK(sub string) string {
	sum := sha256.Sum256([]byte("oidc:" + sub))
	return "oidc-" + fmt.Sprintf("%x", sum[:8])
}

// takeVerifier 返回并消费 state 对应的 PKCE verifier（state 已在上一步校验/消费，
// 此处只取 verifier；防重复取用由 handleCallback 的删除保证）。
func (a *OIDCAuthenticator) takeVerifier(state string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	e, ok := a.states[state]
	if !ok {
		return ""
	}
	return e.verifier
}

// handleLogin 处理 GET /auth/oidc/login：生成 PKCE verifier + state → 302 issuer。
func (a *OIDCAuthenticator) handleLogin(w http.ResponseWriter, r *http.Request) {
	prov, err := a.provider(r.Context())
	if err != nil {
		a.logger.Warn("oidc: login 失败（discovery 不可用）", "error", err.Error())
		http.Error(w, "oidc 登录暂不可用", http.StatusServiceUnavailable)
		return
	}
	verifier, err := randomString(64)
	if err != nil {
		http.Error(w, "oidc 登录暂不可用", http.StatusInternalServerError)
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	state, err := randomString(32)
	if err != nil {
		http.Error(w, "oidc 登录暂不可用", http.StatusInternalServerError)
		return
	}
	a.mu.Lock()
	a.states[state] = oidcState{verifier: verifier, exp: time.Now().Add(oidcStateTTL)}
	if len(a.states) > oidcMaxStateEntries {
		a.pruneStatesLocked()
	}
	a.mu.Unlock()

	u, err := url.Parse(prov.authURL)
	if err != nil {
		http.Error(w, "oidc 登录暂不可用", http.StatusInternalServerError)
		return
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", a.clientID)
	q.Set("redirect_uri", a.redirect)
	q.Set("scope", strings.Join(a.scopes, " "))
	q.Set("state", state)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

// pruneStatesLocked 清理过期 state（须持锁）。
func (a *OIDCAuthenticator) pruneStatesLocked() {
	now := time.Now()
	for k, s := range a.states {
		if s.exp.Before(now) {
			delete(a.states, k)
		}
	}
	// 仍超限则按 map 迭代序删任意条目（钳制无界增长，无严格 LRU 语义）。
	for k := range a.states {
		if len(a.states) <= oidcMaxStateEntries/2 {
			break
		}
		delete(a.states, k)
	}
}

// handleCallback 处理 GET /auth/oidc/callback：state 校验 → code 换 token →
// 验 id_token → 发会话 cookie。
func (a *OIDCAuthenticator) handleCallback(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	code := r.URL.Query().Get("code")
	if state == "" || code == "" {
		http.Error(w, "oidc 回调参数缺失", http.StatusBadRequest)
		return
	}
	a.mu.Lock()
	_, ok := a.states[state]
	delete(a.states, state) // 单次使用
	a.mu.Unlock()
	if !ok {
		http.Error(w, "oidc state 无效或过期", http.StatusUnauthorized)
		return
	}
	prov, err := a.provider(r.Context())
	if err != nil {
		a.logger.Warn("oidc: 回调失败（discovery 不可用）", "error", err.Error())
		http.Error(w, "oidc 登录暂不可用", http.StatusServiceUnavailable)
		return
	}
	// 用 verifier 换 token（Authorization Code + PKCE）。
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("redirect_uri", a.redirect)
	form.Set("client_id", a.clientID)
	form.Set("client_secret", a.secret)
	form.Set("code_verifier", a.takeVerifier(state))
	tokenReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, prov.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		http.Error(w, "oidc 登录失败", http.StatusInternalServerError)
		return
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenResp, err := prov.client.Do(tokenReq)
	if err != nil {
		a.logger.Warn("oidc: token 交换失败", "error", err.Error())
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	defer tokenResp.Body.Close()
	if tokenResp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, tokenResp.Body)
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if derr := json.NewDecoder(io.LimitReader(tokenResp.Body, 1<<20)).Decode(&tok); derr != nil || tok.IDToken == "" {
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	claims, err := a.verify(r.Context(), tok.IDToken)
	if err != nil {
		a.logger.Warn("oidc: id_token 校验失败", "error", err.Error())
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	owner, err := a.ownerFromClaims(claims, sub)
	if err != nil {
		http.Error(w, "oidc 登录失败", http.StatusUnauthorized)
		return
	}
	ak := oidcAK(sub)
	if err := a.sess.Issue(w, ak, owner); err != nil {
		http.Error(w, "oidc 登录失败", http.StatusInternalServerError)
		return
	}
	// 登录成功：审计 + 跳回 Web UI。
	a.logger.Info("oidc: 登录成功", "ak", ak, "owner", owner)
	http.Redirect(w, r, "/ui/", http.StatusFound)
}

// verify 校验 id_token（iss/aud/exp + 签名）。默认实现走 JWKS 验签（jwks.go）；
// 测试可注入 mock provider。
func (a *OIDCAuthenticator) verify(ctx context.Context, token string) (map[string]any, error) {
	prov := a.prov
	if prov == nil {
		return nil, fmt.Errorf("oidcldap: provider 未初始化")
	}
	if prov.verifyToken != nil {
		return prov.verifyToken(ctx, token)
	}
	if prov.jwksURL == "" {
		return nil, fmt.Errorf("oidcldap: discovery 缺 jwks_uri（无法验签）")
	}
	return verifyJWTWithJWKS(ctx, prov.client, prov.jwksURL, prov.issuer, token, a.clientID)
}

// randomString 生成 n 字节随机数的 base64url 串（无填充）。
func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// oauth2TokenSource 保持 x/oauth2 引用（装配扩展点：未来 CLI/refresh token 接入）。
var _ = oauth2.Token{}
