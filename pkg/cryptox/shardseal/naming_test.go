// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"strings"
	"testing"
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
	// 0xAB → 'a','b'；0xCD → 'c','d'；0xEF → 'e','f'；后 8 字节不参与。
	in := []byte{0xAB, 0xCD, 0xEF, 0x12, 0x34, 0x56, 0x78, 0x9A}
	got := to16Hex(in)
	want := "abcdef123456789a"
	if got != want {
		t.Errorf("to16Hex(% x) = %q，期望 %q（高半字节丢失/展开错误）", in, got, want)
	}

	// 全 16 字符必须是真实 hex（非全零占位）。
	for _, c := range got {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("to16Hex 含非 hex 字符 %q", c)
		}
	}
}

func TestRandomSegLen(t *testing.T) {
	t.Parallel()
	seen := map[int]bool{}
	for range 40 {
		n := randomSegLen()
		if n < 3 || n > 7 {
			t.Fatalf("randomSegLen=%d 超出 3-7", n)
		}
		seen[n] = true
	}
	if len(seen) < 3 {
		t.Errorf("randomSegLen 覆盖不全（仅 %d 种长度）", len(seen))
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
		}
	}
}

func TestIsMetaName(t *testing.T) {
	t.Parallel()
	if !IsMetaName("abc-def") {
		t.Error("含 '-' 的应判定为 meta")
	}
	if !IsMetaName("ab_c") {
		t.Error("含 '_' 的应判定为 meta")
	}
	if IsMetaName("abcdef") {
		t.Error("不含 -/_ 不应判定为 meta")
	}
}

func TestChunkNameStructure(t *testing.T) {
	t.Parallel()
	origBlock := strings.Repeat("a", 16)
	total := strings.Repeat("b", 16)
	enc := strings.Repeat("c", 16)
	name := ChunkName(origBlock, total, enc)
	if !strings.HasPrefix(name, origBlock) {
		t.Errorf("分块名前缀应为块原始校验和：%q", name)
	}
	if !strings.Contains(name, total) {
		t.Errorf("分块名应含原始总校验和：%q", name)
	}
	if !strings.HasSuffix(name, enc) {
		t.Errorf("分块名后缀应为块加密后校验和：%q", name)
	}
	if strings.ContainsAny(name, "-_") {
		t.Errorf("分块名不应含 -/_：%q", name)
	}
}

func TestMetaNameStructure(t *testing.T) {
	t.Parallel()
	total := strings.Repeat("b", 16)
	name := MetaName(total)
	// 三段：meta原(16) + rand + 原始总(16) + rand + meta密文(16)，且 rand 必含 - 或 _。
	if !strings.Contains(name, total) {
		t.Errorf("meta 名应含原始总校验和：%q", name)
	}
	if !strings.ContainsAny(name, "-_") {
		t.Errorf("meta 名必须含 -/_ 标记：%q", name)
	}
	if len(name) < 32+2 {
		t.Errorf("meta 名过短：%q", name)
	}
}
