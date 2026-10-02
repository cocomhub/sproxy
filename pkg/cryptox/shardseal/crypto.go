// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"

	"golang.org/x/crypto/scrypt"
)

// 设计 §4.2：AES-256-GCM + scrypt 派生。
//
//   - 每文件随机盐（32B）→ scrypt(secret, salt) 派生文件密钥（32B）；
//   - 每块随机 nonce（12B）；
//   - 块格式：[salt][nonce][ciphertext+GCMtag]（salt 每块重复写入——分块独立自描述，
//     任意分块可单独解密，不需依赖 meta 之外的状态）。
//
// scrypt 参数（N=131072=2^17, r=8, p=1）：OWASP 交互式登录推荐档位（~100ms 量级），
// 适合低频整文件加密/解密路径；不用于高频流路径（本包按设计只做分块整文件加密）。
// N=65536 曾判定为弱档（Sonar go:S5344），2026-10-02 提升至 2^17。
// 派生为每文件一次（解密路径由 meta.Salt 派生一次后逐块复用，不做逐块派生）。

// scryptN/scryptR/scryptP 是 scrypt 派生参数。
const (
	scryptN = 1 << 17
	scryptR = 8
	scryptP = 1
	// SaltLen 是文件级盐长度（32B）。
	SaltLen = 32
	// NonceLen 是 GCM nonce 长度（12B）。
	NonceLen = 12
	// KeyLen 是派生的文件密钥长度（32B，AES-256）。
	KeyLen = 32
)

// deriveKey 用 scrypt 从 secret + salt 派生文件密钥（AES-256）。
func deriveKey(secret, salt []byte) ([]byte, error) {
	if len(secret) == 0 {
		return nil, fmt.Errorf("shardseal: secret 为空（禁止空密钥派生）")
	}
	key, err := scrypt.Key(secret, salt, scryptN, scryptR, scryptP, KeyLen)
	if err != nil {
		return nil, fmt.Errorf("shardseal: scrypt 派生失败: %w", err)
	}
	return key, nil
}

// newGCM 按文件密钥构造 AES-256-GCM AEAD。
func newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, fmt.Errorf("shardseal: 文件密钥长度 %d，应为 %d", len(key), KeyLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("shardseal: AES 构造失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("shardseal: GCM 构造失败: %w", err)
	}
	return gcm, nil
}

// encryptBlock 加密单个分块：返回 [salt][nonce][ciphertext+tag]。
func encryptBlock(key, salt, plain []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("shardseal: 随机 nonce 失败: %w", err)
	}
	ct := gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, SaltLen+NonceLen+len(ct))
	out = append(out, salt...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// decryptBlock 解密单个分块（格式 [salt][nonce][ciphertext+tag]）。key 是调用方
// 按文件级 salt 派生一次的密钥（DecryptFile 每文件只派生一次，避免逐块重复 scrypt）；
// expectSalt 是 meta 中记录的文件级盐，与块内 salt 一致性校验（防块被替换/错位）。
func decryptBlock(key, expectSalt, blob []byte) ([]byte, error) {
	if len(blob) < SaltLen+NonceLen+16 {
		return nil, fmt.Errorf("shardseal: 分块过短（len=%d）", len(blob))
	}
	if !bytes.Equal(blob[:SaltLen], expectSalt) {
		return nil, fmt.Errorf("shardseal: 分块 salt 与 meta 不一致（块被替换或损坏）")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := blob[SaltLen : SaltLen+NonceLen]
	ct := blob[SaltLen+NonceLen:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 解密失败（密钥错误或密文被篡改）: %w", err)
	}
	return plain, nil
}

// newSalt 生成文件级随机盐。
func newSalt() ([]byte, error) {
	s := make([]byte, SaltLen)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("shardseal: 随机盐失败: %w", err)
	}
	return s, nil
}
