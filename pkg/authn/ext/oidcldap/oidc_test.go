// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/authn"
)

// mockOIDCServer 是 OIDC mock issuer：/.well-known + token 端点 + JWKS。
// 签发 RS256 id_token（可注入 claims 覆写）。
type mockOIDCServer struct {
	t        *testing.T
	issuer   string
	clientID string
	priv     *rsa.PrivateKey
	claims   map[string]any // 覆写默认 claims
}

// newMockOIDC 启动 mock OIDC issuer（httptest，127.0.0.1）。
func newMockOIDC(t *testing.T, claims map[string]any) *mockOIDCServer {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	m := &mockOIDCServer{t: t, priv: priv, claims: claims}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", m.handleDiscovery)
	mux.HandleFunc("/oauth/token", m.handleToken)
	mux.HandleFunc("/jwks", m.handleJWKS)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	m.issuer = srv.URL
	m.clientID = "test-client"
	return m
}

func (m *mockOIDCServer) handleDiscovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                 m.issuer,
		"authorization_endpoint": m.issuer + "/auth",
		"token_endpoint":         m.issuer + "/oauth/token",
		"jwks_uri":               m.issuer + "/jwks",
	})
}

func (m *mockOIDCServer) handleJWKS(w http.ResponseWriter, r *http.Request) {
	pub := &m.priv.PublicKey
	n := base64.RawURLEncoding.EncodeToString(pub.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"keys": []any{map[string]any{"kty": "RSA", "kid": "test-kid", "n": n, "e": e}},
	})
}

func (m *mockOIDCServer) handleToken(w http.ResponseWriter, r *http.Request) {
	claims := map[string]any{
		"iss":   m.issuer,
		"aud":   m.clientID,
		"sub":   "sub-user-1",
		"email": "alice@example.com",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	maps.Copy(claims, m.claims)
	token := signTestJWT(m.t, m.priv, claims)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"id_token": token})
}

// signTestJWT 用测试 RSA 私钥签发 RS256 JWT。
func signTestJWT(t *testing.T, priv *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	header := map[string]any{"alg": "RS256", "kid": "test-kid", "typ": "JWT"}
	hRaw, _ := json.Marshal(header)
	pRaw, _ := json.Marshal(claims)
	h := base64.RawURLEncoding.EncodeToString(hRaw)
	p := base64.RawURLEncoding.EncodeToString(pRaw)
	signed := h + "." + p
	digest := sha256Sum([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// oidcTestAuthenticator 构造已注入 mock provider 的 OIDC 认证器（跳过 discovery）。
func oidcTestAuthenticator(t *testing.T, m *mockOIDCServer, cfgMod func(*OIDCConfig)) *OIDCAuthenticator {
	t.Helper()
	cfg := OIDCConfig{
		Enabled:       true,
		Issuer:        m.issuer,
		ClientID:      m.clientID,
		RedirectURL:   m.issuer + "/callback",
		ClaimOwner:    "email",
		AutoProvision: true,
	}
	if cfgMod != nil {
		cfgMod(&cfg)
	}
	sess := authn.NewSessionManager(make([]byte, 32), time.Hour, false)
	a, err := NewOIDCAuthenticator(cfg, sess, testLogger())
	if err != nil {
		t.Fatalf("NewOIDCAuthenticator: %v", err)
	}
	// 注入 provider（跳过 discovery 网络）。
	a.prov = &oidcProvider{
		authURL:  m.issuer + "/auth",
		tokenURL: m.issuer + "/oauth/token",
		jwksURL:  m.issuer + "/jwks",
		issuer:   m.issuer,
		client:   &http.Client{Transport: newIsolatedTransport()},
		now:      time.Now,
		// 默认 verify 走 JWKS 验签；这里不注入 verifyToken 以覆盖真实路径。
	}
	return a
}

// TestOIDC_BearerIDToken_PrincipalOwner 验证 Bearer id_token 认证：Principal 的
// AK 由 sub 派生、Owner 用 email claim（claim_owner）。
func TestOIDC_BearerIDToken_PrincipalOwner(t *testing.T) {
	t.Parallel()
	m := newMockOIDC(t, nil)
	a := oidcTestAuthenticator(t, m, nil)

	token := signTestJWT(t, m.priv, map[string]any{
		"iss": m.issuer, "aud": m.clientID, "sub": "sub-user-1",
		"email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix(),
	})
	r := httptest.NewRequest("GET", "/api/files", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	p, err := a.Authenticate(context.Background(), r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if p.Owner != "alice@example.com" {
		t.Fatalf("Owner = %q, want alice@example.com（claim_owner=email）", p.Owner)
	}
	if p.Role != "user" {
		t.Fatalf("Role = %q, want user", p.Role)
	}
	if p.Secret != nil {
		t.Fatal("外部身份 Secret 必须为空（无 SproxySig SK，不建隧道）")
	}
	if !strings.HasPrefix(p.AK, "oidc-") {
		t.Fatalf("AK = %q, want oidc- 前缀（sub 派生稳定 AK）", p.AK)
	}
}

// TestOIDC_BearerIDToken_AudMismatchRejected 验证 aud 错 → 401。
// 变异点①：verify 若不校验 aud，本用例红。
func TestOIDC_BearerIDToken_AudMismatchRejected(t *testing.T) {
	t.Parallel()
	m := newMockOIDC(t, nil)
	a := oidcTestAuthenticator(t, m, nil)
	token := signTestJWT(t, m.priv, map[string]any{
		"iss": m.issuer, "aud": "WRONG-CLIENT", "sub": "sub-user-1",
		"email": "alice@example.com", "exp": time.Now().Add(time.Hour).Unix(),
	})
	r := httptest.NewRequest("GET", "/api/files", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if _, err := a.Authenticate(context.Background(), r); err == nil {
		t.Fatal("aud 错误的 id_token 竟通过校验——aud 校验未生效")
	}
}

// TestOIDC_BearerIDToken_ExpiredRejected 验证过期 id_token → 401。
// 变异点②：verify 若不校验 exp，本用例红。
func TestOIDC_BearerIDToken_ExpiredRejected(t *testing.T) {
	t.Parallel()
	m := newMockOIDC(t, nil)
	a := oidcTestAuthenticator(t, m, nil)
	token := signTestJWT(t, m.priv, map[string]any{
		"iss": m.issuer, "aud": m.clientID, "sub": "sub-user-1",
		"email": "alice@example.com", "exp": time.Now().Add(-time.Hour).Unix(),
	})
	r := httptest.NewRequest("GET", "/api/files", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if _, err := a.Authenticate(context.Background(), r); err == nil {
		t.Fatal("过期 id_token 竟通过校验——exp 校验未生效")
	}
}

// TestOIDC_Callback_IssuesSession 验证完整回调：state + code → 会话 cookie 签发。
func TestOIDC_Callback_IssuesSession(t *testing.T) {
	t.Parallel()
	m := newMockOIDC(t, nil)
	a := oidcTestAuthenticator(t, m, nil)
	sess := authn.NewSessionManager(make([]byte, 32), time.Hour, false)
	a.sess = sess

	// 先经 login 拿 state（302 issuer）。
	loginReq := httptest.NewRequest("GET", "/auth/oidc/login", nil)
	loginW := httptest.NewRecorder()
	a.handleLogin(loginW, loginReq)
	if loginW.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", loginW.Code)
	}
	loc, err := url.Parse(loginW.Header().Get("Location"))
	if err != nil {
		t.Fatalf("login Location: %v", err)
	}
	state := loc.Query().Get("state")
	if state == "" {
		t.Fatal("login 302 未携带 state")
	}
	// 回调：state + code（mock token 端点返回 id_token）。
	cbReq := httptest.NewRequest("GET", "/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=test-code", nil)
	cbW := httptest.NewRecorder()
	a.handleCallback(cbW, cbReq)
	if cbW.Code != http.StatusFound {
		t.Fatalf("callback status = %d (body=%s), want 302", cbW.Code, cbW.Body.String())
	}
	// 会话 cookie 已签发且可 Lookup 回身份。
	cookies := cbW.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != authn.SessionCookieName {
		t.Fatalf("callback 未签发会话 cookie")
	}
	checkReq := httptest.NewRequest("GET", "/api/files", nil)
	checkReq.AddCookie(cookies[0])
	ak, owner, ok := sess.Lookup(checkReq)
	if !ok || ak == "" || owner != "alice@example.com" {
		t.Fatalf("会话 Lookup = (%q, %q, %v)，want alice@example.com", ak, owner, ok)
	}
}

// TestOIDC_Callback_StateMismatchRejected 验证 state 不匹配 → 401。
func TestOIDC_Callback_StateMismatchRejected(t *testing.T) {
	t.Parallel()
	m := newMockOIDC(t, nil)
	a := oidcTestAuthenticator(t, m, nil)
	cbReq := httptest.NewRequest("GET", "/auth/oidc/callback?state=EVIL&code=test-code", nil)
	cbW := httptest.NewRecorder()
	a.handleCallback(cbW, cbReq)
	if cbW.Code != http.StatusUnauthorized {
		t.Fatalf("state 不匹配 status = %d, want 401", cbW.Code)
	}
}
