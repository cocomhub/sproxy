// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package accesskey

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// masterKeyInfo 是 master key 派生的 HKDF info 段（绑定用途防跨用途复用）。
const masterKeyInfo = "sproxy-masterkey/v1"

// DeriveMasterKey 从口令/密钥材料 + 盐派生静态存储主密钥（HKDF-SHA256，输出 32B）：
//
//	key = HKDF-SHA256(secret=passphrase, salt=salt, info="sproxy-masterkey/v1")
//
// passphrase 任意长（口令或 base64 解码的 32B）；salt 随机 16B（持久化于密文旁/配置）。
// 收归单一事实源——4C KMS 等不得另写派生。当前装配路径（credential_store.encrypt）
// 直接以 32B master key 作 AES key，不调用本函数；口令派生路径（salt 域分离）留作
// 未来 KMS/口令场景的权威实现。
func DeriveMasterKey(passphrase, salt []byte) ([]byte, error) {
	k := make([]byte, 32)
	hk := hkdf.New(sha256.New, passphrase, salt, []byte(masterKeyInfo))
	if _, err := io.ReadFull(hk, k); err != nil {
		return nil, fmt.Errorf("accesskey: derive master key: %w", err)
	}
	return k, nil
}

// MasterKeyFromBase64 把 base64 编码的 32B master key 解码成 key 字节。容忍首尾空白
// （openssl rand -base64 32 输出带尾换行）。解码失败或结果非 32B 返回 error。
func MasterKeyFromBase64(s string) ([]byte, error) {
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("accesskey: master key base64 解码失败: %w", err)
	}
	if len(data) != 32 {
		return nil, fmt.Errorf("accesskey: master key base64 解码后 %d 字节, 必须为 32 字节（AES-256）", len(data))
	}
	return data, nil
}

// LoadMasterKeyFromFile 从文件读取 master key，两种格式都接受：
//
//  1. base64 编码的 32B（常见于 `openssl rand -base64 32` 输出，含尾换行）；
//  2. raw 32B 字节（整文件恰 32 字节；文本写入附带的单个尾换行也容忍）。
//
// 判定顺序：先按 base64 解码，解码成功且为 32B 即采用；否则按 raw 判定。raw 分支为
// **字节级判定，不做全 Unicode TrimSpace**（I-1）：raw 密钥首/尾字节可能是合法空白
// 字节（0x09-0x0D/0x20），TrimSpace 会剥掉真实密钥字节导致误拒。仅容忍文本写入附带的
// 一个尾部换行：文件 33 字节 + 尾 `\n`（或 34 字节 + 尾 `\r\n`）→ 剥后 32 字节；
// 文件恰 32 字节则整文件即密钥（即使末字节碰巧是 0x0a 也不剥）。两者都不满足（含
// 文件缺失）返回 error（fail-fast）。
func LoadMasterKeyFromFile(path string) ([]byte, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("accesskey: 读取 master key 文件 %s 失败: %w", path, err)
	}
	// 先试 base64 格式（权威形态：`openssl rand -base64 32`，文本形态）。TrimSpace 仅
	// 用于 base64 文本（剥首尾空格/tab/换行），不触碰 raw 二进制判定。
	if b64, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content))); derr == nil && len(b64) == 32 {
		return b64, nil
	}
	// 回落 raw 32B：剥「32 字节之外的附加尾换行」（33B+\n 或 34B+\r\n），恰 32B 不剥。
	raw := content
	if n := len(raw); n == 33 && raw[n-1] == '\n' {
		raw = raw[:n-1]
	} else if n := len(raw); n == 34 && raw[n-1] == '\n' && raw[n-2] == '\r' {
		raw = raw[:n-2]
	}
	if len(raw) == 32 {
		return raw, nil
	}
	return nil, errors.New("accesskey: master key 文件格式非法: 既非 base64 编码的 32B（44 字符含 =），也非 raw 32 字节（整文件 32B，或 32B + 单个尾换行）")
}
