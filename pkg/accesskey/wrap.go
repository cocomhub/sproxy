// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"golang.org/x/crypto/hkdf"
)

// WrappedSecret 是信封加密的输出信封：包裹的 SK 密文 + 会话 nonce + 元信息。
// JSON tag 对齐持久化/线上传输（nonce/ciphertext 均为 base64）。
type WrappedSecret struct {
	// Kind 包裹密钥的形态（secret_wrap / totp_wrap）——校验 Kind 与请求的 wrap context。
	Kind Kind `json:"kind"`
	// WrapKeyID 包裹密钥（wrap key）的提供者 AK 标识。
	WrapKeyID string `json:"wrap_key_id"`
	// Nonce AES-GCM 会话随机 nonce（随机生成前置，12 字节）。
	Nonce []byte `json:"nonce"`
	// Cipher 包裹后的 SK 密文（含 GCM 认证标签），Decrypt 还原出 32B SK。
	Cipher []byte `json:"ciphertext"`
}

const (
	// gcmTagSize 是 AES-GCM 认证标签字节数（16）。
	gcmTagSize = 16
)

// wrapKey 从 SK + AK + context 派生信封密钥（HKDF-SHA256）：
//
//	key = HKDF-SHA256(secret=sk,
//	                  salt="sproxy-accesskey-wrap/v1\x00"+context,
//	                  info=ak)
//
// salt 含 context 使不同用途（不同 mesh / 不同端点）派生不同密钥；
// info 绑定调用方显式传入的 AK 标识，防止同一 SK secret 跨 AK 复用信封密钥。
// 输出 32B（AES-256）。
func wrapKey(sk []byte, ak, context string) ([]byte, error) {
	if len(sk) != 32 {
		return nil, ErrInvalidSecret
	}
	salt := append([]byte("sproxy-accesskey-wrap/v1\x00"), []byte(context)...)
	k := make([]byte, 32)
	hk := hkdf.New(sha256.New, sk, salt, []byte(ak))
	if _, err := io.ReadFull(hk, k); err != nil {
		return nil, fmt.Errorf("wrapKey hkdf: %w", err)
	}
	return k, nil
}

// DeriveWrapKey 是 wrapKey 的导出封装，供上层包（server/hub 等）派生同源信封密钥：
//
//	key = HKDF-SHA256(secret=sk, salt="sproxy-accesskey-wrap/v1\x00"+context, info=ak)
//
// 与内部 wrapKey 完全同实现（唯一实现，避免两处漂移）。上层包必须与包裹时的 sk/ak/
// context 一致才能解出新 SK——这正是"持有某旧 SK 才能解开对应新 SK"访问控制的基础。
func DeriveWrapKey(sk []byte, ak, context string) ([]byte, error) {
	return wrapKey(sk, ak, context)
}

// EncryptSecret 用信封密钥 wrapKey 加密 32B SK，返回信封（随机 nonce 前置）。
func EncryptSecret(wrapAK string, sk, wrapKey []byte) (*WrappedSecret, error) {
	if len(sk) != 32 {
		return nil, ErrInvalidSecret
	}
	if len(wrapKey) != 32 {
		return nil, ErrInvalidSecret
	}
	block, err := aes.NewCipher(wrapKey)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("encrypt secret nonce: %w", err)
	}
	ct := gcm.Seal(nil, nonce, sk, nil)
	return &WrappedSecret{
		Kind:      KindSecretWrap,
		WrapKeyID: wrapAK,
		Nonce:     nonce,
		Cipher:    ct,
	}, nil
}

// DecryptSecret 用 envelopeWrapKey 解开信封，还原 32B SK。任何认证失败（密钥错、
// 密文篡改、nonce 篡改）都返回错误（GCM auth 失败），且校验 Kind 必须为 secret_wrap。
func DecryptSecret(w *WrappedSecret, envelopeWrapKey []byte) ([]byte, error) {
	if w == nil {
		return nil, errors.New("accesskey: nil wrapped secret")
	}
	if w.Kind != KindSecretWrap {
		return nil, fmt.Errorf("accesskey: unexpected wrap kind %q", w.Kind)
	}
	if len(envelopeWrapKey) != 32 {
		return nil, ErrInvalidSecret
	}
	block, err := aes.NewCipher(envelopeWrapKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt secret gcm: %w", err)
	}
	if len(w.Nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("accesskey: bad nonce size")
	}
	sk, err := gcm.Open(nil, w.Nonce, w.Cipher, nil)
	if err != nil {
		return nil, fmt.Errorf("accesskey: decrypt secret: %w", err)
	}
	return sk, nil
}
