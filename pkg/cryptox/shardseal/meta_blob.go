// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"fmt"
)

// 本文件把底层加密原语对外暴露为最小包装（任务 4 secretdata 布局重构消费）。
// shardseal.Meta 明文结构在本包内定义；secretdata 卷需要自行将文件 meta JSON /
// 目录 meta JSON 加密落盘（带随机填充），并在重启加载时解密重建索引——这要求
// salt/key 派生、统一格式 blob 加解密原语跨包可达。仅新增只读包装，不改既有行为。
//
// 调用方责任：key 由 DeriveKey(secret, salt, v) 派生；blob 内嵌 salt 由 MetaBlobSalt 读取
// （同一格式 [R][4B 密文长][salt][nonce][ct+tag]，salt 位于固定偏移）。

// DeriveKey 用 scrypt 从 secret + salt 派生 AES-256 文件密钥（与分块/逐文件同参）。
// v 指定算法版本（算法域分离：派生输入 = secret || kdfDomain(v)）；v1 域空串 = 既有行为。
func DeriveKey(secret, salt []byte, v AlgoVersion) ([]byte, error) {
	return deriveKey(secret, salt, v)
}

// EncryptMetaJSON 加密 meta/目录 JSON 明文到「整块落盘总长 = padTarget」（0 = 不
// padding；过小目标不裁剪，只往大里扩）。输出统一 [R][4B 密文长][salt][nonce][ct+tag]。
func EncryptMetaJSON(key, salt, metaJSON []byte, padTarget int) ([]byte, error) {
	return encryptMetaJSON(key, salt, metaJSON, padTarget)
}

// DecryptMetaJSON 解密统一格式 meta blob，返回内嵌真实 JSON（含 padding 截取）。
func DecryptMetaJSON(key, blob []byte) ([]byte, error) {
	return decryptMetaJSON(key, blob)
}

// DecryptChunkStandalone 仅凭 secret + 分块 blob 独立解密（不依赖 meta）。
// blob 自描述：salt/nonce 内嵌固定偏移，先读内嵌 salt → 按注册表试各算法版本派生
// 密钥 → AES-256-GCM 解密（版本不明文进 blob，须试派生定位；升序试 → v1 唯一版本
// 时即单次 scrypt）。blob 内嵌 salt 同时作 decryptBlock 的 expectSalt（自一致，恒过
// 内部一致性校验）。meta 中的 chunks（offset/size/sha256）只作索引加速与事后校验。
func DecryptChunkStandalone(secret, blob []byte) ([]byte, error) {
	salt, err := MetaBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, v := range sortedAlgoVersions() {
		key, kerr := deriveKey(secret, salt, v)
		if kerr != nil {
			lastErr = kerr
			continue
		}
		plain, derr := decryptBlock(key, salt, blob)
		if derr == nil {
			return plain, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("shardseal: 未注册任何算法版本")
	}
	return nil, fmt.Errorf("shardseal: 全部算法版本解密失败: %w", lastErr)
}

// DecryptMetaStandalone 仅凭 secret + meta blob 独立解密（secretdata 卷重启加载
// 目录/文件 meta 用，不依赖外部版本信息）。meta.algo_version 在密文内、解密后方可
// 知，故按注册表升序试各版本派生（v1 唯一版本时即单次 scrypt）；全部失败 fail-closed。
func DecryptMetaStandalone(secret, blob []byte) ([]byte, error) {
	salt, err := MetaBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for _, v := range sortedAlgoVersions() {
		key, kerr := deriveKey(secret, salt, v)
		if kerr != nil {
			lastErr = kerr
			continue
		}
		plain, derr := decryptMetaJSON(key, blob)
		if derr == nil {
			return plain, nil
		}
		lastErr = derr
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("shardseal: 未注册任何算法版本")
	}
	return nil, fmt.Errorf("shardseal: 全部算法版本解密失败: %w", lastErr)
}

// MetaBlobSalt 返回统一格式 blob 内嵌的文件级盐（供按 secret 派生 key）。
func MetaBlobSalt(blob []byte) ([]byte, error) {
	salt, _, _, err := parseBlock(blob)
	if err != nil {
		return nil, err
	}
	return salt, nil
}

// Hash16 返回 blob SHA-256 前 16 字节的 16 位小写 hex（命名三段首/末段语义）。
// 对内存中字节恒可计算，错误恒为 nil；错误返回仅为对齐内部签名，调用方可安全
// 丢弃（`_, _ :=`，M5 审查：非风险、恒定 nil 的冗余返回值）。
func Hash16(blob []byte) (string, error) {
	return hash16(blob)
}

// RandSalt 生成 SaltLen 字节加密随机盐（目录 meta / 独立 blob 使用）。
func RandSalt() ([]byte, error) {
	s := make([]byte, SaltLen)
	if _, err := rand.Read(s); err != nil {
		return nil, fmt.Errorf("shardseal: 随机盐失败: %w", err)
	}
	return s, nil
}

// RandN 返回 [0, n) 加密均匀随机 int64（meta pad 目标抖动用）。
func RandN(n int64) int64 { return cryptoRandN(n) }

// RandIDHex 生成 16 位随机小写 hex（目录 meta 的 dir_id）。
func RandIDHex() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shardseal: 随机 ID 失败: %w", err)
	}
	return to16Hex(b), nil
}
