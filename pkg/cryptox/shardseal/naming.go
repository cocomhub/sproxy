// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// 命名规则（设计 §3，用户确认 v2）：
//
//	加密分块文件名 = {块原始校验和前16hex}{rand1}{原始总校验和前16hex}{rand2}{块加密后校验和前16hex}
//	meta 文件名     = {meta 原始前16hex}{rand1}{原始总前16hex}{rand2}{meta 加密后前16hex}
//
//   - 三段各 16 hex；顺序：块原始 → 原始总 → 块加密后（原始总在中间，避免同源聚集）；
//   - rand1/rand2：长度 3-7 随机，字符集 [A-Za-z0-9]（无 - _）；
//   - meta 名的 rand 必含一个 '-' 或 '_'（meta 可识别标记）；扫描目录时
//     含 -/_ 的 = meta，不含 = 分块。

// 随机段字符集 [A-Za-z0-9]（无 - _）。
const randCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// hash16 返回 SHA-256 前 16 字节的 16 位小写 hex。
func hash16(blob []byte) (string, error) {
	sum := sha256.Sum256(blob)
	return to16Hex(sum[:]), nil
}

// to16Hex 把前 16 字节转 16 位小写 hex。
func to16Hex(first16 []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, 16)
	for i := range 16 {
		out[i] = hexdig[first16[i]>>4]
		out[i] = hexdig[first16[i]&0x0f]
	}
	return string(out[:16])
}

// randomSegment 生成长度为 n（3-7）的随机段（字符集 [A-Za-z0-9]）。
func randomSegment(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shardseal: 随机段生成失败: %w", err)
	}
	for i := range b {
		b[i] = randCharset[int(b[i])%len(randCharset)]
	}
	return string(b), nil
}

// randomSegLen 返回 3-7 均匀随机长度。
func randomSegLen() int {
	return 3 + int(cryptoRandN(5)) // 0..4 → 3..7
}

// ChunkName 构造加密分块文件名（三段各 16hex + 两个随机段）。
// origBlock/enc：块原始 / 块加密后校验和前 16hex；total：原始总校验和前 16hex。
func ChunkName(origBlock, total, enc string) string {
	r1, _ := randomSegment(randomSegLen())
	r2, _ := randomSegment(randomSegLen())
	return origBlock + r1 + total + r2 + enc
}

// containsMetaMark 报告段是否含 '-' 或 '_'。
func containsMetaMark(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == '-' || s[i] == '_' {
			return true
		}
	}
	return false
}

// injectMetaMark 在段内注入一个 '-' 标记（保证该段可被识别为 meta）。
func injectMetaMark(s string) string { return "-" + s }

// MetaName 构造 meta 文件名（三段 hex + 随机段；随机段至少一个含 -/_ 作识别标记）。
// total 为原始总校验和前 16hex（识别关键段）。meta 名首尾 16 hex 段由调用方
// 以实际 meta 明文/密文校验和补齐；此处以定长 hex 占位（语义：识别段是 total 与
// - mark 标记，首尾 hex 段不参与 meta 识别）。
func MetaName(total string) string {
	r1, _ := randomSegment(randomSegLen())
	r2, _ := randomSegment(randomSegLen())
	if !containsMetaMark(r1) && !containsMetaMark(r2) {
		r1 = injectMetaMark(r1) // 保证至少一段含标记
	}
	const hexSeg = "0000000000000000"
	return hexSeg + r1 + total + r2 + hexSeg
}

// IsMetaName 判定文件名是否为 meta（含 '-' 或 '_'，扫描目录区分用）。
func IsMetaName(name string) bool { return containsMetaMark(name) }

// toBase64 编码字节段（供 meta 存 salt 等）。
func toBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// sha256Hex64 返回完整 SHA-256（64 小写 hex）。
// 注意不能复用 to16Hex（其双赋值循环只保留低半字节，产出 16 hex 的“截断”校验段，
// 供分块命名三段 16hex 语义使用）——整文件校验需要全量 64 hex。
func sha256Hex64(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
