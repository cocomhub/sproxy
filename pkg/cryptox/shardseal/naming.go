// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

// 命名规则（设计 §3，用户确认 v2）：
//
//	加密分块文件名 = {块原始校验和前16hex}{rand1}{原始总校验和前16hex}{rand2}{块加密后校验和前16hex}
//	meta 文件名     = {meta 原始前16hex}{rand1}{原始总前16hex}{rand2}{meta 加密后前16hex}
//	目录 meta 文件名= {dirMeta原始前16hex}{rand1}{目录标识前16hex}{rand2}{dirMeta加密后前16hex}
//
//   - 三段各 16 hex；顺序：原始 → 标识 → 加密后（总校验在中间，避免同源聚集）；
//   - rand1/rand2：长度 3-7 随机，字符集 [A-Za-z0-9]（无 - _ @）；
//   - file meta 名的 rand 必含一个 '-' 或 '_'（file meta 可识别标记）；
//   - 目录 meta 名的 rand 必含一个 '@'（目录 meta 可识别标记）；
//   - 扫描目录时：含 @ = 目录 meta，含 -/_ = file meta，不含 = 分块。
//
// 三类文件名长度同分布 54-62（16+3~7+16+3~7+16）。

// 随机段字符集 [A-Za-z0-9]（无 - _ @）。
const randCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// 容器目录名随机字符集 [a-z0-9]（目录名小写，避免与文件名 54-62 混合形态混淆）。
const lowerAlnumCharset = "abcdefghijklmnopqrstuvwxyz0123456789"

// hash16 返回 SHA-256 前 16 字节的 16 位小写 hex。
// 对内存中的字节恒可计算（SHA-256 对任意输入成功），错误恒为 nil；错误返回仅为
// 与导出 Hash16 对齐签名，调用方可安全丢失（`_, _ :=`，M5 审查：非风险）。
func hash16(blob []byte) (string, error) {
	sum := sha256.Sum256(blob)
	return to16Hex(sum[:]), nil
}

// to16Hex 把前 16 字节转 16 位小写 hex（每字节高/低半字节各一个 hex 字符）。
// 注意：不得用「out[i] 两次赋值」写法——那会把高半字节覆盖只剩低4位，
// 有效熵从 128bit 塌缩到 64bit（8 字节低4位 + 8 个 '0'）（2026-10-02 审查发现）。
func to16Hex(first16 []byte) string {
	const hexdig = "0123456789abcdef"
	out := make([]byte, 16)
	for i := range 8 {
		b := first16[i]
		out[2*i] = hexdig[b>>4]
		out[2*i+1] = hexdig[b&0x0f]
	}
	return string(out)
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

// injectMetaMark 在段内注入一个 '-' 标记（保证该段可被识别为 meta）。用首字符
// 替换而非前置，长度不变——否则注入后段长 4-8，使 meta 名总长越界 55-63，
// 破坏「三类文件名长度同分布 54-62」不变量（TestNameLengthUniform 守卫）。
func injectMetaMark(s string) string { return "-" + s[1:] }

// MetaName 构造 file meta 文件名（三段实际校验和真实补齐 + 随机段）。
// metaOrig/total/metaEnc：meta 明文哈希前16 / 原始总校验和前16 / meta 密文哈希
// 前16（完整可靠，不再占位）。随机段至少一个含 '-'/'_' 作 file meta 识别标记。
func MetaName(metaOrig, total, metaEnc string) string {
	r1, _ := randomSegment(randomSegLen())
	r2, _ := randomSegment(randomSegLen())
	// rand 集 [A-Za-z0-9] 不含 -/_，containsMetaMark(r1) 与 (r2) 恒 false ⇒ 原守卫
	// `!containsMetaMark(r1)&&!containsMetaMark(r2)` 恒真，注入分支无条件执行
	// （M1：恒真守卫改为显式直接注入，保证 file meta 名必含识别标记）。
	r1 = injectMetaMark(r1)
	return metaOrig + r1 + total + r2 + metaEnc
}

// IsMetaName 判定文件名是否为 file meta（含 '-' 或 '_'，扫描目录区分用）。
func IsMetaName(name string) bool { return containsMetaMark(name) }

// NameKind 是三文件名类型。
type NameKind int

const (
	KindChunk    NameKind = iota // 无标记
	KindFileMeta                 // 含 - 或 _
	KindDirMeta                  // 含 @
)

// ClassifyName 按标记字符分类文件名（@ > -/_ > 无标记）。
func ClassifyName(name string) NameKind {
	if strings.ContainsRune(name, '@') {
		return KindDirMeta
	}
	if containsMetaMark(name) {
		return KindFileMeta
	}
	return KindChunk
}

// IsDirMetaName 报告是否目录 meta 名（含 @）。
func IsDirMetaName(name string) bool { return strings.ContainsRune(name, '@') }

// injectDirMark 在段内注入一个 '@' 标记（目录 meta 识别标记）。同样用首字符
// 替换保持段长不变（不破坏长度统一不变量）。
func injectDirMark(s string) string { return "@" + s[1:] }

// DirMetaName 构造目录 meta 文件名：{dirOrig}{rand1}{dirID}{rand2}{dirEnc}，
// rand1/rand2 至少一个含 '@'（目录 meta 识别标记）。长度与分块/file meta 一致（54-62）。
func DirMetaName(dirOrig, dirID, dirEnc string) string {
	r1, _ := randomSegment(randomSegLen())
	r2, _ := randomSegment(randomSegLen())
	// rand 集 [A-Za-z0-9] 不含 '@'，r1/r2 恒不含 ⇒ 原守卫恒真，注入分支无条件执行
	// （M1：与 MetaName 同类恒真守卫，显式直接注入保证目录 meta 名必含识别标记）。
	r1 = injectDirMark(r1)
	return dirOrig + r1 + dirID + r2 + dirEnc
}

// RandDirName 生成随机容器目录名（5-30 字符 [a-z0-9]；命中保留词 meta/data/secret
// 或以 secret 开头时重掷，避免与固定 bucket 名混淆）。
func RandDirName() (string, error) {
	for {
		n := 5 + int(cryptoRandN(26)) // 0..25 → 5..30
		name, err := randomLowerAlnum(n)
		if err != nil {
			return "", err
		}
		if name == "meta" || name == "data" || name == "secret" || strings.HasPrefix(name, "secret") {
			continue // 重掷
		}
		return name, nil
	}
}

// randomLowerAlnum 生成长度为 n 的随机小写字母数字串（[a-z0-9]）。
func randomLowerAlnum(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shardseal: 目录名随机生成失败: %w", err)
	}
	for i := range b {
		b[i] = lowerAlnumCharset[int(b[i])%len(lowerAlnumCharset)]
	}
	return string(b), nil
}

// toBase64 编码字节段（供 meta 存 salt 等）。
func toBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// sha256Hex64 返回完整 SHA-256（64 小写 hex）。
// 注意不能复用 to16Hex（其双赋值循环只保留低半字节，产出 16 hex 的“截断”校验段，
// 供分块命名三段 16hex 语义使用）——整文件校验需要全量 64 hex。
func sha256Hex64(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
