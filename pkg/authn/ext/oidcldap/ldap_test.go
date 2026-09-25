// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/authn"
	"github.com/go-ldap/ldap/v3"
)

// mockLDAPConn 是 ldapConn 的测试实现：服务账号 bind + 用户搜索 + 用户 bind。
type mockLDAPConn struct {
	t          *testing.T
	users      map[string]string // username → password
	searchFail bool
	bindCalls  []string
}

func (m *mockLDAPConn) Bind(username, password string) error {
	m.bindCalls = append(m.bindCalls, username)
	if username == "" && password == "" {
		return nil // 匿名 bind 允许
	}
	// 服务账号 bind（非 DN 形态的 uid）。
	if !strings.HasPrefix(username, "uid=") {
		if username == "cn=svc,dc=example" && password == "svc-pass" {
			return nil
		}
		return fmt.Errorf("服务账号 bind 失败")
	}
	// 用户 DN 形态 uid=<name>,...
	parts := strings.SplitN(strings.TrimPrefix(username, "uid="), ",", 2)
	name := parts[0]
	want, ok := m.users[name]
	if !ok {
		return fmt.Errorf("用户不存在")
	}
	if password != want {
		return fmt.Errorf("口令错误")
	}
	return nil
}

func (m *mockLDAPConn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	if m.searchFail {
		return nil, fmt.Errorf("搜索失败")
	}
	filter := req.Filter
	// 提取 uid=<name> 过滤器中的用户名。
	name := ""
	for part := range strings.SplitSeq(filter, ")") {
		if strings.Contains(part, "uid=") {
			if _, after, ok := strings.Cut(part, "uid="); ok {
				name = after
			}
		}
	}
	if name == "" {
		return nil, fmt.Errorf("过滤器无 uid")
	}
	if _, ok := m.users[name]; !ok {
		return &ldap.SearchResult{Entries: nil}, nil
	}
	return &ldap.SearchResult{Entries: []*ldap.Entry{{
		DN:         "uid=" + name + ",dc=example",
		Attributes: []*ldap.EntryAttribute{{Name: "uid", Values: []string{name}}},
	}}}, nil
}

func (m *mockLDAPConn) Close() error { return nil }

// ldapTestAuthenticator 构造注入 mock 连接的 LDAP 认证器。
func ldapTestAuthenticator(t *testing.T, users map[string]string) (*LDAPAuthenticator, *mockLDAPConn) {
	t.Helper()
	cfg := LDAPConfig{
		Enabled:      true,
		URL:          "ldap://127.0.0.1:0",
		BindDN:       "cn=svc,dc=example",
		BindPassword: "svc-pass",
		BaseDN:       "dc=example",
	}
	sess := authn.NewSessionManager(make([]byte, 32), time.Hour, false)
	a, err := NewLDAPAuthenticator(cfg, sess, testLogger())
	if err != nil {
		t.Fatalf("NewLDAPAuthenticator: %v", err)
	}
	mc := &mockLDAPConn{t: t, users: users}
	a.dial = func(string) (ldapConn, error) { return mc, nil }
	return a, mc
}

// TestLDAP_Login_Success 验证 LDAP 绑定成功 → 会话 cookie 签发 + JSON ok。
// 变异点③：bindUser 成功路径若不建会话（不调 sess.Issue），本用例红。
func TestLDAP_Login_Success(t *testing.T) {
	t.Parallel()
	a, _ := ldapTestAuthenticator(t, map[string]string{"alice": "s3cret"})
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": "s3cret"})
	req := httptest.NewRequest("POST", "/auth/ldap/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("login status = %d (body=%s), want 200", w.Code, w.Body.String())
	}
	// 会话 cookie 已签发。
	cookies := w.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != authn.SessionCookieName {
		t.Fatal("LDAP 登录成功未签发会话 cookie")
	}
	// 认证器可用该会话认证。
	checkReq := httptest.NewRequest("GET", "/api/files", nil)
	checkReq.AddCookie(cookies[0])
	p, err := a.Authenticate(context.Background(), checkReq)
	if err != nil {
		t.Fatalf("Authenticate with session: %v", err)
	}
	if p.Owner != "alice" || !strings.HasPrefix(p.AK, "ldap-") {
		t.Fatalf("Principal = (ak=%q owner=%q), want ldap- 前缀 + alice", p.AK, p.Owner)
	}
}

// TestLDAP_Login_WrongPassword_401 验证口令错误 → 401（统一文案，不泄露账号存在性）。
func TestLDAP_Login_WrongPassword_401(t *testing.T) {
	t.Parallel()
	a, _ := ldapTestAuthenticator(t, map[string]string{"alice": "s3cret"})
	body, _ := json.Marshal(map[string]string{"username": "alice", "password": "wrong"})
	req := httptest.NewRequest("POST", "/auth/ldap/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("错误口令 status = %d, want 401", w.Code)
	}
	if got := w.Body.String(); strings.Contains(got, "alice") || strings.Contains(got, "存在") {
		t.Fatalf("401 响应泄露账号存在性：%q", got)
	}
}

// TestLDAP_Login_UserNotFound_401 验证用户不存在 → 401（统一文案）。
func TestLDAP_Login_UserNotFound_401(t *testing.T) {
	t.Parallel()
	a, _ := ldapTestAuthenticator(t, map[string]string{"alice": "s3cret"})
	body, _ := json.Marshal(map[string]string{"username": "bob", "password": "x"})
	req := httptest.NewRequest("POST", "/auth/ldap/login", bytes.NewReader(body))
	w := httptest.NewRecorder()
	a.handleLogin(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("未知用户 status = %d, want 401", w.Code)
	}
}

// TestLDAP_Authenticate_NoSession_401 验证无会话 cookie 的请求 → 认证失败。
func TestLDAP_Authenticate_NoSession_401(t *testing.T) {
	t.Parallel()
	a, _ := ldapTestAuthenticator(t, map[string]string{"alice": "s3cret"})
	req := httptest.NewRequest("GET", "/api/files", nil)
	if _, err := a.Authenticate(context.Background(), req); err == nil {
		t.Fatal("无会话 cookie 竟认证成功")
	}
}

// TestLDAP_Routes_Registered 验证 LDAP 登录路由声明。
func TestLDAP_Routes_Registered(t *testing.T) {
	t.Parallel()
	a, _ := ldapTestAuthenticator(t, nil)
	routes := a.Routes()
	if len(routes) != 1 || routes[0].Pattern != ldapLoginPath || routes[0].Method != http.MethodPost {
		t.Fatalf("Routes = %+v, want 1 条 POST %s", routes, ldapLoginPath)
	}
}

// _ 保持 time 引用（会话 TTL 相关未来扩展）。
var _ = time.Second
