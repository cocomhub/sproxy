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
func EncryptWithKey(key, plaintext []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidSecret
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
// 密文篡改、nonce 篡改）或坏格式（短于 nonce）都返回 error（fail-closed）。
func DecryptWithKey(key, data []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, ErrInvalidSecret
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
