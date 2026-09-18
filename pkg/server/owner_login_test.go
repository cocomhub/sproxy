// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// ---- owner 用户名登录（服务端反查 AK）----

// serveLoginOwner 对 handler 发 owner 登录请求（POST /api/credentials/login 带 owner
// 字段；RemoteAddr 由调用方指定）。
func serveLoginOwner(t *testing.T, h http.Handler, remoteAddr string, body []byte) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/credentials/login", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	if remoteAddr != "" {
		req.RemoteAddr = remoteAddr
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code, w.Body.Bytes()
}

// TestOwnerLogin_ResolvesAK 验证 owner 用户名登录：已注册 owner + 正确 TOTP 动态码 →
// 服务端反查 AK 并签 session（红灯：login 请求体无 owner 字段，owner 登录 400/404）。
func TestOwnerLogin_ResolvesAK(t *testing.T) {
	t.Parallel()
	_, h, _, ring := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	// 注册 + 提交（owner "bob" 成为活跃账号）。
	ak, base32 := registerTOTPFor(t, h, "bob")
	now := ring.Now()
	code, _ := totpCodeFor(t, base32, now)
	nonceObj := requestNonce(t, h, loopRemoteV4)
	if lr := loginTOTP(t, h, loopRemoteV4, ak, nonceObj, code, "cli"); lr == nil {
		t.Fatalf("注册提交登录应成功")
	}

	// owner 登录：POST /login {owner:"bob", nonce, code} → 200（服务端反查 AK）。
	code2, _ := totpCodeFor(t, base32, timeNow())
	nonce2 := requestNonce(t, h, loopRemoteV4)
	body, _ := json.Marshal(map[string]any{
		"owner": "bob", "nonce": nonce2.Nonce, "code": code2, "login_type": "cli",
	})
	st, respBody := serveLoginOwner(t, h, loopRemoteV4, body)
	if st != http.StatusOK {
		t.Fatalf("owner 登录 status = %d, want 200（红灯: %s）", st, respBody)
	}
	var lr loginResp
	if err := json.Unmarshal(respBody, &lr); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if lr.AK != ak {
		t.Errorf("owner 登录响应 AK = %q, want %q（反查命中）", lr.AK, ak)
	}
	if lr.SessionSkeyID == "" {
		t.Error("owner 登录应返回 session_skey_id")
	}
}

// TestOwnerLogin_UnknownOwner_404 验证未知 owner → 404（不泄露账号存在性）。
func TestOwnerLogin_UnknownOwner_404(t *testing.T) {
	t.Parallel()
	_, h, _, _ := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	nonceObj := requestNonce(t, h, loopRemoteV4)
	body, _ := json.Marshal(map[string]any{
		"owner": "no-such-user", "nonce": nonceObj.Nonce, "code": "000000", "login_type": "cli",
	})
	st, _ := serveLoginOwner(t, h, loopRemoteV4, body)
	if st != http.StatusNotFound {
		t.Fatalf("未知 owner 登录 status = %d, want 404（红灯）", st)
	}
}

// TestOwnerLogin_WrongCode_401 验证 owner 登录错误动态码 → 401（与 AK 登录一致）。
func TestOwnerLogin_WrongCode_401(t *testing.T) {
	t.Parallel()
	_, h, _, ring := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	ak, base32 := registerTOTPFor(t, h, "carol")
	now := ring.Now()
	code, _ := totpCodeFor(t, base32, now)
	nonceObj := requestNonce(t, h, loopRemoteV4)
	if lr := loginTOTP(t, h, loopRemoteV4, ak, nonceObj, code, "cli"); lr == nil {
		t.Fatalf("注册提交登录应成功")
	}

	nonce2 := requestNonce(t, h, loopRemoteV4)
	body, _ := json.Marshal(map[string]any{
		"owner": "carol", "nonce": nonce2.Nonce, "code": "000000", "login_type": "cli",
	})
	st, _ := serveLoginOwner(t, h, loopRemoteV4, body)
	if st != http.StatusUnauthorized {
		t.Fatalf("owner 登录错误码 status = %d, want 401", st)
	}
}

// TestOwnerLogin_OwnerIndexRing 验证 Ring.OwnerAK 反查（owner→AK 索引）：注册提交后
// 用 owner 查回 AK（红灯：Ring 无 OwnerAK 方法 → build fail）。
func TestOwnerLogin_OwnerIndexRing(t *testing.T) {
	t.Parallel()
	ring := accesskey.NewRing()
	skB := make([]byte, 32)
	if _, _, err := ring.AddRegistration("ak-index-test-00000000000000000000000000000001", "dave", skB, nil, accesskey.RoleUser, 0); err != nil {
		t.Fatalf("AddRegistration: %v", err)
	}
	ak, ok := ring.OwnerAK("dave")
	if !ok || ak != "ak-index-test-00000000000000000000000000000001" {
		t.Fatalf("OwnerAK(dave) = %q, %v（红灯: Ring 无 owner 索引）", ak, ok)
	}
}

// timeNow 是登录测试用的当前时刻（Ring 时钟一致）。
func timeNow() time.Time { return time.Now() }
