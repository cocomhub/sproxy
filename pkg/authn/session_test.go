// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSessionManager_IssueLookup_Roundtrip 验证签发→回读完整往返：
// cookie 正确携带 (ak, owner)，校验通过返回同一对身份。
func TestSessionManager_IssueLookup_Roundtrip(t *testing.T) {
	t.Parallel()
	m := NewSessionManager(make([]byte, 32), time.Hour, false)
	w := httptest.NewRecorder()
	if err := m.Issue(w, "oidc:sub-123", "alice@example.com"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("应恰好签发 1 个 cookie，实际 %d", len(cookies))
	}
	c := cookies[0]
	if c.Name != SessionCookieName || c.HttpOnly != true || c.Path != "/" {
		t.Fatalf("cookie 属性不符：name=%q httpOnly=%v path=%q", c.Name, c.HttpOnly, c.Path)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	ak, owner, ok := m.Lookup(r)
	if !ok || ak != "oidc:sub-123" || owner != "alice@example.com" {
		t.Fatalf("Lookup = (%q, %q, %v)，want (oidc:sub-123, alice@example.com, true)", ak, owner, ok)
	}
}

// TestSessionManager_Lookup_RejectsTampered 验证篡改载荷（改 ak）→ HMAC 失配 → 拒绝。
// 变异点①：Lookup 若省略 HMAC 校验，本用例红。
func TestSessionManager_Lookup_RejectsTampered(t *testing.T) {
	t.Parallel()
	m := NewSessionManager(make([]byte, 32), time.Hour, false)
	w := httptest.NewRecorder()
	if err := m.Issue(w, "oidc:sub-123", "alice@example.com"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c := w.Result().Cookies()[0]
	// 篡改载荷：把 payload 的 base64url 内容改掉（把 sub-123 的 base64 子串替换——
	// 但同一 base64 串 "sub-123" 不直接出现，故改为**改写 payload 的最后一个字符**
	// 不现实；直接构造一个不同载荷：换 ak 用完整重签不行——我们用「替换 base64
	// 串中代表 alice 的子串」，因为 JSON 中 email 的 base64 是 "YWxpY2VAZXhhbXBsZS5jb20i"，
	// 把其中的 alice 段替换为 EVIL 的 base64 前段。更稳妥：直接对 payload 重新编码——
	// 这里简单起见：把 payload 里的子串 "YWxpY2VAZXhhbXBsZS5jb20i"（base64(alice@example.com")）
	// 换成 "RVZJTA"（base64(EVIL)）——解码后 ak 不变、owner 变 → 但本用例要验 HMAC
	// 对**载荷任何位**敏感：改 owner 同样必须失配。
	tampered := strings.Replace(c.Value, "YWxpY2VAZXhhbXBsZS5jb20i", "RVZJTA", 1)
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(&http.Cookie{Name: SessionCookieName, Value: tampered})
	if ak, _, ok := m.Lookup(r); ok {
		t.Fatalf("篡改 cookie 竟通过校验（ak=%q）——HMAC 未生效", ak)
	}
}

// TestSessionManager_Lookup_RejectsExpired 验证过期 cookie → 拒绝。
// 变异点②：Lookup 若省略 exp 校验，本用例红。
func TestSessionManager_Lookup_RejectsExpired(t *testing.T) {
	t.Parallel()
	m := NewSessionManager(make([]byte, 32), time.Hour, false)
	fake := time.Unix(1_700_000_000, 0)
	m.SetClock(func() time.Time { return fake })
	w := httptest.NewRecorder()
	if err := m.Issue(w, "oidc:sub-123", "alice@example.com"); err != nil {
		t.Fatalf("Issue: %v", err)
	}
	c := w.Result().Cookies()[0]
	// 时钟前进超过 TTL → 过期。
	m.SetClock(func() time.Time { return fake.Add(2 * time.Hour) })
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(c)
	if _, _, ok := m.Lookup(r); ok {
		t.Fatal("过期 cookie 竟通过校验——exp 未生效")
	}
}

// TestSessionManager_Delete_ClearsCookie 验证 Delete 下发 MaxAge=-1 清除 cookie。
func TestSessionManager_Delete_ClearsCookie(t *testing.T) {
	t.Parallel()
	m := NewSessionManager(make([]byte, 32), time.Hour, false)
	w := httptest.NewRecorder()
	m.Delete(w)
	c := w.Result().Cookies()[0]
	if c.Name != SessionCookieName || c.MaxAge != -1 || c.Value != "" {
		t.Fatalf("Delete cookie 不符：name=%q maxAge=%d value=%q", c.Name, c.MaxAge, c.Value)
	}
}

// TestSessionManager_RandomFallbackKey 验证未配置密钥时回落随机密钥（每次实例不同）。
func TestSessionManager_RandomFallbackKey(t *testing.T) {
	t.Parallel()
	a := NewSessionManager(nil, time.Hour, false)
	b := NewSessionManager(nil, time.Hour, false)
	w := httptest.NewRecorder()
	if err := a.Issue(w, "ak-a", "owner-a"); err != nil {
		t.Fatalf("Issue(a): %v", err)
	}
	ca := w.Result().Cookies()[0]
	// b 实例（不同随机密钥）校验 a 签发的 cookie 必须失败。
	r := httptest.NewRequest("GET", "/", nil)
	r.AddCookie(ca)
	if _, _, ok := b.Lookup(r); ok {
		t.Fatal("不同密钥实例竟能校验通过——随机回落密钥未生效")
	}
}

// TestSessionManager_SecureFlag 验证 secure=true 时 cookie 带 Secure 属性。
func TestSessionManager_SecureFlag(t *testing.T) {
	t.Parallel()
	m := NewSessionManager(make([]byte, 32), time.Hour, true)
	w := httptest.NewRecorder()
	_ = m.Issue(w, "ak", "owner")
	c := w.Result().Cookies()[0]
	if !c.Secure {
		t.Fatal("secure=true 时 cookie 必须带 Secure 属性（仅 HTTPS 传输）")
	}
}
