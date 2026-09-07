// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// EncryptWithKey 用 32B master key 加密任意长明文（AES-256-GCM），输出字节级信封：
//
//	密文 = nonce(12B 随机前置) || GCM ciphertext（认证 tag 含尾）
//
// 与 wrap.go 的 EncryptSecretKind（SK 信封）同构但**无 32B 明文长度约束**——凭据文件
// 是任意长 JSON，故收归独立实现（4C KMS 的 DEK 信封复用本函数）。
//
// 格式说明（S-1）：信封为 `nonce || ct`，**无版本/魔术头**——算法或参数升级（如改
// nonce 长度 / 换 AEAD）无法靠字节自描述区分，需整体迁移既有密文文件（重加密为
// 新格式）；未来引入 v2 时可在密文最前加 1B 格式 tag 做显式版本标识，本实现刻意
// 保持最小信封（凭据文件由 master key 域隔离 + 原子写管理生命周期）。
//
// AAD 说明（建议 C）：当前**无 AAD**（gcm.Seal/Open 的 additionalData 传 nil）——单一
// 全局 credentials.json + 单一 master key 下安全（密文不可搬移别处仍被同一 key 解开，
// 因为目标路径本就是同一文件）。未来若多文件共用同一 master key，应以 path/owner 作
// AAD（`gcm.Seal(nil, nonce, plaintext, []byte(path))`）绑定密文到文件身份，防跨文件
// 密文搬移/替换；本实现刻意保持最小接口，AAD 由调用方在需要时经加密上下文引入。
func EncryptWithKey(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidMasterKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("accesskey: encrypt with key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("accesskey: encrypt with key gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("accesskey: encrypt with key nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plaintext, nil)
	return append(nonce, ct...), nil
}

// DecryptWithKey 用 32B master key 解开 EncryptWithKey 产物。任何认证失败（密钥错、
// 密文篡改、nonce 篡改）或坏格式（短于 nonce、不足 GCM tag）都返回 error（fail-closed，
// 不 panic）。
func DecryptWithKey(key, data []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidMasterKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("accesskey: decrypt with key: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("accesskey: decrypt with key gcm: %w", err)
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("accesskey: decrypt with key: 密文过短（缺少 nonce），非 EncryptWithKey 产物")
	}
	nonce, ct := data[:gcm.NonceSize()], data[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("accesskey: decrypt with key: %w", err)
	}
	return pt, nil
}

// AESGCMStorer 是 SecureStorer 的 AES-256-GCM 加密值类型实现：Encrypt/Decrypt 委托
// EncryptWithKey/DecryptWithKey（Key 为 32B master key）。EncryptingStorer 以它为
// 加密后端；4C-3 的 KMSStorer 复用 SecureStorer 形态（DEK 信封亦走 EncryptWithKey）。
type AESGCMStorer struct {
	// Key 是 32B AES-256 master key（直接作密钥；nonce 随机已提供语义安全）。
	Key []byte
}

// 编译期断言：AESGCMStorer 满足 SecureStorer（防签名漂移）。
var _ SecureStorer = AESGCMStorer{}

// Encrypt 用 AES-256-GCM 密封明文（随机 nonce 前置）。
func (s AESGCMStorer) Encrypt(plaintext []byte) ([]byte, error) {
	return EncryptWithKey(s.Key, plaintext)
}

// Decrypt 解开 EncryptWithKey/AESGCMStorer.Encrypt 产物；认证失败/坏格式报错。
func (s AESGCMStorer) Decrypt(ciphertext []byte) ([]byte, error) {
	return DecryptWithKey(s.Key, ciphertext)
}
