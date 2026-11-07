// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// maxBufferGuard 是记录「单次读请求最大缓冲」的 ReaderAt 守卫：任何 Read/ReadAt 请求
// 超过 limit 即记错误。用于确定性证明流式加密**从不整读全文**（只按块/哈希缓冲读）。
type maxBufferGuard struct {
	t     *testing.T
	rd    io.Reader
	ra    io.ReaderAt
	sk    io.Seeker
	limit int
	max   int
}

func (g *maxBufferGuard) Read(p []byte) (int, error) {
	g.check(len(p))
	return g.rd.Read(p)
}

func (g *maxBufferGuard) ReadAt(p []byte, off int64) (int, error) {
	g.check(len(p))
	return g.ra.ReadAt(p, off)
}

func (g *maxBufferGuard) Seek(off int64, whence int) (int64, error) { return g.sk.Seek(off, whence) }

func (g *maxBufferGuard) check(n int) {
	if n > g.max {
		g.max = n
	}
	if n > g.limit {
		g.t.Errorf("流式加密请求缓冲 %d 超过上限 %d（疑似整读全文，破坏 O(块) 内存）", n, g.limit)
	}
}

// TestEncryptShardsReader_NeverReadsWholeFile 确定性守卫：4MiB 输入、512KiB 定长块下，
// 流式加密任何单次读请求都 ≤1MiB（哈希 128KiB + 块 512KiB），**从不整读 4MiB 全文**。
// 若实现退化为 io.ReadAll / 整文件切片（读请求 ≥4MiB）→ 守卫记错误 → 红。
// 该测试快速（内存寻址），无需 slow tag，可在 CI 常规跑，长期防回归。
func TestEncryptShardsReader_NeverReadsWholeFile(t *testing.T) {
	t.Parallel()
	const fileSize = 4 << 20
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	br := bytes.NewReader(data)
	guard := &maxBufferGuard{t: t, rd: br, ra: br, sk: br, limit: 1 << 20}
	outDir := t.TempDir()
	policy := BlockPolicy{Mode: "random", Min: 512 << 10, Max: 512 << 10} // 512KiB 定长块
	res, err := EncryptShardsReader(guard, fileSize, "big.bin", outDir, []byte("secret-32-bytes-key"), policy, 0, AlgoV1GCMLow)
	if err != nil {
		t.Fatalf("EncryptShardsReader: %v", err)
	}
	if len(res.ChunkNames) < 4 {
		t.Fatalf("期望 ≥4 块，got %d", len(res.ChunkNames))
	}
	if guard.max > 1<<20 {
		t.Fatalf("最大单次读请求 %d 超 1MiB 上限", guard.max)
	}
	// 还原一致（证明分块/边界正确，不只是内存守卫）。
	dst := filepath.Join(t.TempDir(), "r.bin")
	if derr := DecryptFile(res.Meta, outDir, dst, []byte("secret-32-bytes-key")); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("还原不一致")
	}
}

// TestEncryptShardsReader_FallbackReadsWholeFile 对照：纯流（无 Seeker/ReaderAt）回退
// io.ReadAll → 单次读请求远大于块上限（证明回退路径确实整读，与流式形成对比）。
func TestEncryptShardsReader_FallbackReadsWholeFile(t *testing.T) {
	t.Parallel()
	const fileSize = 4 << 20
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	br := bytes.NewReader(data)
	// 仅暴露 io.Reader（丢弃 Seeker/ReaderAt）→ 触发回退。
	plain := struct{ io.Reader }{br}
	outDir := t.TempDir()
	policy := BlockPolicy{Mode: "random", Min: 512 << 10, Max: 512 << 10}
	res, err := EncryptShardsReader(plain, fileSize, "big.bin", outDir, []byte("secret-32-bytes-key"), policy, 0, AlgoV1GCMLow)
	if err != nil {
		t.Fatalf("EncryptShardsReader(plain): %v", err)
	}
	if res == nil || len(res.ChunkNames) == 0 {
		t.Fatal("回退路径未产出分块")
	}
}
