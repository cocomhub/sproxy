// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build slow

package shardseal

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 内存峰值量化（slow tag，一次性/回归核对）：
//   go test -tags=slow -run 'TestEncryptShardsReader_PeakMemory' ./pkg/cryptox/shardseal/
//
// 目的：实证「流式加密内存峰值 O(单块上限)」vs「内存变体/回退 O(文件大小)」。
// 用 AlgoV1GCMLow（scrypt low 档 4MiB）压低 KDF 固定开销，使文件大小主导的对比清晰。
// 串行运行（堆峰值采样对并行测试堆扰动敏感）。

// samplePeakWhile 在 fn 执行期间后台采样 HeapAlloc 峰值，返回峰值（字节）。
// base 为先 GC 后的 HeapAlloc 基线；返回 peak-base。
func samplePeakWhile(t *testing.T, fn func()) int64 {
	t.Helper()
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	peak := base.HeapAlloc
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	}()
	fn()
	close(done)
	<-finished
	if peak < base.HeapAlloc {
		return 0
	}
	return int64(peak - base.HeapAlloc)
}

// writeBigFile 向 path 写 size 字节（1MiB 块复用缓冲，避免测试自身驻留全文）。
func writeBigFile(t *testing.T, path string, size int) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	written := 0
	for written < size {
		n := min(len(buf), size-written)
		if _, werr := f.Write(buf[:n]); werr != nil {
			t.Fatal(werr)
		}
		written += n
	}
}

// TestEncryptShardsReader_PeakMemoryBounded 流式加密 64MiB 文件（2MiB 定长块，low 档）
// 堆峰值增量应远小于文件大小（期望 < 32MiB；文件 64MiB）——证明 O(单块) 不 O(文件)。
func TestEncryptShardsReader_PeakMemoryBounded(t *testing.T) {
	// sproxy:serial: 堆峰值实测对全局堆敏感，串行运行避免并行测试扰动
	const fileSize = 64 << 20
	srcPath := filepath.Join(t.TempDir(), "big.bin")
	writeBigFile(t, srcPath, fileSize)
	f, err := os.Open(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	outDir := t.TempDir()
	secret := []byte("stream-peak-secret-key-32bytes")
	policy := BlockPolicy{Mode: "random", Min: 2 << 20, Max: 2 << 20}

	delta := samplePeakWhile(t, func() {
		if _, eerr := EncryptShardsReader(f, fileSize, "big.bin", outDir, secret, policy, 0, AlgoV1GCMLow); eerr != nil {
			t.Fatalf("EncryptShardsReader: %v", eerr)
		}
	})
	t.Logf("流式加密 %d MiB 文件：堆峰值增量 %.1f MiB（块 2 MiB）", fileSize>>20, float64(delta)/(1<<20))
	const streamingBudget = 32 << 20 // 32MiB（远小于 64MiB 文件；含分配器/GC slack）
	if delta > streamingBudget {
		t.Errorf("流式堆峰值增量 %d 超预算 %d（应 O(单块)，疑似退化为整读）", delta, streamingBudget)
	}
}

// TestEncryptShardsReader_FallbackPeakMemoryHoldsFile 对照：纯流回退 io.ReadAll 整读
// 64MiB（基线在文件已存在于内存之后取，delta 即 ReadAll 的整份拷贝）→ 增量 ≥ 40MiB。
func TestEncryptShardsReader_FallbackPeakMemoryHoldsFile(t *testing.T) {
	// sproxy:serial: 堆峰值实测对全局堆敏感，串行运行
	const fileSize = 64 << 20
	data := make([]byte, fileSize)
	for i := range data {
		data[i] = byte(i % 251)
	}
	plain := struct{ io.Reader }{bytes.NewReader(data)}
	outDir := t.TempDir()
	secret := []byte("stream-peak-secret-key-32bytes")
	policy := BlockPolicy{Mode: "random", Min: 2 << 20, Max: 2 << 20}

	delta := samplePeakWhile(t, func() {
		if _, eerr := EncryptShardsReader(plain, fileSize, "big.bin", outDir, secret, policy, 0, AlgoV1GCMLow); eerr != nil {
			t.Fatalf("EncryptShardsReader(plain): %v", eerr)
		}
	})
	t.Logf("回退（io.ReadAll）加密 %d MiB：堆峰值增量 %.1f MiB", fileSize>>20, float64(delta)/(1<<20))
	const fallbackFloor = 40 << 20 // ≥40MiB：证明回退确实整读文件（与流式形成对比）
	if delta < fallbackFloor {
		t.Errorf("回退堆峰值增量 %d 低于 %d（应整读文件）", delta, fallbackFloor)
	}
}

// BenchmarkEncryptShardsReader_Streaming 流式加密吞吐/分配基线。
func BenchmarkEncryptShardsReader_Streaming(b *testing.B) {
	const fileSize = 32 << 20
	srcPath := filepath.Join(b.TempDir(), "big.bin")
	writeBigFileB(b, srcPath, fileSize)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		b.Fatal(err)
	}
	secret := []byte("bench-secret-key-32bytes-abcdef")
	policy := BlockPolicy{Mode: "random", Min: 2 << 20, Max: 2 << 20}
	b.ReportAllocs()
	b.SetBytes(fileSize)
	for b.Loop() {
		outDir := b.TempDir()
		if _, eerr := EncryptShardsReader(bytes.NewReader(data), fileSize, "big.bin", outDir, secret, policy, 0, AlgoV1GCMLow); eerr != nil {
			b.Fatal(eerr)
		}
	}
}

// writeBigFileB 是 writeBigFile 的 *testing.B 变体。
func writeBigFileB(b *testing.B, path string, size int) {
	b.Helper()
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	written := 0
	for written < size {
		n := min(len(buf), size-written)
		if _, werr := f.Write(buf[:n]); werr != nil {
			b.Fatal(werr)
		}
		written += n
	}
}
