// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/scrypt"
)

// share_password.go 是**分享访问口令**的哈希与校验（roadmap：分享密码）。
//
// 为什么之前没有：早期 `docs/api.md` 写了 `password?`，但服务端从未实现（**文档腐化**），
// Web UI 也没有输入框。这里补上真实实现，并把文档对齐。
//
// 存储形态：`scrypt$<N>$<r>$<p>$<base64 salt>$<base64 hash>`（单字段自描述，便于将来调参）。
//   - 每链接随机 16B 盐（go:S2053：禁止常量盐）；
//   - scrypt N=1<<15 / r=8 / p=1（约 32MiB 内存、~50ms）：分享口令通常是可读短语，必须慢哈希；
//   - 校验用 subtle.ConstantTimeCompare（防时序侧信道）；
//   - **明文口令绝不落盘、绝不入审计**（审计只记「有无口令」）。

const (
	sharePWCost    = 1 << 15
	sharePWBlock   = 8
	sharePWParalle = 1
	sharePWSaltLen = 16
	sharePWKeyLen  = 32
)

// HashSharePassword 派生分享口令的哈希串（空口令 → 空串，表示无口令保护）。
func HashSharePassword(plain string) (string, error) {
	if strings.TrimSpace(plain) == "" {
		return "", nil
	}
	salt := make([]byte, sharePWSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("share password: 生成盐失败: %w", err)
	}
	key, err := scrypt.Key([]byte(plain), salt, sharePWCost, sharePWBlock, sharePWParalle, sharePWKeyLen)
	if err != nil {
		return "", fmt.Errorf("share password: 派生失败: %w", err)
	}
	return fmt.Sprintf("scrypt$%d$%d$%d$%s$%s", sharePWCost, sharePWBlock, sharePWParalle,
		base64.StdEncoding.EncodeToString(salt), base64.StdEncoding.EncodeToString(key)), nil
}

// VerifySharePassword 校验口令是否匹配给定哈希串（空哈希串 = 无口令保护，任何输入都通过）。
func VerifySharePassword(stored, plain string) bool {
	if stored == "" {
		return true
	}
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}
	var cost, block, par int
	if _, err := fmt.Sscanf(parts[1]+" "+parts[2]+" "+parts[3], "%d %d %d", &cost, &block, &par); err != nil {
		return false
	}
	salt, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	got, err := scrypt.Key([]byte(plain), salt, cost, block, par, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// passwordFingerprint 是口令的**不可逆短指纹**，仅供审计/排障区分「不同口令」，
// 不可用于校验（不参与 VerifySharePassword）。
func passwordFingerprint(stored string) string {
	if stored == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(stored))
	return base64.RawURLEncoding.EncodeToString(sum[:6])
}
