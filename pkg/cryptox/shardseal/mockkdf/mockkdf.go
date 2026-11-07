// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mockkdf 提供轻量派生（HKDF-SHA256）的 mock 算法，供依赖方（secretdata
// 装配层等）的**测试/开发**使用，替代真实 scrypt 档位的过渡取舍。
//
// 分层原则：算法可靠性（真实 scrypt 档的 roundtrip / 跨档 fail-closed）由
// shardseal 包负责；secretdata 是「组装编排」，其测试用 mock 派生 + 真实 AES-GCM
// 加密——组装正确性（key/salt/格式传递、密文往返）仍真实验证，仅派生轻量化
// （HKDF ~µs，而非真实 scrypt 的毫秒~百毫秒）。
//
// **本包仅供测试/开发，生产装配不得调用/引用**（mock 派生对低熵/生产机密无防御
// 价值；生产默认档仍是 shardseal 的 standard 真实 scrypt）。
package mockkdf

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// MockAlgorithmName 是 mock 算法的注册标识（写进 meta.algorithm；secretdata 测试的
// testAlgo 即此值）。算法可靠性由 shardseal 真实档维护，本名仅供测试/开发装配。
const MockAlgorithmName = "shardseal/aes-256-gcm-mock"

// MockAlgoVersion 是 mock 算法的 AlgoVersion。版本 4 复用原 test 档移除后释放的槽位
// （真实档 high/standard/low = 1/2/3 不变；mock 仅测试/dev 动态注册，生产不占版本）。
// 与真实档「不同 AlgoVersion + 不同 KDF 派生域」同构——跨档 fail-closed 语义不变。
const MockAlgoVersion shardseal.AlgoVersion = 4

// mockKDFDomain 是 mock 算法的 KDF 派生域标记（deriveKey 混入 secret，域分离：与任何
// 真实档域不同 → 同 secret+salt 派生 key 不同，跨档 fail-closed 成立）。
const mockKDFDomain = "shardseal/v1-mock"

// mockHKDFInfoLabel 是 HKDF info 段固定标签：info = salt || 标签（绑定用途，防跨用途
// 复用派生结果；secret 已含 KDF 域标记，标签仅锚定「这是 mock 派生」的用途）。
const mockHKDFInfoLabel = "shardseal-mockkdf/v1"

// MockKDF 是 mock 算法的 HKDF-SHA256 轻量派生（~µs）：
//
//	key = HKDF-SHA256(IKM=secret, salt=nil, info=salt || "shardseal-mockkdf/v1")，输出 32B
//
// 同 secret+salt 确定性（派生缓存/跨挂载 key 稳定）；不同 secret/salt → 不同 key。
// 返回 32B AES-256 key。错误 %w 包装。deriveKey 以 secret||KDFDomain 传入（域分离）。
func MockKDF(secret, salt []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, errors.New("shardseal/mockkdf: secret 为空（禁止空密钥派生）")
	}
	if len(salt) == 0 {
		return nil, errors.New("shardseal/mockkdf: salt 为空")
	}
	info := string(salt) + mockHKDFInfoLabel
	key, kerr := hkdf.Key(sha256.New, secret, nil, info, shardseal.KeyLen)
	if kerr != nil {
		return nil, fmt.Errorf("shardseal/mockkdf: HKDF 派生失败: %w", kerr)
	}
	return key, nil
}

// mockSeal 是 mock 算法的真实 AES-256-GCM 加密回调（Algorithm.Encrypt 签名）：派生
// key + salt + 明文 → [salt][nonce][ct+tag]。仅供 mock 算法回调往返测试用；高层加密
// 组装（secretdata 写路径）走 shardseal 内部统一格式（sealBlock），与本回调无关。
func mockSeal(key, salt, plain []byte) ([]byte, error) {
	if len(key) != shardseal.KeyLen {
		return nil, fmt.Errorf("shardseal/mockkdf: 密钥长度 %d，应为 %d", len(key), shardseal.KeyLen)
	}
	gcm, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("shardseal/mockkdf: nonce 随机失败: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, len(salt)+len(nonce)+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// mockOpen 是 mockSeal 的逆（Algorithm.Decrypt 签名）：解析 salt/nonce/ct → GCM 解密。
// 校验 blob 内嵌 salt 与入参一致（防护错位）；篡改密文 GCM 认证失败（fail-closed）。
func mockOpen(key, salt, blob []byte) ([]byte, error) {
	if len(key) != shardseal.KeyLen {
		return nil, fmt.Errorf("shardseal/mockkdf: 密钥长度 %d，应为 %d", len(key), shardseal.KeyLen)
	}
	gcm, err := newAESGCM(key)
	if err != nil {
		return nil, err
	}
	nonceLen := gcm.NonceSize()
	if len(blob) < shardseal.SaltLen+nonceLen+16 {
		return nil, fmt.Errorf("shardseal/mockkdf: blob 过短（len=%d）", len(blob))
	}
	if !bytes.Equal(blob[:shardseal.SaltLen], salt) {
		return nil, errors.New("shardseal/mockkdf: blob salt 与入参不一致")
	}
	plain, oerr := gcm.Open(nil, blob[shardseal.SaltLen:shardseal.SaltLen+nonceLen], blob[shardseal.SaltLen+nonceLen:], nil)
	if oerr != nil {
		return nil, fmt.Errorf("shardseal/mockkdf: 解密失败（密钥错误或密文被篡改）: %w", oerr)
	}
	return plain, nil
}

// newAESGCM 构造 AES-256-GCM AEAD（字节等长校验；返回统一错误包装）。
func newAESGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("shardseal/mockkdf: AES 构造失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("shardseal/mockkdf: GCM 构造失败: %w", err)
	}
	return gcm, nil
}

var mockOnce sync.Once

// RegisterMockAlgorithm 注册 mock 算法到 shardseal 注册表（once 守卫，可重复安全调用；
// 重复注册 panic 被抑制——mock 仅测试/开发装配，防并行注册竞态）。
//
// 算法参数：KDFOverride=MockKDF（HKDF 轻量派生）、Encrypt/Decrypt=真实 AES-256-GCM、
// ScryptN 占位 low 档参数（仅供 secretdata loadGate 并发预算——mock 派生 ~µs、无真实
// scrypt 内存，取 low 档使并发 = NumCPU 不被预算拖慢）。
func RegisterMockAlgorithm() {
	mockOnce.Do(func() {
		shardseal.RegisterAlgorithm(shardseal.Algorithm{
			Version:   MockAlgoVersion,
			Name:      MockAlgorithmName,
			KDFDomain: mockKDFDomain,
			ScryptN:   1 << 12, ScryptR: 8, ScryptP: 1,
			KDFOverride: MockKDF,
			Encrypt:     mockSeal,
			Decrypt:     mockOpen,
		})
	})
}
