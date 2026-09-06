// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package otp 提供 RFC 6238 TOTP（基于时间的一次性密码）验证器，纯标准库实现，
// Google Authenticator 兼容（默认算法 SHA1、周期 30s、6 位数字码）。用于账号注册
// TOTP 开启与登录校验（4B-2 credentials 插件）。
package otp

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // G505: RFC 6238 TOTP 强制 SHA1（GA 兼容，无安全替代）
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"net/url"
	"time"
)

const (
	// defaultPeriod 为 TOTP 时间步长（秒），GA 默认 30s。
	defaultPeriod = 30
	// secretLen 为 GenerateSecret 输出的密钥长度（字节），160-bit = GA 兼容。
	secretLen = 20
)

// TOTP 持有一个共享密钥（secret），按 RFC 6238 生成与校验一次性密码。
// 参数固定在 GA 兼容取值：SHA1 / 30s 周期 / 6 位码。
type TOTP struct {
	secret []byte
}

// NewTOTP 基于给定共享密钥创建 TOTP 验证器。
func NewTOTP(secret []byte) *TOTP {
	return &TOTP{secret: secret}
}

// GenerateSecret 生成一个 20 字节（160-bit）的随机共享密钥，由 crypto/rand 填充，
// 与 Google Authenticator 兼容（作为 TOTP 种子使用）。
func GenerateSecret() ([]byte, error) {
	buf := make([]byte, secretLen)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("generate totp secret: %w", err)
	}
	return buf, nil
}

// Code 按 RFC 6238 计算指定时刻的 6 位 TOTP：
//
//   - counter = floor(unix(now)/period)，编码为 8 字节大端；
//   - HMAC-SHA1(secret, counter)；
//   - RFC 4226 动态截断取 31-bit 后 mod 10^6，补零为 6 位。
func (t *TOTP) Code(now time.Time) (string, error) {
	if len(t.secret) == 0 {
		return "", fmt.Errorf("totp: empty secret")
	}
	counter := now.Unix() / defaultPeriod

	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))

	mac := hmac.New(sha1.New, t.secret)
	_, _ = mac.Write(msg[:])
	sum := mac.Sum(nil)

	// RFC 4226 动态截断（Dynamic Truncation）：
	// offset = 摘要末字节低 4 位；取从 offset 起 4 字节，最高位置 0 → 31-bit。
	offset := sum[len(sum)-1] & 0x0f
	binCode := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])

	return fmt.Sprintf("%06d", binCode%1_000_000), nil
}

// Validate 校验 code 是否为 now 所在 step 及前后 window 个 step 内有效的 TOTP
// （默认 window=1 即容差 ±30s，覆盖时钟漂移与传输时延）。窗口足够大时会把相邻
// 周期的一致码都判为有效，由调用方决定是否做重放防护。
func (t *TOTP) Validate(code string, now time.Time, window int) bool {
	if window < 0 {
		window = 0
	}
	cur, err := t.Code(now)
	if err != nil {
		return false
	}
	if constantTimeEqual(code, cur) {
		return true
	}
	for i := 1; i <= window; i++ {
		prev, err := t.Code(now.Add(-time.Duration(i) * defaultPeriod * time.Second))
		if err != nil {
			return false
		}
		next, err := t.Code(now.Add(time.Duration(i) * defaultPeriod * time.Second))
		if err != nil {
			return false
		}
		if constantTimeEqual(code, prev) || constantTimeEqual(code, next) {
			return true
		}
	}
	return false
}

// constantTimeEqual 恒时比较两个等长 ASCII 码字符串，避免时序侧信道泄露匹配位置。
// 长度不同直接失败（TOTP 校验失败不区分"格式错误"与"数值不符"）。
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}

// URI 生成 Google Authenticator 可识别的 otpauth 配置串（otpauth://totp/...）：
// label 经 PathEscape 转义，issuer 经 QueryEscape 转义，secret 用无 padding 的
// base32 编码。参数固定 algorithm=SHA1、digits=6、period=30。
func (t *TOTP) URI(label, issuer string) string {
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(t.secret)
	u := "otpauth://totp/" + url.PathEscape(label) +
		"?secret=" + secret +
		"&issuer=" + url.QueryEscape(issuer) +
		"&algorithm=SHA1&digits=6&period=30"
	return u
}
