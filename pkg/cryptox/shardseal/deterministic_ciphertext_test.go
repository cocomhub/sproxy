// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本文件锁定收敛加密（确定性密文）的硬验收：
// docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md §6（#1/#3/#4/#5/#6/#7）
// 与 §10（派生绑定卷密钥）。

const convTestSecretA = "volume-key-A-0123456789abcdef"
const convTestSecretB = "volume-key-B-0123456789abcdef"

// readBlobSet 读取 outDir 下给定文件名集合的逐字节内容。
func readBlobSet(t *testing.T, dir string, names []string) map[string][]byte {
	t.Helper()
	m := make(map[string][]byte, len(names))
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			t.Fatalf("读 blob %s 失败: %v", n, err)
		}
		m[n] = b
	}
	return m
}

// TestDeterministicCiphertext_SameContentSameBlobs 硬验收 #1：同一文件（同卷密钥）两次
// 加密 ⇒ blob 名集合（含 meta 名）与每个 blob 的密文**逐字节相同**。
func TestDeterministicCiphertext_SameContentSameBlobs(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	// 固定 mtime（EncryptShards 文件变体取 os.Stat 的 mtime；meta 内含它 ⇒ 必须稳定）。
	mt := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	secret := []byte(convTestSecretA)
	policy := testPolicy()
	outA, outB := t.TempDir(), t.TempDir()
	resA, err := EncryptShards(src, outA, secret, policy, 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards A: %v", err)
	}
	resB, err := EncryptShards(src, outB, secret, policy, 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards B: %v", err)
	}
	if len(resA.ChunkNames) != len(resB.ChunkNames) {
		t.Fatalf("分块数不同：%d vs %d", len(resA.ChunkNames), len(resB.ChunkNames))
	}
	for i := range resA.ChunkNames {
		if resA.ChunkNames[i] != resB.ChunkNames[i] {
			t.Fatalf("分块 %d 名不同：%q vs %q", i, resA.ChunkNames[i], resB.ChunkNames[i])
		}
	}
	if resA.MetaName != resB.MetaName {
		t.Fatalf("meta 名不同：%q vs %q", resA.MetaName, resB.MetaName)
	}
	blobsA := readBlobSet(t, outA, append(append([]string(nil), resA.ChunkNames...), resA.MetaName))
	blobsB := readBlobSet(t, outB, append(append([]string(nil), resB.ChunkNames...), resB.MetaName))
	for name, ba := range blobsA {
		if !bytes.Equal(ba, blobsB[name]) {
			t.Fatalf("blob %s 密文不同（长度 %d vs %d）", name, len(ba), len(blobsB[name]))
		}
	}
	// 确定性密文必须仍可正常解密回原内容。
	dst := filepath.Join(t.TempDir(), "restored.bin")
	if err := DecryptFile(resA.Meta, outA, dst, secret); err != nil {
		t.Fatalf("DecryptFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatal("解密内容与原文不一致")
	}
}

// TestDeterministicCiphertext_CrossPathSameChunks 硬验收 #5：同内容 + 不同逻辑路径/文件名 ⇒
// **分块密文与分块名相同**（派生源不含路径/文件名 ⇒ 跨路径去重）。meta 因承载逻辑名而不同属预期。
func TestDeterministicCiphertext_CrossPathSameChunks(t *testing.T) {
	t.Parallel()
	data := make([]byte, 3000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	mt := time.Unix(1700000000, 0)
	dir := t.TempDir()
	p1 := filepath.Join(dir, "a", "one.mp4")
	p2 := filepath.Join(dir, "b", "two.mp4")
	for _, p := range []string{p1, p2} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	secret := []byte(convTestSecretA)
	outA, outB := t.TempDir(), t.TempDir()
	resA, err := EncryptShards(p1, outA, secret, testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards A: %v", err)
	}
	resB, err := EncryptShards(p2, outB, secret, testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards B: %v", err)
	}
	if len(resA.ChunkNames) != len(resB.ChunkNames) {
		t.Fatalf("跨路径分块数不同：%d vs %d", len(resA.ChunkNames), len(resB.ChunkNames))
	}
	for i := range resA.ChunkNames {
		if resA.ChunkNames[i] != resB.ChunkNames[i] {
			t.Fatalf("跨路径分块 %d 名不同：%q vs %q", i, resA.ChunkNames[i], resB.ChunkNames[i])
		}
	}
	blobsA := readBlobSet(t, outA, resA.ChunkNames)
	blobsB := readBlobSet(t, outB, resB.ChunkNames)
	for name, ba := range blobsA {
		if !bytes.Equal(ba, blobsB[name]) {
			t.Fatalf("跨路径分块 %s 密文不同", name)
		}
	}
}

// TestDeterministicCiphertext_DifferentVolumeSecretsDiffer 硬验收 #3 + §10 硬要求：同内容 +
// **不同卷密钥** ⇒ 分块名与密文**必不相等**（跨卷不可关联）。
func TestDeterministicCiphertext_DifferentVolumeSecretsDiffer(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	mt := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	outA, outB := t.TempDir(), t.TempDir()
	resA, err := EncryptShards(src, outA, []byte(convTestSecretA), testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards A: %v", err)
	}
	resB, err := EncryptShards(src, outB, []byte(convTestSecretB), testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards B: %v", err)
	}
	// 分块名至少一处不同（整份不同名）。
	if resA.ChunkNames[0] == resB.ChunkNames[0] {
		t.Fatalf("不同卷密钥必须产生不同分块名（跨卷可关联）: %q", resA.ChunkNames[0])
	}
	blobsA := readBlobSet(t, outA, resA.ChunkNames)
	blobsB := readBlobSet(t, outB, resB.ChunkNames)
	for name, ba := range blobsA {
		for _, bb := range blobsB {
			if bytes.Equal(ba, bb) {
				t.Fatalf("不同卷密钥的密文不得相同（blob %s）", name)
			}
		}
	}
}

// TestDeterministicCiphertext_ContentChangeDiffers 硬验收 #4：改 1 字节 ⇒ 分块密文不同。
func TestDeterministicCiphertext_ContentChangeDiffers(t *testing.T) {
	t.Parallel()
	src, data := writeTestFile(t)
	mt := time.Unix(1700000000, 0)
	dir := t.TempDir()
	alt := make([]byte, len(data))
	copy(alt, data)
	alt[len(alt)/2] ^= 0x01                    // 改 1 字节
	altPath := filepath.Join(dir, "video.mp4") // 同名、仅内容不同
	if err := os.WriteFile(altPath, alt, 0o644); err != nil {
		t.Fatalf("write alt: %v", err)
	}
	for _, p := range []string{src, altPath} {
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatalf("Chtimes: %v", err)
		}
	}
	outA, outB := t.TempDir(), t.TempDir()
	resA, err := EncryptShards(src, outA, []byte(convTestSecretA), testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards A: %v", err)
	}
	resB, err := EncryptShards(altPath, outB, []byte(convTestSecretA), testPolicy(), 0, AlgoV2GCM)
	if err != nil {
		t.Fatalf("EncryptShards B: %v", err)
	}
	sameNames := len(resA.ChunkNames) == len(resB.ChunkNames)
	if sameNames {
		for i := range resA.ChunkNames {
			if resA.ChunkNames[i] != resB.ChunkNames[i] {
				sameNames = false
				break
			}
		}
	}
	if sameNames {
		t.Fatal("内容改 1 字节后分块名不应完全相同")
	}
}

// TestDeterministicCiphertext_V1StaysRandomAndReadable 硬验收 #6（向后兼容）：v1 语义不变
// （仍随机 ⇒ 两次加密名不同），且 v1 blob 仍可读回。
func TestDeterministicCiphertext_V1StaysRandomAndReadable(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	mt := time.Unix(1700000000, 0)
	if err := os.Chtimes(src, mt, mt); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	secret := []byte(convTestSecretA)
	outA, outB := t.TempDir(), t.TempDir()
	resA, err := EncryptShards(src, outA, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards A: %v", err)
	}
	resB, err := EncryptShards(src, outB, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShards B: %v", err)
	}
	if resA.ChunkNames[0] == resB.ChunkNames[0] {
		t.Fatal("v1 必须保持随机（两次加密名不同）；出现相同名说明 v1 语义被改")
	}
	dst := filepath.Join(t.TempDir(), "restored.bin")
	if err := DecryptFile(resA.Meta, outA, dst, secret); err != nil {
		t.Fatalf("v1 blob 必须仍可读回: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, want) {
		t.Fatal("v1 解密内容与原文不一致")
	}
}

// countingReaderAt 统计读取字节数（断言 v2 不因取 CID 而多读）。
type countingReaderAt struct {
	r    *bytes.Reader
	read int64
}

func (c *countingReaderAt) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	return n, err
}
func (c *countingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.read += int64(n)
	return n, err
}
func (c *countingReaderAt) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

// TestDeterministicCiphertext_StreamingNoExtraRead 硬验收 #6：流式入口在 v2 下的读取量
// **不得**因取 CID 而增加——CID 复用本路径本就必需的整文件哈希 pass（设计 §6 验收 6）。
func TestDeterministicCiphertext_StreamingNoExtraRead(t *testing.T) {
	t.Parallel()
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	secret := []byte(convTestSecretA)
	measure := func(v AlgoVersion) int64 {
		cr := &countingReaderAt{r: bytes.NewReader(data)}
		if _, err := EncryptShardsReader(cr, int64(len(data)), "v.bin", t.TempDir(), secret, testPolicy(), 0, v); err != nil {
			t.Fatalf("EncryptShardsReader v%d: %v", v, err)
		}
		return cr.read
	}
	readV1 := measure(AlgoV1GCM)
	readV2 := measure(AlgoV2GCM)
	if readV2 != readV1 {
		t.Fatalf("v2 流式读取量 %d 与 v1 %d 不同（v2 不得为取 CID 多读）", readV2, readV1)
	}
	if readV2 < int64(len(data)) || readV2 >= 3*int64(len(data)) {
		t.Fatalf("读取量 %d 不在合理区间 [%d, %d)", readV2, len(data), 3*len(data))
	}
}
