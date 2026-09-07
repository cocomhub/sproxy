// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
)

// 本文件是 KMSStorer——内置的 SecureStorer 实现（DEK 信封加密）。
//
// 设计（4C-2）：数据用本地随机 32B DEK（AES-256-GCM，EncryptWithKey/DecryptWithKey）
// 加密；DEK 本身经可插拔 KMSClient 包裹。默认 KMSClient（UnconfiguredClient）返回
// ErrNotConfigured（fail-closed，非占位/panic）。
//
// KMSClient 是未来 ext 接缝：引入三方 SDK 的 AWS/GCP 适配器（那层才需要独立 go.mod）
// 未来在 pkg/accesskey/ext/kms 实现 KMSClient，再经 NewKMSStorer 注入。本内置文件
// 本身不依赖任何三方库。
//
// 密文信封格式（自描述，含 magic + 长度前缀——task 2 S-1 教训：信封无版本头难诊断）：
//
//	magic("spkms1", 6B) || dek_ciphertext_len(2B BE) || dek_ciphertext || body
//
// 其中 body = EncryptWithKey(dek, plaintext) 产物 = nonce(12B) || GCM ct。

// ErrNotConfigured 表示 KMS 未配置（需注入 KMSClient 实现后可用）。
// 作为哨兵错误供调用方 errors.Is 判定（fail-closed）。
var ErrNotConfigured = errors.New("accesskey: KMS 未配置（需注入 KMSClient 实现）")

// magic 是密文信封的自描述格式标识（魔数前缀）。算法/参数升级（如换 magic 长度或
// DEK 包裹格式）需整体迁移既有密文——变更即破坏既有密文可读性，须显式评估。
const magic = "spkms1"

const (
	magicLen       = len(magic) // 信封 magic 前缀字节数（6）
	dekLenFieldLen = 2          // dek_ciphertext 长度字段字节数（BE）
	maxDEKLen      = 1<<16 - 1  // 长度字段可表达的上限（65535）
)

// KMSClient 是可插拔 KMS 客户端：EncryptDEK/DecryptDEK 用外部 KMS 加解密数据密钥。
// 本接口是未来 ext 接缝——真实 AWS/GCP 适配器（引入三方 SDK 那层）在
// pkg/accesskey/ext/kms 实现本接口后经 NewKMSStorer 注入。
type KMSClient interface {
	// EncryptDEK 用 KMS 包裹数据密钥；返回 KMS 密文（含 key id 等元数据由实现定义）。
	EncryptDEK(ctx context.Context, plaintext []byte) ([]byte, error)
	// DecryptDEK 解包 EncryptDEK 产物，还原数据密钥。
	DecryptDEK(ctx context.Context, ciphertext []byte) ([]byte, error)
}

// KMSStorer 实现 SecureStorer：本地随机 DEK 加密数据 + DEK 经 KMS 包裹的信封。
// 零值/未配置态（client 为 UnconfiguredClient 或 nil）的 Encrypt/Decrypt 返回
// ErrNotConfigured（fail-closed）。
type KMSStorer struct {
	client KMSClient
}

// 编译期断言：*KMSStorer 满足 SecureStorer（防签名漂移，仿 AESGCMStorer 断言模式）。
var _ SecureStorer = (*KMSStorer)(nil)

// NewKMSStorer 创建 KMSStorer。kms 传 nil 时返回未配置态（client = UnconfiguredClient，
// Encrypt/Decrypt 返回 ErrNotConfigured）。
func NewKMSStorer(kms KMSClient) *KMSStorer {
	if kms == nil {
		kms = UnconfiguredClient{}
	}
	return &KMSStorer{client: kms}
}

// UnconfiguredClient 是默认 KMSClient（nil/未配置）：EncryptDEK/DecryptDEK 返回
// ErrNotConfigured（fail-closed，非占位/panic）。KMSStorer 以它为「未配置」哨兵值：
// 显式注入或 NewKMSStorer(nil) 均进入未配置态。
type UnconfiguredClient struct{}

// 编译期断言：UnconfiguredClient 满足 KMSClient。
var _ KMSClient = UnconfiguredClient{}

// EncryptDEK 返回 ErrNotConfigured（未配置态）。
func (UnconfiguredClient) EncryptDEK(_ context.Context, _ []byte) ([]byte, error) {
	return nil, ErrNotConfigured
}

// DecryptDEK 返回 ErrNotConfigured（未配置态）。
func (UnconfiguredClient) DecryptDEK(_ context.Context, _ []byte) ([]byte, error) {
	return nil, ErrNotConfigured
}

// requireConfigured 校验 storer 处于已配置态。nil 接收者、nil client、或 client 为
// UnconfiguredClient 哨兵（值或指针形态）一律返回 ErrNotConfigured——Encrypt/Decrypt
// 在处理任何密文/信封之前先行 fail-fast，保证未配置语义不被后续解析错误掩盖。
func (s *KMSStorer) requireConfigured() error {
	if s == nil || s.client == nil {
		return ErrNotConfigured
	}
	switch s.client.(type) {
	case UnconfiguredClient, *UnconfiguredClient:
		return ErrNotConfigured
	}
	return nil
}

// Encrypt 密封明文：随机 32B DEK → EncryptWithKey 加密数据 → KMS 包裹 DEK → 拼装自描述
// 信封（magic || len(dekCt) || dekCt || body）。client 为 UnconfiguredClient 时返回
// ErrNotConfigured。
func (s *KMSStorer) Encrypt(plaintext []byte) ([]byte, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	// 随机 DEK（AES-256 数据密钥）。
	dek := make([]byte, 32)
	if _, err := rand.Read(dek); err != nil {
		return nil, fmt.Errorf("kms: 生成 DEK 失败: %w", err)
	}
	// 数据正文用 DEK 加密（复用本包 AES-GCM 信封：nonce || ct）。
	body, err := EncryptWithKey(dek, plaintext)
	if err != nil {
		return nil, fmt.Errorf("kms: 用 DEK 加密数据失败: %w", err)
	}
	// DEK 经 KMS 包裹。
	dekCt, err := s.client.EncryptDEK(context.Background(), dek)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, ErrNotConfigured
		}
		return nil, fmt.Errorf("kms: 包裹 DEK 失败: %w", err)
	}
	if len(dekCt) == 0 {
		// 行为异常但返 nil error 的 KMSClient 会给出空密文：拼装会得到
		// dek_len=0 的永不可解信封。写入端 fail-fast 拒绝（M1）。
		return nil, errors.New("kms: KMS 包裹 DEK 返回空密文（实现异常，拒绝拼装不可解信封）")
	}
	if len(dekCt) > maxDEKLen {
		return nil, fmt.Errorf("kms: DEK 密文过长（%d 字节，长度字段上限 %d）", len(dekCt), maxDEKLen)
	}
	// 拼装信封：magic || 2B BE 长度 || dekCt || body。
	out := make([]byte, 0, magicLen+dekLenFieldLen+len(dekCt)+len(body))
	out = append(out, magic...)
	var lenBuf [dekLenFieldLen]byte
	binary.BigEndian.PutUint16(lenBuf[:], uint16(len(dekCt)))
	out = append(out, lenBuf[:]...)
	out = append(out, dekCt...)
	out = append(out, body...)
	return out, nil
}

// Decrypt 解析自描述信封还原明文：拆出 dek_ciphertext 与 body → KMS 解包 DEK →
// DecryptWithKey 还原明文。任何格式/认证失败返回 error（fail-closed，不 panic）。
func (s *KMSStorer) Decrypt(ciphertext []byte) ([]byte, error) {
	if err := s.requireConfigured(); err != nil {
		return nil, err
	}
	dekCt, body, err := parseEnvelope(ciphertext)
	if err != nil {
		return nil, err
	}
	dek, err := s.client.DecryptDEK(context.Background(), dekCt)
	if err != nil {
		if errors.Is(err, ErrNotConfigured) {
			return nil, ErrNotConfigured
		}
		return nil, fmt.Errorf("kms: 解包 DEK 失败: %w", err)
	}
	pt, err := DecryptWithKey(dek, body)
	if err != nil {
		return nil, fmt.Errorf("kms: 用 DEK 解密数据失败: %w", err)
	}
	return pt, nil
}

// parseEnvelope 拆分自描述信封，返回 (dek_ciphertext, body)。校验 magic 与长度边界，
// 格式错误/越界返回清晰 error（task 2 S-1 教训：坏信封要能定位到格式而非裸 GCM 失败）。
func parseEnvelope(data []byte) ([]byte, []byte, error) {
	if len(data) < magicLen+dekLenFieldLen {
		return nil, nil, fmt.Errorf("kms: 密文过短（%d 字节），非 spkms1 信封", len(data))
	}
	if string(data[:magicLen]) != magic {
		return nil, nil, errors.New("kms: 密文 magic 不匹配，非本格式产物（格式错误或文件损坏）")
	}
	dekLen := int(binary.BigEndian.Uint16(data[magicLen : magicLen+dekLenFieldLen]))
	segStart := magicLen + dekLenFieldLen
	segEnd := segStart + dekLen
	if segEnd > len(data) {
		return nil, nil, fmt.Errorf("kms: 信封长度字段越界（dek_len=%d，实际剩余 %d）", dekLen, len(data)-segStart)
	}
	// body 的最小长度（nonce 12B + GCM tag 16B）由 DecryptWithKey 校验，
	// 这里仅负责按长度字段切分。
	return data[segStart:segEnd], data[segEnd:], nil
}
