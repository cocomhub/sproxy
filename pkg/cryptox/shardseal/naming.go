// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
)

// 命名规则（匿名性收敛）：
//
//	任何 blob 文件名 = {core前9}{rand1}{core中9}{rand2}{core后9}   （总长 35-43）
//	  - core = 三段 9 字符（encA / HMAC组签 / encB）按固定乱序排列表 permute27 打散：
//	    三段 hash 不再以 hex 连续段或 3 字符周期出现，观察者无法凭 hex 密度/周期切分。
//	  - 每段 9 字符 = 48bit 的 base62 编码（完整字符集 fullCharset），使文件名字符分布
//	    与普通随机 base62 串同域——不再有「hex 占比 80%」的统计特征。
//	  - encA/encB：自身加密 blob 的 SHA-256 窗口（offset 0/16），自包含可不解密验证密文完整；
//	  - 中段：HMAC-SHA256(secret, fileHash) 窗口（offset 8）——同文件分片/meta 共享组签，
//	    供无 meta 时盲分组恢复；HMAC 单向，无密钥无法反推明文或做内容存在性探测。
//	  - rand1/rand2：长度 4-8 随机，字符集 randCharset（[A-Za-z0-9] 剔除预留 12 字符）；
//	    目录 meta 注入 'q'、file meta 注入 'z'（两段随机选一、段内随机位置）作类型标记。
//	  - 扫描目录时：含 q → 目录 meta；含 z → file meta；二者皆无 → 分块。
//
// 关键匿名性设计：**仅 rand 段剔除预留字符，hash 段用完整字符集**——q/z/x/v/j/w 在
// hash 段正常出现，观察者无法凭「某字符从未出现」推断存在标记，也无法区分标记与哈希字符。
//
// 三类文件名长度同分布 35-43（9*3 + 4~8*2）。

// nameChars 是每段 hash/组签的字符数（48bit 编码为 9 字符 base62，62^9≈1.35e16 ≈ 53bit，
// 略高于 48bit 下限，满足密文/损坏校验强度）。
const nameChars = 9

// Hash48 返回 blob SHA-256 从 off 起的 6 字节（48bit）的 9 字符 base62 编码
// （命名首/末段通用窗口；错误恒为 nil，仅对齐签名，调用方可安全丢弃）。
func Hash48(blob []byte, off int) (string, error) {
	return hash48(blob, off), nil
}

// reservedMarkChars 是类型标记预留字符集（从随机池剔除，名称中仅 meta 注入用）。
// 出于隐藏需要，用普通小写字母而非 '@'/'_”-”：q=目录meta、z=filemeta 已用，
// x/v/j/w 预留备将来扩展新类型。
const reservedMarkChars = "qzxjvw"

// fullCharset 是完整 [A-Za-z0-9] 字符集（62 字符）。hash 段编码用它——预留标记字符在
// hash 段正常出现，使标记字符的存在不构成可观测信号。
const fullCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// randCharset 是随机段字符集（[A-Za-z0-9] 剔除 6 个预留小写标记字符及其大写形式，
// 62→50）。剔除不变式：随机段永不含标记字符的任何大小写形态——但 hash 段用 fullCharset
// 不受影响，故「标记字符是否出现」无法区分 hash 与 rand。
const randCharset = "ABCDEFGHIKLMNOPRSTUYabcdefghiklmnoprstuy0123456789"

// dirAlnumCharset 是容器目录名字符集 [A-Za-z0-9]（目录名与文件名同域，降低目录/文件分层视觉差异）。
const dirAlnumCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// hash48 返回 SHA-256 从 off 起的 6 字节（48bit），再编码为 nameChars 字符 base62 串。
// 用于文件名首尾密文段与 HMAC 组签（非首段统一截取：encA off=0、encB off=16、组签 off=8）。
func hash48(data []byte, off int) string {
	sum := sha256.Sum256(data)
	return encode62(sum[off : off+6])
}

// encode62 把 6 字节（48bit）大端整数编码为 nameChars 字符 base62（字符集 fullCharset）。
// 62^9 ≈ 1.35e16 ≥ 2^48，无信息损失；输出字符分布与普通随机 base62 串同域。
// 用完整字符集（非剔除集）：标记字符在 hash 段正常出现，避免「某字符从未出现」的指纹。
func encode62(b []byte) string {
	v := new(big.Int).SetBytes(b)
	out := make([]byte, nameChars)
	base := big.NewInt(62)
	mod := new(big.Int)
	for i := nameChars - 1; i >= 0; i-- {
		v.QuoRem(v, base, mod)
		out[i] = fullCharset[mod.Int64()]
	}
	return string(out)
}

// hash16 返回 SHA-256 前 16 字节的 16 位小写 hex。
// 对内存中的字节恒可计算（SHA-256 对任意输入成功），错误恒为 nil；错误返回仅为
// 与导出 Hash16 对齐签名，调用方可安全丢弃（`_, _ :=`，M5 审查：非风险）。
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

// GroupSig 派生分组盲签：HMAC-SHA256(secret, fileHash) 的 48bit 窗口 base62 编码。
// 同一原始文件的全部 blob（分片 + meta）共享，用于无 meta 时按中段分组恢复。
// 与明文内容解耦（HMAC 单向），无 secret 无法反推明文或验证内容存在性。
// **非首段截取（从偏移 8 字节起取 6 字节）**：与密文段 A（offset 0）、B（offset 16）
// 三个 hash 各取不同范围段，避免「都取首段」的规律形态。
func GroupSig(secret, fileHash []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(fileHash)
	sum := mac.Sum(nil)
	return hash48(sum, 8) // 窗口 [8:14]，非首段，base62 编码
}

// randomSegment 生成长度为 n（4-8）的随机段（字符集 randCharset，无预留标记）。
// 用 cryptoRandN（拒绝采样均匀）而非 `%len(randCharset)`——后者对非 2 幂字符集
// 有轻微 modulo bias；命名匿名性非密钥材料可忽略，但既有拒绝采样模式现成统一。
func randomSegment(n int) (string, error) {
	b := make([]byte, n)
	for i := range b {
		b[i] = randCharset[cryptoRandN(int64(len(randCharset)))]
	}
	return string(b), nil
}

// randPairLen 返回两个随机段的长度对 (r1, r2)：先均匀生成总长 8-16，再均等拆半
// （r1 = total/2 下取整，r2 = total - r1）。这样**文件名总长唯一确定随机段边界**：
//
//	L = 27 + total（total ∈ [8,16]）→ r1 = (L-27)/2, r2 = (L-27) - r1
//
// 读写两侧均按此映射定位随机段范围（无需额外元数据），便于解析恢复三段 hash。
func randPairLen() (r1, r2 int) {
	total := 8 + int(cryptoRandN(9)) // 0..8 → 8..16
	return total / 2, total - total/2
}

// randSegBounds 按文件名长度映射两个随机段的范围（读写两侧同参：
// total = L-27 ∈ [8,16]，r1 = total/2 下取整、r2 = total-r1）。
// 长度非法（total∉[8,16]）返回 (0,0,false)。分类（markInRandSeg）与解析（SplitRandSegments）
// 共用此单一映射，杜绝两侧漂移。
func randSegBounds(name string) (r1, r2 int, ok bool) {
	total := len(name) - 27
	if total < 8 || total > 16 {
		return 0, 0, false
	}
	return total / 2, total - total/2, true
}

// SplitRandSegments 按文件名长度映射出两个随机段，返回拼回后的 27 字符 core。
// 映射见 randSegBounds；core = name[0:9] + name[9+r1 : 9+r1+9] + name[18+r1+r2:]。
// 长度非法（total∉[8,16]）报错。
func SplitRandSegments(name string) (core string, err error) {
	r1, r2, ok := randSegBounds(name)
	if !ok {
		return "", fmt.Errorf("shardseal: 文件名长度 %d 非法（core27+rand8~16）", len(name))
	}
	return name[:9] + name[9+r1:18+r1] + name[18+r1+r2:], nil
}

// injectMark 在段内随机一个位置替换为标记字符（长度不变，位置随机防聚集）。
func injectMark(s string, mark byte) string {
	i := int(cryptoRandN(int64(len(s))))
	b := []byte(s)
	b[i] = mark
	return string(b)
}

// permute27 是固定的 27 位乱序表：把三段的 27 字符（a:0-8, b:9-17, c:18-26）按乱序重排，
// 使最终 core 中三段字符交错且**无 3 字符周期**（观察者无法凭周期/密度切分）。
// 读方不需解析（加载靠 ClassifyName 标记），写方同参可重建验证。
var permute27 = [...]int{
	0, 9, 18, 1, 19, 10, 2, 11, 20, 12, 3, 21, 4, 22, 13, 5, 14, 23,
	24, 6, 15, 7, 25, 16, 8, 26, 17,
}

// interleaveCore 把三段 9 字符按 permute27 乱序重排为 27 字符 core。
// 每段必须恰为 nameChars 字符——**长度守卫**：曾有调用方喂 16-hex/12-hex 串，经
// interleaveCore 静默截断成前 9 字符 → meta/dir 名 hash 段仍是 hex，匿名性回归
// （实测 hex 占比 55-89%）。此处长度不符即 panic（fail-fast，杜绝静默截断重演）。
func interleaveCore(a, b, c string) string {
	if len(a) != nameChars || len(b) != nameChars || len(c) != nameChars {
		panic(fmt.Sprintf("shardseal: interleaveCore 段长非法（应各 %d 字符，got %d/%d/%d）",
			nameChars, len(a), len(b), len(c)))
	}
	var out strings.Builder
	out.Grow(27)
	for _, i := range permute27 {
		switch {
		case i < 9:
			out.WriteByte(a[i])
		case i < 18:
			out.WriteByte(b[i-9])
		default:
			out.WriteByte(c[i-18])
		}
	}
	return out.String()
}

// ChunkName 构造分块文件名：三段 9 字符（encA/组签/encB）按 permute27 乱序重排成 27 字符
// core，插入两个随机段（总长 8-16，均等拆半）在固定位置：
//
//	core[0:9] + rand1 + core[9:18] + rand2 + core[18:27]   （总长 35-43）
func ChunkName(encA, group, encB string) string {
	core := interleaveCore(encA, group, encB)
	r1, r2 := randPairLen()
	seg1, _ := randomSegment(r1)
	seg2, _ := randomSegment(r2)
	return core[:9] + seg1 + core[9:18] + seg2 + core[18:]
}

// MetaName 构造 file meta 文件名：同 ChunkName 的交错结构，两个随机段中随机一个注入 'z'。
func MetaName(blobA, group, blobB string) string {
	core := interleaveCore(blobA, group, blobB)
	r1, r2 := randPairLen()
	seg1, _ := randomSegment(r1)
	seg2, _ := randomSegment(r2)
	if cryptoRandN(2) == 0 {
		seg1 = injectMark(seg1, 'z')
	} else {
		seg2 = injectMark(seg2, 'z')
	}
	return core[:9] + seg1 + core[9:18] + seg2 + core[18:]
}

// DirMetaName 构造目录 meta 文件名：同 ChunkName 的交错结构（blobA/目录id/blobB 乱序重排），
// 两个随机段中随机一个注入 'q'。长度与分块/file meta 一致（35-43）。
func DirMetaName(blobA, dirID, blobB string) string {
	core := interleaveCore(blobA, dirID, blobB)
	r1, r2 := randPairLen()
	seg1, _ := randomSegment(r1)
	seg2, _ := randomSegment(r2)
	if cryptoRandN(2) == 0 {
		seg1 = injectMark(seg1, 'q')
	} else {
		seg2 = injectMark(seg2, 'q')
	}
	return core[:9] + seg1 + core[9:18] + seg2 + core[18:]
}

// NameKind 是三文件名类型。
type NameKind int

const (
	KindChunk    NameKind = iota // 无标记
	KindFileMeta                 // rand 段含 z
	KindDirMeta                  // rand 段含 q
)

// markInRandSeg 判断标记字符是否出现在**随机段范围**（按文件名长度映射），而非整个名字。
// hash 段用 fullCharset（q/z 正常出现），故必须只查 rand 段，否则 chunk 的 hash 段
// 天然含 q/z 会被误判为 meta。随机段范围由 randSegBounds 定位（与 SplitRandSegments 同一映射）。
func markInRandSeg(name string, marks string) bool {
	r1, r2, ok := randSegBounds(name)
	if !ok {
		return false
	}
	seg1 := name[9 : 9+r1]
	seg2 := name[18+r1 : 18+r1+r2]
	return strings.ContainsAny(seg1, marks) || strings.ContainsAny(seg2, marks)
}

// ClassifyName 判定类型：只检查随机段范围（长度映射定位）是否含预留标记字符。
// chunk 的 hash 段用 fullCharset 可正常含 q/z——不影响分类。
func ClassifyName(name string) NameKind {
	if markInRandSeg(name, "q") {
		return KindDirMeta
	}
	if markInRandSeg(name, "z") {
		return KindFileMeta
	}
	return KindChunk
}

// IsDirMetaName 报告文件名是否为目录 meta（随机段含 'q'）。
func IsDirMetaName(name string) bool { return markInRandSeg(name, "q") }

// IsMetaName 报告文件名是否为 file meta（随机段含 'z'）。
func IsMetaName(name string) bool { return markInRandSeg(name, "z") }

// RandDirName 生成随机容器目录名（5-30 字符 [A-Za-z0-9]；规避含保留词前缀形式）。
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

// randomLowerAlnum 生成长度为 n 的随机 [A-Za-z0-9] 串。
func randomLowerAlnum(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("shardseal: 目录名随机生成失败: %w", err)
	}
	for i := range b {
		b[i] = dirAlnumCharset[int(b[i])%len(dirAlnumCharset)]
	}
	return string(b), nil
}

// toBase64 编码字节段（供 meta 存 salt 等）。
func toBase64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// sha256Hex64 返回完整 SHA-256（64 小写 hex）。
func sha256Hex64(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
