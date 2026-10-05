// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestHash16(t *testing.T) {
	t.Parallel()
	h1, err := hash16([]byte("abc"))
	if err != nil {
		t.Fatalf("hash16: %v", err)
	}
	if len(h1) != 16 {
		t.Fatalf("hash16 长度=%d，应为 16", len(h1))
	}
	// 全 hex
	for _, c := range h1 {
		if c < '0' || c > '9' && c < 'a' {
			t.Fatalf("hash16 含非 hex 字符 %q", c)
		}
		if c > 'f' {
			t.Fatalf("hash16 含非 hex 字符 %q", c)
		}
	}
	h2, _ := hash16([]byte("abc"))
	if h1 != h2 {
		t.Errorf("hash16 不确定")
	}
	h3, _ := hash16([]byte("abd"))
	if h3 == h1 {
		t.Errorf("不同输入产生相同 hash16")
	}
}

// TestTo16Hex_FullEntropy 守卫 to16Hex 完整展开每字节高低半字节（回归：旧实现
// 「out[i] 两次赋值」把高半字节覆盖只剩低4位，输出塌缩到每字节低半字节 +
// 且后 8 字符全 '0'）。直接断言字节展开正确。
func TestTo16Hex_FullEntropy(t *testing.T) {
	t.Parallel()
	in := []byte{0xAB, 0xCD, 0xEF, 0x12, 0x34, 0x56, 0x78, 0x9A}
	got := to16Hex(in)
	want := "abcdef123456789a"
	if got != want {
		t.Errorf("to16Hex(% x) = %q，期望 %q（高半字节丢失/展开错误）", in, got, want)
	}
	for _, c := range got {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("to16Hex 含非 hex 字符 %q", c)
		}
	}
}

func TestHash48(t *testing.T) {
	t.Parallel()
	h0 := hash48([]byte("abc"), 0)
	h16 := hash48([]byte("abc"), 16)
	if h0 == h16 {
		t.Errorf("hash48 offset 0/16 不应相同（单分片防重复段）：%q", h0)
	}
	if len(h0) != 9 || len(h16) != 9 {
		t.Errorf("窗口长度应为 9：%d/%d", len(h0), len(h16))
	}
	// 编码字符必须来自 fullCharset（hash 段含预留标记字符是正常的）。
	for _, c := range h0 {
		if strings.IndexByte(fullCharset, byte(c)) < 0 {
			t.Errorf("hash48 输出含字符集外字符 %q", c)
		}
	}
	// 确定性。
	if hash48([]byte("abc"), 0) != h0 {
		t.Errorf("hash48 不确定")
	}
}

func TestEncode62(t *testing.T) {
	t.Parallel()
	got := encode62(make([]byte, 6))
	if len(got) != 9 {
		t.Errorf("encode62 长度=%d，应为 9", len(got))
	}
	for _, c := range got {
		if strings.IndexByte(fullCharset, byte(c)) < 0 {
			t.Errorf("encode62 输出含字符集外字符 %q", c)
		}
	}
	// 不同输入不同输出。
	if encode62([]byte{0, 0, 0, 0, 0, 1}) == encode62([]byte{0, 0, 0, 0, 0, 2}) {
		t.Errorf("encode62 不同输入产生相同输出")
	}
	// 碰撞抽样。
	seen := map[string]bool{}
	for _, b := range [][]byte{
		{0, 0, 0, 0, 0, 0}, {0, 0, 0, 0, 0, 1}, {0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
		{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc}, {0xde, 0xad, 0xbe, 0xef, 0x00, 0x01},
	} {
		e := encode62(b)
		if seen[e] {
			t.Errorf("encode62 碰撞: % x", b)
		}
		seen[e] = true
	}
	// 全 0 → 全 fullCharset[0]。
	z := encode62(make([]byte, 6))
	for _, c := range z {
		if c != rune(fullCharset[0]) {
			t.Errorf("encode62(0) 输出 %q，期望全 %q", z, fullCharset[0])
		}
	}
}

// TestSplitRandSegmentsRoundtrip：文件名长度 → 随机段范围映射的往返校验——
// 对 ChunkName/MetaName/DirMetaName 生成的名称，SplitRandSegments 必须还原出
// 与输入 hash 段完全一致的 core（interleaveCore 逆向由 permute27 唯一性保证，
// 此处锁 core 结构；三段还原属 permute27 内部一致性，由 TestPermute27 锁定）。
func TestSplitRandSegmentsRoundtrip(t *testing.T) {
	t.Parallel()
	for range 500 {
		encA := encode62([]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc})
		group := encode62([]byte{0xde, 0xad, 0xbe, 0xef, 0x00, 0x01})
		encB := encode62([]byte{0xff, 0xee, 0xdd, 0xcc, 0xbb, 0xaa})
		name := ChunkName(encA, group, encB)
		core, err := SplitRandSegments(name)
		if err != nil {
			t.Fatalf("SplitRandSegments: %v", err)
		}
		// core 必须 = interleaveCore(encA, group, encB)（与写侧同参重建一致）。
		want := interleaveCore(encA, group, encB)
		if core != want {
			t.Errorf("往返还原 core 不一致：got=%q want=%q", core, want)
		}
	}
}

// TestRandPairLen：两个随机段长度合计 8-16 且 r1 ≤ r2（均等拆半）。
func TestRandPairLen(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for range 200 {
		r1, r2 := randPairLen()
		total := r1 + r2
		if total < 8 || total > 16 {
			t.Fatalf("randPairLen 总长=%d 超出 8-16", total)
		}
		if r1 != total/2 || r2 != total-total/2 {
			t.Errorf("randPairLen 非均等拆半：r1=%d r2=%d total=%d", r1, r2, total)
		}
		seen[total] = true
	}
	if len(seen) < 8 {
		t.Errorf("randPairLen 覆盖不全（仅 %d 种总长）", len(seen))
	}
}

func TestGroupSig(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret")
	fh := []byte("deadbeefdeadbeef")
	g1 := GroupSig(secret, fh)
	g2 := GroupSig(secret, fh)
	if g1 != g2 {
		t.Errorf("GroupSig 不确定")
	}
	if len(g1) != 9 {
		t.Errorf("GroupSig 长度=%d，应为 9", len(g1))
	}
	g3 := GroupSig(secret, []byte("deadbeefdeadbeefX"))
	if g3 == g1 {
		t.Errorf("不同文件哈希产生相同组签")
	}
	g4 := GroupSig([]byte("other"), fh)
	if g4 == g1 {
		t.Errorf("不同 secret 产生相同组签（组签必须绑定密钥）")
	}
}

func TestRandomSegmentCharSet(t *testing.T) {
	t.Parallel()
	for range 50 {
		seg, err := randomSegment(6)
		if err != nil {
			t.Fatalf("randomSegment: %v", err)
		}
		if len(seg) != 6 {
			t.Fatalf("长度=%d", len(seg))
		}
		for _, c := range seg {
			valid := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !valid {
				t.Fatalf("随机段含非法字符 %q", c)
			}
			// 预留标记字符大小写均不应出现。
			if strings.IndexByte(reservedMarkChars, byte(c)) >= 0 || strings.IndexByte("QZXVJW", byte(c)) >= 0 {
				t.Fatalf("随机段含预留标记字符 %q（破坏分类不变式）", c)
			}
		}
	}
}

func TestIsMetaName(t *testing.T) {
	t.Parallel()
	// 长度映射定位随机段：构造 35 长度（core27 + rand total 8 → r1=4 r2=4）。
	// rand1 在 [9:13]，rand2 在 [22:26]。
	makeName := func(r1, r2 string) string {
		core := strings.Repeat("a", 27)
		return core[:9] + r1 + core[9:18] + r2 + core[18:]
	}
	if !IsMetaName(makeName("abcz", "bcde")) {
		t.Error("rand 段含 z 的应判定为 file meta")
	}
	if IsMetaName(makeName("abcq", "bcde")) {
		t.Error("rand 段含 q 的是目录 meta，不是 file meta")
	}
	if IsMetaName(makeName("abcx", "bcde")) {
		t.Error("rand 段不含 z 不应判定为 file meta")
	}
	// hash 段（core）含 z 不应判定为 meta（fullCharset 正常含 z）。
	hashZ := "azzzzzzzz" + "bbbbbbbbb" + "ccccccccc"
	name := hashZ[:9] + "bcde" + hashZ[9:18] + "fghi" + hashZ[18:]
	if IsMetaName(name) {
		t.Errorf("hash 段含 z 不应判定为 file meta（fullCharset 正常）：%q", name)
	}
}

func TestChunkNameStructure(t *testing.T) {
	t.Parallel()
	encA := strings.Repeat("a", 9)
	encB := strings.Repeat("b", 9)
	group := strings.Repeat("c", 9)
	name := ChunkName(encA, group, encB)
	// 结构：core27（三段 9 字符按 permute27 乱序重排）+ rand(4-8) 插入在 9/18 处。
	// 总长 35-43。
	if len(name) < 35 || len(name) > 43 {
		t.Errorf("分块名长度 %d 超出 35-43", len(name))
	}
	// 三段各 9 字符完整包含（rand 只会增加不会减少）。
	count := func(r rune) int { return strings.Count(name, string(r)) }
	if count('a') < 9 || count('b') < 9 || count('c') < 9 {
		t.Errorf("三段 9 字符未完整包含（a=%d b=%d c=%d）", count('a'), count('b'), count('c'))
	}
	// permute27 乱序打散：core 不得等于任意平凡拼接（encA+group+encB 及其它排列）——
	// 那证明三段实际被交错重排而非顺序拼接（周期/密度指纹的根源）。随机段与 core
	// 边界拼出的任意 6 字符子串是正常随机现象，不构成周期，不做子串级断言。
	core, cerr := SplitRandSegments(name)
	if cerr != nil {
		t.Fatalf("SplitRandSegments: %v", cerr)
	}
	for _, want := range []string{
		encA + group + encB, encA + encB + group, group + encA + encB,
		group + encB + encA, encB + encA + group, encB + group + encA,
	} {
		if core == want {
			t.Errorf("core 等于平凡拼接 %q（permute27 未打散）", want)
		}
	}
	// 注意：hash 段用 fullCharset（含 q/z/x/v/j/w），真实 chunk 名**允许**含 q/z——
	// 分类只查随机段（markInRandSeg），hash 段含 q/z 不应把 chunk 误判为 meta。
	// （本构造用字面 aaa/bbb/ccc 无 q/z；真实的 q/z 出现由 TestIsMetaName 覆盖。）
	if ClassifyName(name) != KindChunk {
		t.Errorf("分块名分类应恒为 KindChunk：%q -> %v", name, ClassifyName(name))
	}
}

func TestPermute27(t *testing.T) {
	t.Parallel()
	// permute27 必须是 0-26 的全排列。
	seen := make([]bool, 27)
	for _, i := range permute27 {
		if i < 0 || i > 26 || seen[i] {
			t.Fatalf("permute27 非法元素 %d", i)
		}
		seen[i] = true
	}
}

func TestMetaNameStructure(t *testing.T) {
	t.Parallel()
	blobA := strings.Repeat("a", 9)
	group := strings.Repeat("b", 9)
	blobB := strings.Repeat("c", 9)
	name := MetaName(blobA, group, blobB)
	count := func(r rune) int { return strings.Count(name, string(r)) }
	if count('a') < 9 || count('b') < 9 || count('c') < 9 {
		t.Errorf("三段 9 字符未完整包含（a=%d b=%d c=%d）", count('a'), count('b'), count('c'))
	}
	if !strings.ContainsRune(name, 'z') {
		t.Errorf("meta 名必须含 z 标记：%q", name)
	}
	if strings.ContainsAny(name, "@-_") {
		t.Errorf("meta 名不应含特殊符号 @ - _：%q", name)
	}
}

func TestDirMetaNameStructure(t *testing.T) {
	t.Parallel()
	name := DirMetaName("aaaaaaaaa", "bbbbbbbbb", "ccccccccc")
	if !strings.ContainsRune(name, 'q') {
		t.Errorf("目录 meta 名应含 q 标记：%q", name)
	}
	if !IsDirMetaName(name) {
		t.Errorf("IsDirMetaName(%q) 应为 true", name)
	}
	if ClassifyName(name) != KindDirMeta {
		t.Errorf("ClassifyName(%q)=%v，want KindDirMeta", name, ClassifyName(name))
	}
	if strings.ContainsAny(name, "@-_") {
		t.Errorf("目录 meta 名不应含特殊符号 @ - _：%q", name)
	}
}

func TestRandDirName(t *testing.T) {
	t.Parallel()
	seenUpper := false
	for range 200 {
		n, err := RandDirName()
		if err != nil {
			t.Fatalf("RandDirName: %v", err)
		}
		if len(n) < 5 || len(n) > 30 {
			t.Fatalf("目录名长度 %d 超出 5-30", len(n))
		}
		for _, c := range n {
			valid := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
			if !valid {
				t.Fatalf("目录名含非法字符 %q", c)
			}
			if c >= 'A' && c <= 'Z' {
				seenUpper = true
			}
		}
		if n == "meta" || n == "data" || n == "secret" || strings.HasPrefix(n, "secret") {
			t.Fatalf("目录名命中保留词：%q", n)
		}
	}
	if !seenUpper {
		t.Errorf("200 个样本均无大写字母（目录字符集未扩 A-Z）")
	}
}

func TestClassifyName(t *testing.T) {
	t.Parallel()
	// 构造合法长度（35：core27 + r1=4 + r2=4）。
	mk := func(r1, r2 string) string {
		core := strings.Repeat("a", 27)
		return core[:9] + r1 + core[9:18] + r2 + core[18:]
	}
	cases := map[string]NameKind{
		mk("bcde", "fghi"): KindChunk,
		mk("bcdz", "fghi"): KindFileMeta, // rand1 含 z
		mk("bcde", "fghz"): KindFileMeta, // rand2 含 z
		mk("bcdq", "fghi"): KindDirMeta,  // rand1 含 q
		mk("bcde", "fghq"): KindDirMeta,  // rand2 含 q
		mk("bcdz", "fghq"): KindDirMeta,  // 两个标记都在 rand，q 优先
	}
	for name, want := range cases {
		if got := ClassifyName(name); got != want {
			t.Errorf("ClassifyName(%q)=%v，want %v", name, got, want)
		}
	}
}

func TestNameLengthUniform(t *testing.T) {
	t.Parallel()
	// 三类文件名长度同分布 35-43：批量生成，断言三者 min/max 完全一致。
	build := func(fn func() string) (minLen, maxLen int) {
		for range 200 {
			l := len(fn())
			if l < minLen || minLen == 0 {
				minLen = l
			}
			if l > maxLen {
				maxLen = l
			}
		}
		return minLen, maxLen
	}
	a, b, c := "aaaaaaaaa", "bbbbbbbbb", "ccccccccc"
	chunkMin, chunkMax := build(func() string { return ChunkName(a, c, b) }) // (encA, group, encB)
	fmMin, fmMax := build(func() string { return MetaName(a, c, b) })        // (blobA, group, blobB)
	dmMin, dmMax := build(func() string { return DirMetaName(a, b, c) })     // (blobA, dirID, blobB)
	if chunkMin != fmMin || chunkMin != dmMin || chunkMax != fmMax || chunkMax != dmMax {
		t.Errorf("三类文件名长度范围不一致：chunk %d-%d fileMeta %d-%d dirMeta %d-%d",
			chunkMin, chunkMax, fmMin, fmMax, dmMin, dmMax)
	}
	if chunkMin != 35 || chunkMax != 43 {
		t.Errorf("分块名长度 %d-%d，应为 35-43", chunkMin, chunkMax)
	}
}

// TestHexDensityLow：验证新命名 hex 字符占比显著下降（旧版 73-89%，目标 <45%）。
// hex 字符 = [0-9a-f]，base50 中仅 16/50=32%。
// TestHexDensityLow：验证**三类**文件名 hex 字符占比全部显著下降（旧版 chunk 73-89%、
// meta/dir 亦高 hex，目标 <45%）。用真实 hash48/GroupSig 输出（base62）而非字面 hex——
// 锁住「meta/dir 名不再喂 hex 段」的匿名性回归（命名匿名性收敛）。
func TestHexDensityLow(t *testing.T) {
	t.Parallel()
	measure := func(fn func() string) float64 {
		total, hexTotal := 0, 0
		for range 500 {
			name := fn()
			total += len(name)
			for j := 0; j < len(name); j++ {
				ch := name[j]
				if (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') {
					hexTotal++
				}
			}
		}
		return float64(hexTotal) / float64(total)
	}
	secret := []byte("hex-density-secret")
	chunk := func() string {
		blob := []byte("chunk" + fmt.Sprint(time.Now().UnixNano()))
		sum := sha256.Sum256(blob)
		return ChunkName(hash48(blob, 0), GroupSig(secret, sum[:]), hash48(blob, 16))
	}
	meta := func() string {
		blob := []byte("meta" + fmt.Sprint(time.Now().UnixNano()))
		sum := sha256.Sum256(blob)
		return MetaName(hash48(blob, 0), GroupSig(secret, sum[:]), hash48(blob, 16))
	}
	dir := func() string {
		blob := []byte("dir" + fmt.Sprint(time.Now().UnixNano()))
		return DirMetaName(hash48(blob, 0), "AAAAAAAAA", hash48(blob, 16))
	}
	for kind, ratio := range map[string]float64{
		"chunk":     measure(chunk),
		"file meta": measure(meta),
		"dir meta":  measure(dir),
	} {
		if ratio > 0.45 {
			t.Errorf("%s hex 字符占比 %.0f%% 超 45%%（匿名性目标）", kind, ratio*100)
		} else {
			t.Logf("%s hex 占比 %.0f%%", kind, ratio*100)
		}
	}
}
