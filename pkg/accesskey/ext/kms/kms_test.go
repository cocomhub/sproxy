// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package kms

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"testing"

	accesskey "github.com/cocomhub/sproxy/pkg/accesskey"
)

// recordingKMSClient 是 KMS 插件测试探针：EncryptDEK 用可辨识前缀包裹明文 DEK
// （便于断言 DEK 段在信封中可寻址），DecryptDEK 还原；同时记录每次收到的 DEK。
// 只依赖 stdlib，无 mock 库。
type recordingKMSClient struct {
	mu           sync.Mutex
	encryptedDEK [][]byte // 每次 EncryptDEK 收到的明文 DEK（防 -race 并发）
	decryptCalls int
}

// prefix 是 mock 包裹前缀（"kms:" + 原始 DEK = 返回的 KMS 密文）。
func (m *recordingKMSClient) prefix() []byte { return []byte("kms:") }

func (m *recordingKMSClient) EncryptDEK(_ context.Context, plaintext []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.encryptedDEK = append(m.encryptedDEK, append([]byte(nil), plaintext...))
	return append(append([]byte(nil), m.prefix()...), plaintext...), nil
}

func (m *recordingKMSClient) DecryptDEK(_ context.Context, ciphertext []byte) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.decryptCalls++
	if !bytes.HasPrefix(ciphertext, m.prefix()) {
		return nil, fmt.Errorf("mock KMS: 未知密文前缀")
	}
	return append([]byte(nil), ciphertext[len(m.prefix()):]...), nil
}

func (m *recordingKMSClient) lastEncryptedDEK() []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.encryptedDEK) == 0 {
		return nil
	}
	return append([]byte(nil), m.encryptedDEK[len(m.encryptedDEK)-1]...)
}

func (m *recordingKMSClient) encryptDEKCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.encryptedDEK)
}

// TestKMSStorer_Roundtrip_EnvelopeSelfDescribing 验证 KMSStorer 核心契约：
//   - Encrypt→Decrypt 往返还原明文；
//   - 密文信封自描述（magic 前缀 + 2B BE 长度段），DEK 密文在信封中可寻址；
//   - 信封内 DEK 确实就是 EncryptWithKey 加密数据所用的数据密钥（用 accesskey
//     导出原语独立还原正文）。
func TestKMSStorer_Roundtrip_EnvelopeSelfDescribing(t *testing.T) {
	mock := &recordingKMSClient{}
	s := NewKMSStorer(mock)
	plaintext := []byte(`{"version":1,"keys":[{"ak":"ak-test-4c3"}]}`)

	ct, err := s.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// 信封自描述：magic 前缀 + DEK 长度字段。
	if !bytes.HasPrefix(ct, []byte(magic)) {
		t.Fatalf("密文应含 magic 前缀 %q, got %q", magic, ct[:min(len(ct), len(magic))])
	}
	if len(ct) < magicLen+dekLenFieldLen {
		t.Fatalf("密文过短: %d", len(ct))
	}
	dekLen := int(binary.BigEndian.Uint16(ct[magicLen : magicLen+dekLenFieldLen]))
	segStart := magicLen + dekLenFieldLen
	if segStart+dekLen > len(ct) {
		t.Fatalf("DEK 长度字段越界: dek_len=%d, 实际剩余=%d", dekLen, len(ct)-segStart)
	}
	seg := ct[segStart : segStart+dekLen]
	// mock 包裹格式："kms:" + 32B DEK → 段可寻址。
	if !bytes.HasPrefix(seg, mock.prefix()) {
		t.Fatalf("DEK 段应为 mock 包裹格式（含 %q 前缀）", mock.prefix())
	}
	dek := seg[len(mock.prefix()):]
	if len(dek) != 32 {
		t.Fatalf("DEK 应为 32B（AES-256），got %d", len(dek))
	}
	body := ct[segStart+dekLen:]

	// 独立还原：用信封内 DEK + accesskey 导出原语解开正文，应等于明文。
	got, err := accesskey.DecryptWithKey(dek, body)
	if err != nil {
		t.Fatalf("用信封内 DEK 独立解密正文: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("信封内 DEK 解密结果与明文不一致: got %q", got)
	}

	// mock 记录：EncryptDEK 收到的 DEK 与信封内 DEK 一致。
	if n := mock.encryptDEKCount(); n != 1 {
		t.Fatalf("EncryptDEK 应恰好调用 1 次, got %d", n)
	}
	if captured := mock.lastEncryptedDEK(); !bytes.Equal(captured, dek) {
		t.Fatalf("KMS 包裹的 DEK 与信封内 DEK 不一致")
	}

	// 整体 Decrypt 往返。
	dec, err := s.Decrypt(ct)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(dec, plaintext) {
		t.Fatalf("Decrypt 应还原明文: got %q", dec)
	}
}

// TestKMSStorer_Decrypt_TamperFails 验证解密对篡改的 fail-closed：正文任一字节翻转
// （GCM 认证失败）与 magic 篡改（格式错误）都必须报错，绝不返回错误明文。
func TestKMSStorer_Decrypt_TamperFails(t *testing.T) {
	mock := &recordingKMSClient{}
	s := NewKMSStorer(mock)
	plaintext := []byte("credentials-snapshot-with-secrets")
	ct, err := s.Encrypt(plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}

	// 篡改正文区（跳过 magic/长度段，命中 body 的 nonce/ct/tag）。
	bodyStart := magicLen + dekLenFieldLen + int(binary.BigEndian.Uint16(ct[magicLen:magicLen+dekLenFieldLen]))
	if bodyStart >= len(ct) {
		t.Fatalf("测试前提不成立：无正文区")
	}
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-5] ^= 0xFF // 翻转 body 尾部（GCM tag 区）
	if _, err := s.Decrypt(tampered); err == nil {
		t.Fatalf("篡改正文后 Decrypt 应失败（fail-closed）")
	}

	// 篡改 magic → 格式错误（自描述诊断路径）。
	badMagic := append([]byte(nil), ct...)
	badMagic[0] ^= 0xFF
	if _, err := s.Decrypt(badMagic); err == nil {
		t.Fatalf("篡改 magic 后 Decrypt 应失败（格式错误）")
	}

	// 截断信封 → 错误。
	truncated := ct[:bodyStart-1]
	if _, err := s.Decrypt(truncated); err == nil {
		t.Fatalf("截断密文后 Decrypt 应失败")
	}
}

// TestKMSStorer_Unconfigured_ErrNotConfigured 验证骨架默认态 fail-closed：
// nil/未配置 KMS 客户端 → Encrypt/Decrypt 返回 ErrNotConfigured（哨兵错误），
// 非占位、非 panic。
func TestKMSStorer_Unconfigured_ErrNotConfigured(t *testing.T) {
	s := NewKMSStorer(nil) // 默认未配置态
	if _, err := s.Encrypt([]byte("x")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置 Encrypt 应返回 ErrNotConfigured, got %v", err)
	}
	// 即使输入是非法信封，未配置也应先行报 ErrNotConfigured（fail-fast）。
	if _, err := s.Decrypt([]byte("not-an-envelope")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("未配置 Decrypt 应返回 ErrNotConfigured, got %v", err)
	}

	// UnconfiguredClient 显式注入同样返回哨兵错误。
	var uc UnconfiguredClient
	if _, err := uc.EncryptDEK(context.Background(), []byte("dek")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("UnconfiguredClient.EncryptDEK 应返回 ErrNotConfigured, got %v", err)
	}
	if _, err := uc.DecryptDEK(context.Background(), []byte("dek")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("UnconfiguredClient.DecryptDEK 应返回 ErrNotConfigured, got %v", err)
	}
}

// TestKMSStorer_RegistryIntegration 验证注册表接入：
//   - init() 已把默认未配置骨架注册为 "kms"，GetStorer[SecureStorer] 命中且
//     Encrypt 返回 ErrNotConfigured；
//   - 自定义 KMSStorer 注册后经注册表取值可完成加解密往返；
//   - UnregisterStorer 后不再命中。
func TestKMSStorer_RegistryIntegration(t *testing.T) {
	// init 注册的默认项。
	def, ok := accesskey.GetStorer[accesskey.SecureStorer]("kms")
	if !ok {
		t.Fatalf("init 注册的 %q 应命中 SecureStorer", "kms")
	}
	if _, err := def.Encrypt([]byte("x")); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("默认注册项 Encrypt 应返回 ErrNotConfigured, got %v", err)
	}

	// 自定义注册 + 往返。
	const name = "kms-test-4c3"
	mock := &recordingKMSClient{}
	if err := accesskey.RegisterStorer(name, NewKMSStorer(mock)); err != nil {
		t.Fatalf("RegisterStorer: %v", err)
	}
	got, ok := accesskey.GetStorer[accesskey.SecureStorer](name)
	if !ok {
		t.Fatalf("GetStorer[SecureStorer](%q) 应命中", name)
	}
	ct, err := got.Encrypt([]byte("hello-kms"))
	if err != nil {
		t.Fatalf("注册项 Encrypt: %v", err)
	}
	dec, err := got.Decrypt(ct)
	if err != nil {
		t.Fatalf("注册项 Decrypt: %v", err)
	}
	if !bytes.Equal(dec, []byte("hello-kms")) {
		t.Fatalf("注册项往返不一致: got %q", dec)
	}

	accesskey.UnregisterStorer(name)
	if _, ok := accesskey.GetStorer[accesskey.SecureStorer](name); ok {
		t.Fatalf("UnregisterStorer 后不应命中")
	}
}
