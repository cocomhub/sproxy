// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// ---- TOTP pending 确认制（两段式提交）----

// TestRegisterTOTP_Pending_NoImmediateCommit 验证核心不变量：TOTP 注册**不立即写
// ring / 不落盘**——注册只产生 pending（返回 ak+otpauth），ring 无任何凭据、无 admin
// （红灯：现 registerTotp 立即 AddRegistration + persist）。
func TestRegisterTOTP_Pending_NoImmediateCommit(t *testing.T) {
	t.Parallel()
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	st, body := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"pending1"}`))
	if st != http.StatusOK {
		t.Fatalf("TOTP 注册 status = %d, want 200 (body=%s)", st, body)
	}
	var p totpRegistered
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.AK == "" || p.Base32Secret == "" {
		t.Fatalf("注册应返回 ak+base32（pending 形态）, got %+v", p)
	}
	// 关键：ring 中应无该 AK（pending 未提交），且无任何 admin。
	if _, ok := ring.GetKey(p.AK); ok {
		t.Fatalf("pending 注册不应立即写 ring（红灯：当前 registerTotp 立即 AddRegistration）")
	}
	if countAdmins(ring) != 0 {
		t.Fatalf("pending 阶段不应有任何 admin（红灯）")
	}
	if ring.Len() != 0 {
		t.Fatalf("pending 阶段 ring 应为空, len=%d", ring.Len())
	}
}

// TestRegisterTOTP_Pending_OwnerConflict 验证 owner 幂等：同 owner 已有活跃 pending →
// 409（不新建第二个 pending；红灯：现实现无 pending 概念，重复注册静默成功）。
func TestRegisterTOTP_Pending_OwnerConflict(t *testing.T) {
	t.Parallel()
	// 首 admin 阶段单槽优先于 owner 幂等——先提交一个 pending 成为 admin，
	// 再测「同 owner 已有活跃 pending → 409」。
	_, h, _, ring := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	// 第一个 pending（提交成为 admin，释放单槽）。
	ak1, base32 := registerTOTPFor(t, h, "dup-owner")
	now := ring.Now()
	code, _ := totpCodeFor(t, base32, now)
	nonceObj := requestNonce(t, h, loopRemoteV4)
	lr := loginTOTP(t, h, loopRemoteV4, ak1, nonceObj, code, "cli")
	if lr == nil {
		t.Fatalf("首次 pending 提交应成功（后续 owner 幂等才有意义）")
	}
	if countAdmins(ring) != 1 {
		t.Fatalf("提交后 admin 数 = %d, want 1", countAdmins(ring))
	}

	// 同 owner 重复注册 → 409（已有活跃 pending）。
	st2, body2 := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"dup-owner"}`))
	if st2 != http.StatusConflict {
		t.Fatalf("同 owner 重复注册 status = %d, want 409（红灯: %s）", st2, body2)
	}
	if !strings.Contains(string(body2), "已有") && !strings.Contains(string(body2), "already") {
		t.Errorf("409 响应应含幂等提示, got %s", body2)
	}
}

// TestRegisterTOTP_Pending_AdminSingleSlot 验证首 admin 单槽：首个 pending 存在时
// 第二个 pending（不同 owner）也 409——首 admin 阶段只允许一个候选，绝对避免
// 「并发 pending 抢 admin」（红灯：现实现每次注册都立即授 admin，无单槽）。
func TestRegisterTOTP_Pending_AdminSingleSlot(t *testing.T) {
	t.Parallel()
	h, _, ring := newRegisterTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true })

	// 第一个 pending（将成为 admin 的唯一候选）。
	st1, _ := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"admin-cand-1"}`))
	if st1 != http.StatusOK {
		t.Fatalf("首个 pending status = %d, want 200", st1)
	}
	// 第二个 pending（不同 owner）：首 admin 阶段单槽 → 409。
	st2, body2 := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"admin-cand-2"}`))
	if st2 != http.StatusConflict {
		t.Fatalf("首 admin 阶段第二 pending status = %d, want 409（红灯: %s）", st2, body2)
	}
	if countAdmins(ring) != 0 {
		t.Fatalf("pending 阶段不应有 admin")
	}
}

// TestRegisterTOTP_LoginCommitsPending 验证两段式提交：注册（pending）→ 输入正确
// TOTP 动态码登录 → ring 出现该 AK（提交）+ 唯一 admin 授予（红灯：现实现登录端点
// 直接取 ring TOTPSecret，pending 态无 secret → 404）。
func TestRegisterTOTP_LoginCommitsPending(t *testing.T) {
	t.Parallel()
	_, h, _, ring := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	// 注册 → pending。
	ak, base32 := registerTOTPFor(t, h, "commit-me")
	// ring 无该 AK（pending 未提交）。
	if _, ok := ring.GetKey(ak); ok {
		t.Fatalf("注册后 ring 不应有 AK（pending 态）")
	}

	// 登录：pending 提交（红灯：现实现 GetKey 404 → 无法登录）。
	now := ring.Now()
	code, secret := totpCodeFor(t, base32, now)
	_ = secret
	nonceObj := requestNonce(t, h, loopRemoteV4)
	lr := loginTOTP(t, h, loopRemoteV4, ak, nonceObj, code, "cli")
	if lr == nil {
		t.Fatalf("pending 提交登录应成功（红灯: 现实现 404）")
	}

	// 提交后 ring 出现该 AK + admin。
	k, ok := ring.GetKey(ak)
	if !ok {
		t.Fatalf("登录提交后 ring 应出现 AK %q", ak)
	}
	if len(k.TOTPSecret) != 20 {
		t.Errorf("提交后 TOTPSecret 长度 = %d, want 20", len(k.TOTPSecret))
	}
	if k.Role != accesskey.RoleAdmin {
		t.Errorf("首个提交登录者角色 = %q, want admin（首 admin 唯一）", k.Role)
	}
	if countAdmins(ring) != 1 {
		t.Fatalf("提交后 admin 数 = %d, want 唯一 1", countAdmins(ring))
	}
}

// TestRegisterTOTP_Pending_BindingFail_AdminNotConsumed 验证绑定失败回收：注册
// pending 后**从未登录**（模拟扫码失败/放弃）→ admin 未被占用、AK 可被新注册复用
// （红灯：现实现注册即写 ring + 授 admin，永不回收）。
func TestRegisterTOTP_Pending_BindingFail_AdminNotConsumed(t *testing.T) {
	t.Parallel()
	_, h, _, ring := newTOTPTestServer(t, func(c *Config) { c.Registration.ForceTOTP = true }, nil)

	// 注册 pending（首个候选）。
	ak1, _ := registerTOTPFor(t, h, "give-up")
	// ring 无 admin（pending 未提交）。
	if countAdmins(ring) != 0 {
		t.Fatalf("pending 阶段不应有 admin")
	}
	// 同 owner 再注册：既有 pending（未提交）→ 409（幂等，不产生第二 pending）。
	st, _ := serveRegister(t, h, loopRemoteV4, []byte(`{"owner":"give-up"}`))
	if st != http.StatusConflict {
		t.Fatalf("同 owner pending 未提交时重复注册 status = %d, want 409", st)
	}
	_ = ak1
}

// requestNonce 发 nonce 请求返回 nonce 对象。
func requestNonce(t *testing.T, h http.Handler, remoteAddr string) nonceResp {
	t.Helper()
	st, body := serveNonce(t, h, remoteAddr, nil)
	if st != http.StatusOK {
		t.Fatalf("nonce status = %d, want 200 (body=%s)", st, body)
	}
	var nr nonceResp
	if err := json.Unmarshal(body, &nr); err != nil {
		t.Fatalf("unmarshal nonce: %v", err)
	}
	return nr
}

// loginTOTP 用 nonce+code 登录，返回 loginResult（失败返回 nil 并打印原因）。
func loginTOTP(t *testing.T, h http.Handler, remoteAddr string, ak string, nonce nonceResp, code, loginType string) *loginResult {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"ak": ak, "nonce": nonce.Nonce, "code": code, "login_type": loginType,
	})
	st, respBody := serveLogin(t, h, remoteAddr, body)
	if st != http.StatusOK {
		t.Logf("login status = %d (body=%s)", st, respBody)
		return nil
	}
	var lr loginResult
	if err := json.Unmarshal(respBody, &lr.resp); err != nil {
		t.Fatalf("unmarshal login: %v", err)
	}
	return &lr
}
