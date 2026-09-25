// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package authn

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// SessionCookieName 是外部认证（OIDC/LDAP）会话 cookie 名。
// 与服务端既有凭据体系（SproxySig / TOTP session SK）**完全隔离**——外部会话只
// 走本 cookie + 服务端签名，不污染 SK Ring（设计文档 oidc-ldap 风险 2）。
const SessionCookieName = "sproxy_session"

// SessionManager 签发/校验外部认证会话 cookie（HMAC-SHA256 签名，无状态）。
//
// 语义（设计文档 oidc-ldap 会话段）：
//   - cookie 载荷 = base64url(JSON{ak, owner, exp}) + "." + hex(HMAC-SHA256(key, payload))；
//   - 密钥 = external_auth.session_secret（32B）；为空时回落**进程内随机密钥**
//     （重启即全员下线，文档明示需持久化 session_secret 才能跨重启保持会话）；
//   - 校验失败/过期 → 401（Authenticate 失败），前端重新走 /auth/*/login；
//   - 无内存表（无状态）：撤销 = 服务端 DeleteCookie；防篡改由 HMAC 保证。
//   - 有界防滥用：cookie 值长度上限 + 单请求只读一次。
type SessionManager struct {
	key        []byte
	ttl        time.Duration
	cookieName string
	secure     bool
	now        func() time.Time
}

// NewSessionManager 构造会话管理器。key 为 32B HMAC 密钥（nil/长度 != 32 时
// 回落进程内随机密钥）；ttl <= 0 时默认 24h；secure=true 时 cookie 仅 HTTPS
// 传输（TLS 启用时装配层传 true）。
func NewSessionManager(key []byte, ttl time.Duration, secure bool) *SessionManager {
	if len(key) != 32 {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			// crypto/rand 失败近乎不可达（熵源故障）；仍给出确定回退防 nil key panic。
			copy(k, []byte("sproxy-session-fallback-key-00000000"))
		}
		key = k
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &SessionManager{
		key:        key,
		ttl:        ttl,
		cookieName: SessionCookieName,
		secure:     secure,
		now:        time.Now,
	}
}

// sessionPayload 是签名 cookie 的载荷（AK 即外部身份锚/桶 ID，Owner 为映射用户名）。
type sessionPayload struct {
	AK    string `json:"ak"`
	Owner string `json:"owner"`
	Exp   int64  `json:"exp"` // unix 秒
}

// maxSessionCookieLen 是 cookie 值长度上限（防畸形超长 cookie 占用解析时间）。
const maxSessionCookieLen = 4096

// Issue 签发会话 cookie 并写入响应（ak/owner 即登录成功映射的身份）。
// 返回错误仅在随机/序列化失败时发生（近乎不可达）。
//
// #nosec G124 -- Secure 属性按装配层 TLS 开关传入（secure=false 仅用于无 TLS 的
// 本地/内网部署，此时 SameSite=Lax + HttpOnly 已提供防护；生产启用 TLS 即 secure=true）。
func (m *SessionManager) Issue(w http.ResponseWriter, ak, owner string) error {
	if ak == "" {
		return fmt.Errorf("authn: session ak 不能为空")
	}
	now := m.now()
	pl := sessionPayload{AK: ak, Owner: owner, Exp: now.Add(m.ttl).Unix()}
	raw, err := json.Marshal(pl)
	if err != nil {
		return fmt.Errorf("authn: 序列化会话载荷失败: %w", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte(payload))
	token := payload + "." + hex.EncodeToString(mac.Sum(nil))

	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(m.ttl.Seconds()),
	})
	return nil
}

// Lookup 从请求读取并校验会话 cookie，成功返回 (ak, owner, true)。
// 未携带 / 格式非法 / HMAC 不匹配 / 已过期 → ("", "", false)。
func (m *SessionManager) Lookup(r *http.Request) (string, string, bool) {
	c, err := r.Cookie(m.cookieName)
	if err != nil || c == nil || c.Value == "" || len(c.Value) > maxSessionCookieLen {
		return "", "", false
	}
	payload, sig, ok := strings.Cut(c.Value, ".")
	if !ok || payload == "" || sig == "" {
		return "", "", false
	}
	mac := hmac.New(sha256.New, m.key)
	_, _ = mac.Write([]byte(payload))
	got := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(got), []byte(sig)) {
		return "", "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return "", "", false
	}
	var pl sessionPayload
	if err := json.Unmarshal(raw, &pl); err != nil || pl.AK == "" {
		return "", "", false
	}
	if pl.Exp <= m.now().Unix() {
		return "", "", false
	}
	return pl.AK, pl.Owner, true
}

// Delete 使会话 cookie 失效（清除浏览器 cookie；无状态签名无服务端表可清）。
//
// #nosec G124 -- 同 Issue（Secure 按装配层 TLS 开关）。
func (m *SessionManager) Delete(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     m.cookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   m.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   -1,
	})
}

// TTL 返回会话有效期（供日志/诊断）。
func (m *SessionManager) TTL() time.Duration { return m.ttl }

// SetClock 注入测试时钟（nil 回落 time.Now）。
func (m *SessionManager) SetClock(now func() time.Time) {
	if now != nil {
		m.now = now
	}
}

// keyLen 返回当前 HMAC 密钥字节数（供装配层日志提示是否回落随机密钥）。
func (m *SessionManager) KeyLen() int { return len(m.key) }
