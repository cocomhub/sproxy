// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oidcldap

import (
	"testing"
)

// TestProvider_New_AllDisabled_ReturnsNil 验证全 disabled → New 返回 (nil, nil)
// （未配置不启用，零回归）。
func TestProvider_New_AllDisabled_ReturnsNil(t *testing.T) {
	t.Parallel()
	p, err := New(Config{})
	if err != nil {
		t.Fatalf("New(全 disabled) 不应报错: %v", err)
	}
	if p != nil {
		t.Fatalf("全 disabled 应返回 nil（不装配），实际 %+v", p)
	}
}

// TestProvider_New_OIDC_MissingIssuer_Fails 验证 enabled 但缺字段 → fail-closed 错误。
func TestProvider_New_OIDC_MissingIssuer_Fails(t *testing.T) {
	t.Parallel()
	_, err := New(Config{OIDC: OIDCConfig{Enabled: true, ClientID: "c", RedirectURL: "https://x/cb"}})
	if err == nil {
		t.Fatal("oidc enabled 但 issuer 空应报错（fail-closed）")
	}
}

// TestProvider_New_LDAP_MissingURL_Fails 验证 enabled 但缺 URL → fail-closed 错误。
func TestProvider_New_LDAP_MissingURL_Fails(t *testing.T) {
	t.Parallel()
	_, err := New(Config{LDAP: LDAPConfig{Enabled: true, BaseDN: "dc=x"}})
	if err == nil {
		t.Fatal("ldap enabled 但 url 空应报错（fail-closed）")
	}
}

// TestProvider_SessionSecret_Raw32B 验证 raw 32B 会话密钥可解析。
func TestProvider_SessionSecret_Raw32B(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte('a' + i%26)
	}
	p, err := New(Config{LDAP: LDAPConfig{Enabled: true, URL: "ldap://127.0.0.1:0", BaseDN: "dc=x"}, SessionSecret: string(key)})
	if err != nil {
		t.Fatalf("New(raw 32B key): %v", err)
	}
	if p == nil || !p.IsLDAPEnabled() {
		t.Fatal("ldap enabled 应装配 provider")
	}
	_ = p.Close()
}

// TestProvider_SessionSecret_InvalidLen_Fails 验证非法长度会话密钥 → fail-closed。
func TestProvider_SessionSecret_InvalidLen_Fails(t *testing.T) {
	t.Parallel()
	_, err := New(Config{LDAP: LDAPConfig{Enabled: true, URL: "ldap://127.0.0.1:0", BaseDN: "dc=x"}, SessionSecret: "short"})
	if err == nil {
		t.Fatal("session_secret 长度非法应报错")
	}
}
