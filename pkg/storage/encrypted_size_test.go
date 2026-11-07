// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

// encrypted_size_test.go 钉住「加密卷物理占用公式 == 磁盘真值」（2026-10-14 容量口径收口）。
//
// 判据是**逐字节相等**（不是「大约」）：公式若与写入格式漂移（块大小/前缀/tag 变化），本测试必红。
// 变异：去掉头部长度 ⇒ 全用例红；块开销按 0 计 ⇒ 跨块用例红。

// TestEncryptedPhysicalSize_Formula：公式边界（块边界、空文件、负值钳 0）。
func TestEncryptedPhysicalSize_Formula(t *testing.T) {
	t.Parallel()
	overhead := int64(encRootChunkOverhead())
	header := int64(encRootHeaderLen)
	chunk := int64(encRootDefaultChunk)
	if overhead <= 0 || header <= 0 || chunk <= 0 {
		t.Fatalf("常量异常: header=%d chunk=%d overhead=%d", header, chunk, overhead)
	}
	cases := []struct {
		n    int64
		want int64
	}{
		{0, header},
		{-5, header},
		{1, header + 1 + overhead},
		{chunk - 1, header + chunk - 1 + overhead},
		{chunk, header + chunk + overhead},
		{chunk + 1, header + chunk + 1 + 2*overhead},
		{3*chunk + 7, header + 3*chunk + 7 + 4*overhead},
	}
	for _, c := range cases {
		if got := EncryptedPhysicalSize(c.n); got != c.want {
			t.Fatalf("EncryptedPhysicalSize(%d)=%d want %d", c.n, got, c.want)
		}
	}
}

// TestEncryptedPhysicalSize_MatchesDiskTruth：写入真实加密卷后**磁盘字节数 == 公式值**。
func TestEncryptedPhysicalSize_MatchesDiskTruth(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if serr := root.SetEncryption(make([]byte, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	if !root.IsEncrypted() {
		t.Fatal("应报告为加密卷")
	}
	sizes := []int64{0, 1, 4096, encRootDefaultChunk - 1, encRootDefaultChunk, encRootDefaultChunk + 1, 3*encRootDefaultChunk + 7}
	payload := make([]byte, 4*encRootDefaultChunk+16)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for _, n := range sizes {
		rel := "user/f" + itoaTest(n) + ".bin"
		if err := root.MkdirAll("user", 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		f, oerr := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		if oerr != nil {
			t.Fatalf("OpenFile: %v", oerr)
		}
		if n > 0 {
			if _, werr := f.Write(payload[:n]); werr != nil {
				_ = f.Close()
				t.Fatalf("Write(%d): %v", n, werr)
			}
		}
		if cerr := f.Close(); cerr != nil {
			t.Fatalf("Close: %v", cerr)
		}
		// 磁盘真值：物理文件字节数（绕过解密层，直接 Stat 底层路径）。
		phys, serr := os.Stat(filepath.Join(root.AbsPath(), filepath.FromSlash(rel)))
		if serr != nil {
			t.Fatalf("Stat 物理文件: %v", serr)
		}
		if got, want := phys.Size(), EncryptedPhysicalSize(n); got != want {
			t.Fatalf("逻辑 %d 字节 ⇒ 物理 %d，公式给出 %d（公式与写入格式漂移）", n, got, want)
		}
		// 解密读回必须仍是 n 字节（公式不得影响语义）。
		rc, rerr := root.OpenDecrypted(rel)
		if rerr != nil {
			t.Fatalf("OpenDecrypted: %v", rerr)
		}
		got, lerr := io.ReadAll(rc)
		_ = rc.Close()
		if lerr != nil {
			t.Fatalf("ReadAll: %v", lerr)
		}
		if int64(len(got)) != n {
			t.Fatalf("解密读回 %d 字节 want %d", len(got), n)
		}
	}
}

// TestEncryptedPhysicalSize_UnencryptedIdentityNote：非加密卷不适用本函数（公式仅对加密卷成立）——
// 用显式断言钉住「调用方必须按 IsEncrypted 分流」，避免被误用到明文卷。
func TestEncryptedPhysicalSize_UnencryptedIdentityNote(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	if root.IsEncrypted() {
		t.Fatal("默认根不应加密")
	}
	if EncryptedPhysicalSize(1000) == 1000 {
		t.Fatal("加密公式不应等于逻辑长度（否则调用方无从区分是否适用）")
	}
}

// itoaTest 是测试内联十进制转换（避免为测试引入 strconv 依赖噪音）。
func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
