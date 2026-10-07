// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity_test

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/integrity"
)

// gcidCompute 在测试内按固定分块扇区独立复算 GCID（sha1(concat(sha1(分块)))），
// 用作断言基准（不依赖被测 ComputeGCID/indexedBlockGCID 内部实现）。
func gcidCompute(data []byte, blockSize int64) string {
	h := sha1.New()
	for off := int64(0); off < int64(len(data)); off += blockSize {
		end := min(off+blockSize, int64(len(data)))
		chunkHash := sha1.Sum(data[off:end])
		h.Write(chunkHash[:])
	}
	return bytesToHex(h.Sum(nil))
}

// bytesToHex 把字节转小写十六进制（避免依赖被测包 hex）。
func bytesToHex(b []byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hexDigits[c>>4]
		out[i*2+1] = hexDigits[c&0x0f]
	}
	return string(out)
}

// TestGCIDSmallFile_256KB：256KB 整数倍文件，256KB 分块应命中候选
// （PikPak 官方实测：小文件 256KB 分块复算命中官方 hash）。
func TestGCIDSmallFile_256KB(t *testing.T) {
	t.Parallel()
	const block = int64(262144) // 256KB
	data := make([]byte, block*4)
	// 填充非平凡内容（全零 sha1 分块亦可，但非全零更能暴露分块切片/拼接 bug）。
	for i := range data {
		data[i] = byte(i*31 + 17)
	}
	want := gcidCompute(data, block)
	got, ok := integrity.RecomputeGCID(data, []int64{block, 524288, 4194304})
	if !ok {
		t.Fatal("256KB 整数倍文件应命中候选分块")
	}
	if got != want {
		t.Fatalf("GCID 不匹配: got=%s want=%s", got, want)
	}
}

// TestGCIDSmallFile_512KB：512KB 整数倍（非 256KB 整数倍）应命中 512KB 候选。
func TestGCIDSmallFile_512KB(t *testing.T) {
	t.Parallel()
	const block = int64(524288) // 512KB
	data := make([]byte, block*3)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	want := gcidCompute(data, block)
	// 512KB 是 256KB 的整数倍 → 512KB×3 亦被 256KB 整除；候选须把 512KB 放前面
	// 才断言命中 512KB（RecomputeGCID 返回首个整除候选）。
	got, ok := integrity.RecomputeGCID(data, []int64{block, 1048576, 4194304})
	if !ok {
		t.Fatal("512KB 整数倍文件应命中 512KB 候选")
	}
	if got != want {
		t.Fatalf("GCID 不匹配: got=%s want=%s", got, want)
	}
}

// TestGCIDLargeFile_Fallback：非任何候选分块的整数倍（带余量）→ 全部候选未命中
// → ok=false（回落 ② 态本地自洽，Review Focus 5）。
func TestGCIDLargeFile_Fallback(t *testing.T) {
	t.Parallel()
	data := make([]byte, 5*1024*1024+1) // 5MB+1：非 256KB/512KB/1MB/2MB/4MB 整数倍
	if _, ok := integrity.RecomputeGCID(data, []int64{262144, 524288, 1048576, 2097152, 4194304}); ok {
		t.Fatal("非候选分块整数倍文件不应命中")
	}
}

// TestRecomputeGCID_EmptyAndTail：空数据 → ok=false；单块恰好等于分块大小（无余量）→ 命中。
func TestRecomputeGCID_EmptyAndTail(t *testing.T) {
	t.Parallel()
	if _, ok := integrity.RecomputeGCID(nil, []int64{262144}); ok {
		t.Fatal("空数据不应命中")
	}
	block := int64(262144)
	data := make([]byte, block)
	for i := range data {
		data[i] = byte(i*13 + 5)
	}
	want := gcidCompute(data, block)
	got, ok := integrity.RecomputeGCID(data, []int64{block})
	if !ok {
		t.Fatal("恰好一块且无余量应命中")
	}
	if got != want {
		t.Fatalf("GCID 不匹配: got=%s want=%s", got, want)
	}
}

// TestComputeGCID_MatchesManual：ComputeGCID 与测试内独立复算一致（256KB 分块）。
func TestComputeGCID_MatchesManual(t *testing.T) {
	t.Parallel()
	const block = int64(262144)
	data := make([]byte, block*2+12345) // 带余量尾块
	for i := range data {
		data[i] = byte((i*11 + 7) & 0xff)
	}
	if got, want := integrity.ComputeGCID(data, block), gcidCompute(data, block); got != want {
		t.Fatalf("ComputeGCID: got=%s want=%s", got, want)
	}
}

// TestGCIDDeterministic：同输入同分块 → 两次结果一致（确定性）。
func TestGCIDDeterministic(t *testing.T) {
	t.Parallel()
	data := make([]byte, 262144*2)
	for i := range data {
		data[i] = byte(i)
	}
	a := integrity.ComputeGCID(data, 262144)
	b := integrity.ComputeGCID(data, 262144)
	if a != b {
		t.Fatalf("GCID 应确定性: a=%s b=%s", a, b)
	}
	if len(a) != 40 {
		t.Fatalf("GCID (sha1) 应为 40 hex，got %d: %s", len(a), a)
	}
}

// TestRecomputeGCID candidates 含 0/负数：应安全跳过（不 panic）。
func TestRecomputeGCID_InvalidCandidates(t *testing.T) {
	t.Parallel()
	data := make([]byte, 0)
	if _, ok := integrity.RecomputeGCID(data, []int64{0, -262144}); ok {
		t.Fatal("非法候选 + 空数据不应命中")
	}
	// 非零数据 + 全非法候选 → 未命中不 panic。
	if _, ok := integrity.RecomputeGCID(bytes.Repeat([]byte{0xab}, 262144), []int64{0, -1, 3}); ok {
		t.Fatal("非法候选不应命中")
	}
}

// TestRecomputeGCIDAll_TailBlock：文件流式权威复算必须计入余量尾块（与内存版
// ComputeGCID 语义一致）——回归：原实现强制 size%bs==0，带尾块文件对全部候选
// 整除失败 → 权威匹配恒失效（真实文件几乎不可能恰为候选整数倍）。
func TestRecomputeGCIDAll_TailBlock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const block = int64(262144)
	data := make([]byte, block*2+12345) // 带余量尾块
	for i := range data {
		data[i] = byte((i*11 + 7) & 0xff)
	}
	path := filepath.Join(dir, "tail.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := integrity.RecomputeGCIDAll(path, []int64{block})
	if err != nil {
		t.Fatalf("RecomputeGCIDAll: %v", err)
	}
	want := gcidCompute(data, block)
	if len(got) != 1 || got[0] != want {
		t.Fatalf("尾块文件应命中（got=%v want=%s）——整除限制会跳过尾块导致权威复算失效", got, want)
	}
}

// TestVerifyGCID 验证通用 GCID 校验入口（全候选比对命中/未命中/无权威）。
func TestVerifyGCID(t *testing.T) {
	payload := bytes.Repeat([]byte("gcid-verify-"), 30000) // 大小保证 256KB 整除
	if len(payload)%262144 != 0 {
		// 调整到整除 256KB
		payload = bytes.Repeat([]byte("gcid-verify-"), 262144/12)
	}
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	target := integrity.ComputeGCID(payload, 262144)
	// 命中
	ok, err := integrity.VerifyGCID(path, integrity.GCIDCandidates, target)
	if err != nil || !ok {
		t.Fatalf("VerifyGCID hit: ok=%v err=%v", ok, err)
	}
	// 未命中
	ok, err = integrity.VerifyGCID(path, integrity.GCIDCandidates, "0000000000000000000000000000000000000000")
	if err != nil || ok {
		t.Fatalf("VerifyGCID miss: ok=%v err=%v", ok, err)
	}
	// 无权威 hash → false 无 err
	ok, err = integrity.VerifyGCID(path, integrity.GCIDCandidates, "")
	if err != nil || ok {
		t.Fatalf("VerifyGCID empty: ok=%v err=%v", ok, err)
	}
}

// TestGCIDOrdered_SizeGuided 验证大小引导候选 + 大→小计算 + 命中轮次统计。
// 构造 300MB 文件（256M~1G 区间推荐 512K/1M），用 1M 分块 GCID 命中 → 应第 2 轮命中。
func TestGCIDOrdered_SizeGuided(t *testing.T) {
	// 300MB 文件（256M~1G 区间）
	size := int64(300 << 20)
	payload := make([]byte, size)
	// 用 1MiB 分块算 GCID 作为"官方 hash"（推荐候选 512K/1M 里 1M 是第二候选）
	block := int64(1 << 20)
	h := sha1.New()
	inner := sha1.New()
	for off := int64(0); off < size; off += block {
		end := off + block
		if end > size {
			end = size
		}
		inner.Reset()
		inner.Write(payload[off:end])
		h.Write(inner.Sum(nil))
	}
	target := hex.EncodeToString(h.Sum(nil))
	path := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	stats := &integrity.GCIDVerifyStats{}
	hitRound, hitBlock, hitGCID, err := integrity.RecomputeGCIDOrdered(path, target, stats)
	if err != nil {
		t.Fatal(err)
	}
	// 用户规则：推荐候选从大到小 → 推荐 [512K, 1M] 降序 [1M, 512K]，1M 首轮命中。
	if hitRound != 1 || hitBlock != block {
		t.Fatalf("hitRound=%d hitBlock=%d, want 1/%d（推荐 1M 首轮命中）", hitRound, hitBlock, block)
	}
	if hitGCID != target {
		t.Fatalf("hitGCID mismatch")
	}
	if stats.FirstHit.Load() != 1 || stats.SecondHit.Load() != 0 {
		t.Fatalf("stats: first=%d second=%d, want first=1 second=0", stats.FirstHit.Load(), stats.SecondHit.Load())
	}
	// 首轮命中：无不一致详情
	if stats.MismatchFirst.Load() != 0 {
		t.Fatalf("mismatch_first=%d, want 0（首轮命中无不一致）", stats.MismatchFirst.Load())
	}
}
