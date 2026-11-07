// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_login_scenarios_test.go 补设计 §9.1「认证面」最后两个动作：
// `credential_login`（TOTP 登录成功签发 session）与 `credential_login_denied`（被拒）。
//
// 登录是**凭据面最敏感的入口**（会话=临时访问权）：成功与被拒都必须可追溯，
// 且 Detail 绝不能包含 TOTP code / nonce / session SK。

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_Login 登录成功 + 未知 nonce 被拒 ⇒ 两条审计（action 正确、不含 code/nonce）。
func TestAuditScenario_Login(t *testing.T) {
	t.Parallel()
	hh, handler, cfgPtr, _ := newTOTPTestServer(t, nil, nil)
	ak, b32 := registerTOTPFor(t, handler, "")

	// ① 成功登录（正确 nonce + 正确 code）。
	nonce := obtainNonce(t, handler, loopRemoteV4)
	lr := loginSuccessf(t, handler, cfgPtr, loginSpec{ak: ak, base32Secret: b32, nonce: nonce})

	// ② 被拒登录（未知 nonce ⇒ 401 且不计失败）。
	raw, _ := json.Marshal(map[string]any{"ak": ak, "nonce": "not-a-real-nonce", "code": "000000"})
	if st, body := serveLogin(t, handler, loopRemoteV4, raw); st != http.StatusUnauthorized {
		t.Fatalf("未知 nonce 登录 status = %d, want 401 (body=%s)", st, body)
	}

	entries := hh.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	var sawLogin, sawDenied bool
	for _, ev := range entries {
		switch ev.Action {
		case "credential_login":
			if ev.Object != ak {
				continue
			}
			sawLogin = true
			if ev.Result != "success" {
				t.Errorf("credential_login result = %q, want success", ev.Result)
			}
		case "credential_login_denied":
			if ev.Object != ak {
				continue
			}
			sawDenied = true
			if ev.Result == "success" {
				t.Error("credential_login_denied 不能记 success")
			}
		default:
			continue
		}
		// 红线：Detail 不得出现 nonce / TOTP code / session SK。
		if strings.Contains(ev.Detail, nonce) {
			t.Errorf("%s detail 泄露 nonce ✗: %q", ev.Action, ev.Detail)
		}
		// skey_id 是**标识**（可出现在审计里，便于取证），但 session **密钥本体**绝不能出现。
		if len(lr.sessionSK) > 0 && strings.Contains(ev.Detail, string(lr.sessionSK)) {
			t.Errorf("%s detail 泄露 session 密钥本体 ✗: %q", ev.Action, ev.Detail)
		}
		if len(lr.sessionSK) >= 8 && strings.Contains(ev.Detail, hex.EncodeToString(lr.sessionSK[:8])) {
			t.Errorf("%s detail 泄露 session 密钥(hex) ✗: %q", ev.Action, ev.Detail)
		}
	}
	if !sawLogin {
		t.Error("缺少 credential_login 审计行（会话签发必须可追溯 ✗）")
	}
	if !sawDenied {
		t.Error("缺少 credential_login_denied 审计行（登录被拒是攻击信号 ✗）")
	}
}
