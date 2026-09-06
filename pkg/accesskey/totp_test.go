// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"crypto/sha256"
	"testing"
)

// TestWrapContextTOTP 常量契约：值固定 "sproxy-totp/v1"，且与 WrapContextCredentials 明确
// 区分（防跨 context 复用）。
func TestWrapContextTOTP(t *testing.T) {
	if WrapContextTOTP != "sproxy-totp/v1" {
		t.Fatalf("WrapContextTOTP 值不符: got %q, want %q", WrapContextTOTP, "sproxy-totp/v1")
	}
	if WrapContextTOTP == WrapContextCredentials {
		t.Fatalf("WrapContextTOTP 必须与 WrapContextCredentials 不同（防跨 context 复用）: %q", WrapContextTOTP)
	}
}

// TestDeriveTOTPWrapKey_Deterministic 同 (code, ak, nonce) 两次一致；nonce / code 不同 → key 不同。
func TestDeriveTOTPWrapKey_Deterministic(t *testing.T) {
	const code = "123456"
	const ak = "ak-totp-1234567890abcdef"
	const nonce = "aabbccdd"

	k1, err := DeriveTOTPWrapKey(code, ak, nonce)
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey: %v", err)
	}
	if len(k1) != 32 {
		t.Fatalf("信封密钥应为 32 字节, got %d", len(k1))
	}
	k2, err := DeriveTOTPWrapKey(code, ak, nonce)
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey 二次: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("同 (code, ak, nonce) 两次派生应一致")
	}

	// nonce 不同 → key 不同（nonce 唯一性补偿 6 位码低熵）
	kNonce, err := DeriveTOTPWrapKey(code, ak, "different-nonce")
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey(nonce): %v", err)
	}
	if bytes.Equal(k1, kNonce) {
		t.Fatalf("不同 nonce 应派生不同 key")
	}

	// code 不同 → key 不同
	kCode, err := DeriveTOTPWrapKey("654321", ak, nonce)
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey(code): %v", err)
	}
	if bytes.Equal(k1, kCode) {
		t.Fatalf("不同 code 应派生不同 key")
	}
}

// TestDeriveTOTPWrapKey_CrossContext TOTP wrap key 与 credentials wrap context 派生 key
// 不同（防跨 context 复用）；且实现严格等于 wrapKey(sha256(code), ak, WrapContextTOTP#nonce)。
// 对抗侧使用 4A 实际派生的完整 context 形态：WrapContextCredentials + "#" + mesh
// （credentialWrapKey 的 mesh 由 AK 派生，见 pkg/server/credentials_handler.go）。
func TestDeriveTOTPWrapKey_CrossContext(t *testing.T) {
	const code = "123456"
	const ak = "ak-totp-1234567890abcdef"
	const nonce = "aabbccdd"

	totpK, err := DeriveTOTPWrapKey(code, ak, nonce)
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey: %v", err)
	}
	// 对抗侧：server recover / renew 实际用的 credentials wrap 路径
	// （credentialWrapKey = DeriveWrapKey(entry.SK, ak, WrapContextCredentials#mesh)）。
	mesh := ParseMesh(ak) // "totp"；4A 由 AK 派生并化入 context（无 mesh 则裸前缀）
	credsCtx := WrapContextCredentials + "#" + mesh
	credsK, err := wrapKey(sumSHA256([]byte(code)), ak, credsCtx)
	if err != nil {
		t.Fatalf("wrapKey(credentials ctx): %v", err)
	}
	if bytes.Equal(totpK, credsK) {
		t.Fatalf("TOTP 与 credentials 两个 context 派生 key 不得相同（防跨 context 复用）")
	}
	// 派生参数自检：wrapKey(sha256(code), ak, WrapContextTOTP+"#"+nonce)。
	derived, err := DeriveWrapKey(sumSHA256([]byte(code)), ak, WrapContextTOTP+"#"+nonce)
	if err != nil {
		t.Fatalf("DeriveWrapKey: %v", err)
	}
	if !bytes.Equal(totpK, derived) {
		t.Fatalf("DeriveTOTPWrapKey 未按 wrapKey(sha256(code), ak, WrapContextTOTP#nonce) 实现")
	}
}

// sumSHA256 计算 sha256.Sum256 返回切片（测试辅助）。
func sumSHA256(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}
