// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package compressx

// compressx_test.go 验证压缩算法注册表（roadmap 11.10-⑨ 压缩算法扩展）：
//  1. 三算法（gzip/zstd/brotli）round-trip 一致性（写入 → 读回字节相等）。
//  2. 损坏流解压失败（不吞错误）。
//  3. 非法算法 Parse 报错（不静默回退 gzip）。
//  4. level 边界：非法 level 构造失败。
//
// 变异点：① Parse 静默回退 gzip → 非法参数用例红；② zstd 少 flush → round-trip 红。

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// compressiblePayload 返回可压缩测试数据（重复段，保证压缩产物有意义）。
func compressiblePayload(n int) []byte {
	chunk := []byte("the quick brown fox jumps over the lazy dog 0123456789 ")
	var buf bytes.Buffer
	for buf.Len() < n {
		buf.Write(chunk)
	}
	return buf.Bytes()
}

// TestCompressx_RoundTripAllAlgos 三算法 round-trip 一致性。
func TestCompressx_RoundTripAllAlgos(t *testing.T) {
	t.Parallel()
	payload := compressiblePayload(64 << 10)
	for _, tc := range []struct {
		algo string
	}{
		{algo: "gzip"},
		{algo: "zstd"},
		{algo: "brotli"},
	} {
		t.Run(tc.algo, func(t *testing.T) {
			t.Parallel()
			algo, perr := Parse(tc.algo)
			if perr != nil {
				t.Fatalf("Parse(%q) = %v", tc.algo, perr)
			}
			var compressed bytes.Buffer
			w, werr := NewWriter(algo, &compressed, 0)
			if werr != nil {
				t.Fatalf("NewWriter(%s) = %v", tc.algo, werr)
			}
			if _, werr := w.Write(payload); werr != nil {
				t.Fatalf("Write: %v", werr)
			}
			if cerr := w.Close(); cerr != nil {
				t.Fatalf("Close: %v", cerr)
			}
			if compressed.Len() == 0 {
				t.Fatalf("%s 压缩产物为空", tc.algo)
			}
			if compressed.Len() >= len(payload) {
				t.Fatalf("%s 压缩未生效（%d >= %d）", tc.algo, compressed.Len(), len(payload))
			}
			r, rerr := NewReader(algo, bytes.NewReader(compressed.Bytes()))
			if rerr != nil {
				t.Fatalf("NewReader(%s) = %v", tc.algo, rerr)
			}
			defer r.Close()
			got, rerr2 := io.ReadAll(r)
			if rerr2 != nil {
				t.Fatalf("ReadAll: %v", rerr2)
			}
			if !bytes.Equal(got, payload) {
				t.Fatalf("%s round-trip 不一致: got %d bytes, want %d", tc.algo, len(got), len(payload))
			}
		})
	}
}

// TestCompressx_ParseInvalidAlgo 非法算法报错（不静默回退 gzip）。
func TestCompressx_ParseInvalidAlgo(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "unknown", "zstd1", "GZIP "} {
		if _, err := Parse(s); err == nil {
			t.Fatalf("Parse(%q) 应报错", s)
		}
	}
}

// TestCompressx_NewReaderCorruptStream 损坏流解压失败（不吞错误）。
func TestCompressx_NewReaderCorruptStream(t *testing.T) {
	t.Parallel()
	payload := compressiblePayload(8 << 10)
	for _, tc := range []struct {
		algo string
	}{
		{algo: "gzip"},
		{algo: "zstd"},
		{algo: "brotli"},
	} {
		t.Run(tc.algo, func(t *testing.T) {
			t.Parallel()
			algo, _ := Parse(tc.algo)
			var compressed bytes.Buffer
			w, err := NewWriter(algo, &compressed, 0)
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			if _, err := w.Write(payload); err != nil {
				t.Fatalf("Write: %v", err)
			}
			if err := w.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			data := compressed.Bytes()
			// 截断一半（frame 不完整 → 解压必须报错）。
			truncated := data[:len(data)/2]
			r, rerr := NewReader(algo, bytes.NewReader(truncated))
			if rerr != nil {
				return // 构造即失败也满足「损坏流不吞错误」
			}
			defer r.Close()
			if _, rerr := io.ReadAll(r); rerr == nil {
				t.Fatalf("%s 截断流解压应报错", tc.algo)
			}
		})
	}
}

// TestCompressx_LevelBounds level 边界：非法 level 构造失败。
func TestCompressx_LevelBounds(t *testing.T) {
	t.Parallel()
	payload := compressiblePayload(1024)
	for _, tc := range []struct {
		algo  string
		level int
	}{
		{algo: "gzip", level: 10},   // gzip 合法范围 1-9（含 -1/-2/0 特殊值）
		{algo: "brotli", level: 12}, // brotli 合法范围 1-11
	} {
		t.Run(tc.algo, func(t *testing.T) {
			t.Parallel()
			algo, perr := Parse(tc.algo)
			if perr != nil {
				t.Fatalf("Parse(%q) = %v", tc.algo, perr)
			}
			var buf bytes.Buffer
			w, werr := NewWriter(algo, &buf, tc.level)
			if werr == nil {
				w.Close()
				t.Fatalf("%s level=%d 应构造失败", tc.algo, tc.level)
			}
			// 合法 level 构造成功。
			algoOK, perr2 := Parse(tc.algo)
			if perr2 != nil {
				t.Fatalf("Parse(%q) = %v", tc.algo, perr2)
			}
			var ok bytes.Buffer
			w2, err2 := NewWriter(algoOK, &ok, 3)
			if err2 != nil {
				t.Fatalf("%s level=3 应构造成功: %v", tc.algo, err2)
			}
			if _, werr := w2.Write(payload); werr != nil {
				t.Fatalf("Write: %v", werr)
			}
			if cerr := w2.Close(); cerr != nil {
				t.Fatalf("Close: %v", cerr)
			}
		})
	}
}

// TestCompressx_NewWriterUnknownAlgo 未注册算法构造失败。
func TestCompressx_NewWriterUnknownAlgo(t *testing.T) {
	t.Parallel()
	algo := Algorithm("definitely-not-registered")
	var buf bytes.Buffer
	if _, err := NewWriter(algo, &buf, 0); err == nil {
		t.Fatal("未知算法 NewWriter 应失败")
	}
	if _, err := NewReader(algo, strings.NewReader("x")); err == nil {
		t.Fatal("未知算法 NewReader 应失败")
	}
}
