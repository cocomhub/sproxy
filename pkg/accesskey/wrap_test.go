// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestWrapKey_Deterministic wrapKey 确定性：同输入两次结果一致；不同 context 不同 key。
func TestWrapKey_Deterministic(t *testing.T) {
	sk := make([]byte, 32)
	for i := range sk {
		sk[i] = byte(i)
	}
	k1, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx-a")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	k2, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx-a")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Fatalf("同输入 wrapKey 应确定性一致")
	}
	k3, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx-b")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	if bytes.Equal(k1, k3) {
		t.Fatalf("不同 context 的 wrapKey 应不同")
	}
	// 不同 ak（info）派生不同 key（修复轮 1#4：info 绑定 AK）
	k4, err := wrapKey(sk, "ak-other-1234567890abcdef", "ctx-a")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	if bytes.Equal(k1, k4) {
		t.Fatalf("不同 ak 的 wrapKey 应不同")
	}
	// 非 32B sk 报错
	if _, err := wrapKey([]byte("short"), "ak-mesh-1234567890abcdef", "ctx"); err == nil {
		t.Fatalf("非 32B sk 的 wrapKey 应报错")
	}
}

// TestWrapKey_DifferentMaterials 不同 sk / 不同 ak（info）派生不同 key。
func TestWrapKey_DifferentMaterials(t *testing.T) {
	skA := bytes.Repeat([]byte{0x01}, 32)
	skB := bytes.Repeat([]byte{0x02}, 32)
	kA, err := wrapKey(skA, "ak-mesh-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey A: %v", err)
	}
	kB, err := wrapKey(skB, "ak-mesh-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey B: %v", err)
	}
	if bytes.Equal(kA, kB) {
		t.Fatalf("不同 sk 派生 key 应不同")
	}
	// 同 sk 不同 ak（info）也派生不同 key
	kC, err := wrapKey(skA, "ak-other-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey C: %v", err)
	}
	if bytes.Equal(kA, kC) {
		t.Fatalf("不同 ak 派生 key 应不同")
	}
}

// TestEncryptDecrypt_Roundtrip EncryptSecret / DecryptSecret 往返成功。
func TestEncryptDecrypt_Roundtrip(t *testing.T) {
	sk := bytes.Repeat([]byte{0x42}, 32)
	wrapK, err := wrapKey(sk, "ak-mesh-a-1234567890abcdef", "mesh-a")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	// 两个不同密文的 wrap 密钥（一个来自其他 sk 的派生、一个 context 混用）——见下述独立测试
	ws, err := EncryptSecret("ak-mesh-a-1234567890abcdef", sk, wrapK)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if ws == nil {
		t.Fatalf("EncryptSecret 返回 nil")
	}
	if ws.Kind != KindSecretWrap {
		t.Fatalf("Kind 应为 secret_wrap, got %q", ws.Kind)
	}
	if ws.WrapKeyID == "" {
		t.Fatalf("WrapKeyID 不应为空")
	}
	if len(ws.Nonce) == 0 {
		t.Fatalf("Nonce 前置（随机会话 nonce）非空")
	}
	// 密文应为 32B（GCM 标签 16B 附加，去掉 nonce）
	if len(ws.Cipher) != 32+gcmTagSize {
		t.Fatalf("Cipher 长度应为 32+16, got %d", len(ws.Cipher))
	}

	got, err := DecryptSecret(ws, wrapK)
	if err != nil {
		t.Fatalf("DecryptSecret: %v", err)
	}
	if !bytes.Equal(got, sk) {
		t.Fatalf("往返后 SK 不匹配")
	}
}

// TestEncryptDecrypt_Tamper 密文篡改 → 解密报错（GCM auth 失败）。
func TestEncryptDecrypt_Tamper(t *testing.T) {
	sk := bytes.Repeat([]byte{0x77}, 32)
	wrapK, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	ws, err := EncryptSecret("ak-a-1234567890abcdef", sk, wrapK)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	// 篡改 Cipher 一个字节
	tampered := *ws
	tampered.Cipher = append([]byte(nil), ws.Cipher...)
	tampered.Cipher[0] ^= 0xff
	if _, err := DecryptSecret(&tampered, wrapK); err == nil {
		t.Fatalf("篡改密文应解密失败")
	}
	// 篡改 Nonce 一个字节
	tampered2 := *ws
	tampered2.Nonce = append([]byte(nil), ws.Nonce...)
	tampered2.Nonce[0] ^= 0x01
	if _, err := DecryptSecret(&tampered2, wrapK); err == nil {
		t.Fatalf("篡改 Nonce 应解密失败")
	}
	// 篡改 Kind（wrap 协议上下文校验失败）
	tampered3 := *ws
	tampered3.Kind = KindPlain
	if _, err := DecryptSecret(&tampered3, wrapK); err == nil {
		t.Fatalf("篡改 Kind 应解密失败")
	}
}

// TestEncryptDecrypt_ContextMismatch context 混用（派生 key 不同）→ 解密失败。
func TestEncryptDecrypt_ContextMismatch(t *testing.T) {
	sk := bytes.Repeat([]byte{0x31}, 32)
	kA, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx-a")
	if err != nil {
		t.Fatalf("wrapKey A: %v", err)
	}
	kB, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx-b")
	if err != nil {
		t.Fatalf("wrapKey B: %v", err)
	}
	ws, err := EncryptSecret("ak-a-1234567890abcdef", sk, kA)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	// 用不同 context 派生的 key 解 → 失败
	if _, err := DecryptSecret(ws, kB); err == nil {
		t.Fatalf("context 混用应解密失败")
	}
}

// TestEncryptDecrypt_WrongEnvelopeKey 用错误信封密钥（其他 sk 派生）解密失败。
func TestEncryptDecrypt_WrongEnvelopeKey(t *testing.T) {
	sk := bytes.Repeat([]byte{0x51}, 32)
	other := bytes.Repeat([]byte{0x52}, 32)
	wrapK, err := wrapKey(sk, "ak-mesh-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey: %v", err)
	}
	otherK, err := wrapKey(other, "ak-mesh-1234567890abcdef", "ctx")
	if err != nil {
		t.Fatalf("wrapKey(other): %v", err)
	}
	ws, err := EncryptSecret("ak-a-1234567890abcdef", sk, wrapK)
	if err != nil {
		t.Fatalf("EncryptSecret: %v", err)
	}
	if _, err := DecryptSecret(ws, otherK); err == nil {
		t.Fatalf("错误信封密钥应解密失败")
	}
}

// TestEncryptDecryptKind_TOTPKind EncryptSecretKind(KindTOTPWrap, ...) 产出 Kind=="totp_wrap"
// 信封；DecryptSecretKind(w, KindTOTPWrap, key) 往返成功（4B-2 TOTP 登录解 session SK 路径）。
func TestEncryptDecryptKind_TOTPKind(t *testing.T) {
	sk := must32BHex(t, 0x77)
	totpK, err := DeriveTOTPWrapKey("123456", "ak-totp-1234567890abcdef", "aabbccdd")
	if err != nil {
		t.Fatalf("DeriveTOTPWrapKey: %v", err)
	}
	env, err := EncryptSecretKind(KindTOTPWrap, "ak-totp-1234567890abcdef", sk, totpK)
	if err != nil {
		t.Fatalf("EncryptSecretKind: %v", err)
	}
	if env.Kind != KindTOTPWrap {
		t.Fatalf("Kind 应为 totp_wrap, got %q", env.Kind)
	}
	if env.WrapKeyID != "ak-totp-1234567890abcdef" {
		t.Fatalf("WrapKeyID 不符, got %q", env.WrapKeyID)
	}

	got, err := DecryptSecretKind(env, KindTOTPWrap, totpK)
	if err != nil {
		t.Fatalf("DecryptSecretKind: %v", err)
	}
	if !bytes.Equal(got, sk) {
		t.Fatalf("TOTP 包裹往返后 SK 不匹配")
	}

	// 默认 DecryptSecret（仅认 secret_wrap）对 TOTP 信封必须报错（防跨 kind 误认）。
	if _, derr := DecryptSecret(env, totpK); derr == nil {
		t.Fatalf("DecryptSecret 对 KindTOTPWrap 信封应报错")
	}

	// DecryptSecretKind 对 kind 不匹配（secret_wrap 信封用 totp_wrap 期望）应报错。
	envSW, err := EncryptSecretKind(KindSecretWrap, "ak-a-1234567890abcdef", sk, totpK)
	if err != nil {
		t.Fatalf("EncryptSecretKind(secret_wrap): %v", err)
	}
	if _, derr := DecryptSecretKind(envSW, KindTOTPWrap, totpK); derr == nil {
		t.Fatalf("DecryptSecretKind 期望 totp_wrap 对 secret_wrap 信封应报错")
	}
}

// TestEncryptSecretKind_DefaultKind EncryptSecretKind 默认 Kind=KindSecretWrap 时与
// EncryptSecret 行为一致（无显式 kind 时生产 secret_wrap 信封）。
func TestEncryptSecretKind_DefaultKind(t *testing.T) {
	sk := must32BHex(t, 0x42)
	ak := "ak-mesh-a-1234567890abcdef"
	wrapK := must32BHex(t, 0x11)
	env, err := EncryptSecretKind(KindSecretWrap, ak, sk, wrapK)
	if err != nil {
		t.Fatalf("EncryptSecretKind: %v", err)
	}
	if env.Kind != KindSecretWrap {
		t.Fatalf("Kind 应为 secret_wrap, got %q", env.Kind)
	}
	got, err := DecryptSecretKind(env, KindSecretWrap, wrapK)
	if err != nil {
		t.Fatalf("DecryptSecretKind: %v", err)
	}
	if !bytes.Equal(got, sk) {
		t.Fatalf("secret_wrap 往返后 SK 不匹配")
	}
}

// TestWrappedSecret_JSON WrappedSecret json tag（nonce/ciphertext）序列化往返。
func TestWrappedSecret_JSON(t *testing.T) {
	ws := WrappedSecret{
		Kind:      KindSecretWrap,
		WrapKeyID: "ak-mesh-1234567890abcdef",
		Nonce:     []byte{0xde, 0xad, 0xbe, 0xef},
		Cipher:    []byte{0x01, 0x02, 0x03},
	}
	b, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var back WrappedSecret
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if back.Kind != ws.Kind || back.WrapKeyID != ws.WrapKeyID {
		t.Fatalf("Kind/WrapKeyID 序列化往返不符")
	}
	if !bytes.Equal(back.Nonce, ws.Nonce) || !bytes.Equal(back.Cipher, ws.Cipher) {
		t.Fatalf("Nonce/Cipher 序列化往返不符")
	}
}
