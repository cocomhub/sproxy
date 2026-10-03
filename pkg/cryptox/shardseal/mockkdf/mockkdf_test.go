// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockkdf

import (
	"bytes"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestMockKDF_DeterministicAndSeparation 验证 HKDF 派生：同 secret+salt 确定性；不同
// secret/salt → 不同 key（HS 输出 32B）。
func TestMockKDF_DeterministicAndSeparation(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-key-000")
	saltA := bytes.Repeat([]byte{0x42}, shardseal.SaltLen)
	k1, err := MockKDF(secret, saltA)
	if err != nil {
		t.Fatalf("MockKDF: %v", err)
	}
	if len(k1) != shardseal.KeyLen {
		t.Fatalf("派生 key 长 %d，应为 %d", len(k1), shardseal.KeyLen)
	}
	// 确定性：同 secret+salt → 同 key。
	k2, err := MockKDF(secret, saltA)
	if err != nil {
		t.Fatalf("MockKDF#2: %v", err)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("同 secret+salt 应派生相同 key（确定性）")
	}
	// 不同 salt → 不同 key。
	saltB := bytes.Repeat([]byte{0x24}, shardseal.SaltLen)
	k3, _ := MockKDF(secret, saltB)
	if bytes.Equal(k1, k3) {
		t.Error("不同 salt 应派生不同 key")
	}
	// 不同 secret → 不同 key。
	k4, _ := MockKDF([]byte("other-secret-key"), saltA)
	if bytes.Equal(k1, k4) {
		t.Error("不同 secret 应派生不同 key")
	}
	// 空输入 fail-closed。
	if _, err := MockKDF(nil, saltA); err == nil {
		t.Error("空 secret 应报错")
	}
	if _, err := MockKDF(secret, nil); err == nil {
		t.Error("空 salt 应报错")
	}
}

// TestMockAlgorithm_RoundTrip 验证 mock 算法经注册表装配后完整可用：KDFOverride 注入
// （DeriveKey 走 HKDF 而非真实 scrypt）、派生的 key 与「secret||KDFDomain 直算 MockKDF」
// 一致（域分离语义）、真实 AES-GCM 加密/解密 roundtrip、篡改密文 fail-closed。
func TestMockAlgorithm_RoundTrip(t *testing.T) {
	t.Parallel()
	RegisterMockAlgorithm()
	// 可重复注册不 panic（once 守卫）。
	RegisterMockAlgorithm()

	v, err := shardseal.ResolveAlgorithm(MockAlgorithmName)
	if err != nil {
		t.Fatalf("ResolveAlgorithm(%q): %v", MockAlgorithmName, err)
	}
	if v != MockAlgoVersion {
		t.Errorf("解析 version=%d，应为 %d", v, MockAlgoVersion)
	}
	alg, ok := shardseal.AlgoByVersion(v)
	if !ok {
		t.Fatal("AlgoByVersion 应返回 mock 算法注册")
	}
	if alg.KDFOverride == nil {
		t.Fatal("mock 算法应注入 KDFOverride（否则走真实 scrypt 非轻量）")
	}
	if alg.Name != MockAlgorithmName {
		t.Errorf("mock 算法名=%q，应为 %q", alg.Name, MockAlgorithmName)
	}

	secret := []byte("test-secret-key-000")
	salt := bytes.Repeat([]byte{0x24}, shardseal.SaltLen)
	key, err := shardseal.DeriveKey(secret, salt, v)
	if err != nil {
		t.Fatalf("DeriveKey(mock): %v", err)
	}
	if len(key) != shardseal.KeyLen {
		t.Fatalf("派生 key 长度=%d，应为 %d", len(key), shardseal.KeyLen)
	}
	// deriveKey 混入 KDF 域标记：DeriveKey 结果应等于 secret||KDFDomain 的直算 MockKDF。
	material := append(append([]byte(nil), secret...), []byte(alg.KDFDomain)...)
	direct, derr := MockKDF(material, salt)
	if derr != nil {
		t.Fatalf("MockKDF 直算失败: %v", derr)
	}
	if !bytes.Equal(key, direct) {
		t.Error("DeriveKey(mock) 应等于 secret||KDFDomain 的 MockKDF（域标记混入派生）")
	}

	// 真实 AES-GCM 加密/解密 roundtrip（key 派生轻量、加密真实）。
	plain := []byte("secret volume content 文件内容 12345")
	blob, serr := alg.Encrypt(key, salt, plain)
	if serr != nil {
		t.Fatalf("Encrypt: %v", serr)
	}
	got, oerr := alg.Decrypt(key, salt, blob)
	if oerr != nil {
		t.Fatalf("Decrypt: %v", oerr)
	}
	if !bytes.Equal(got, plain) {
		t.Errorf("roundtrip 内容不一致：len(got)=%d len(plain)=%d", len(got), len(plain))
	}

	// 篡改密文 → GCM 认证失败 fail-closed。
	tampered := append([]byte(nil), blob...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := alg.Decrypt(key, salt, tampered); err == nil {
		t.Error("篡改密文应解密失败（GCM 认证）")
	}
	// 错误密钥 → 解密失败。
	if _, err := alg.Decrypt(bytes.Repeat([]byte{0xAA}, shardseal.KeyLen), salt, blob); err == nil {
		t.Error("错误密钥应解密失败")
	}
}
