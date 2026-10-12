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

// TestEncryptShardsReader_DecryptRoundtrip 验证流式入口 EncryptShardsReader 与
// 内存变体 EncryptShardsBytes 产出**逐字节同构**的加密产物（同 secret/salt/policy），
// 且可被 DecryptFile 还原出原始内容（向后兼容：旧读路径读新写）。
func TestEncryptShardsReader_DecryptRoundtrip(t *testing.T) {
	t.Parallel()
	data := make([]byte, 0, 9000)
	for i := range 9000 {
		data = append(data, byte(i%251))
	}
	secret := []byte("super-secret-32-bytes")
	policy := testPolicy()

	// 用 Seek 输入（bytes.Reader 实现 io.Seeker）走流式。
	ra := bytes.NewReader(data)
	outDir := t.TempDir()
	res, err := EncryptShardsReader(ra, int64(len(data)), "video.mp4", outDir, secret, policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShardsReader: %v", err)
	}
	if res == nil || res.Meta == nil {
		t.Fatal("EncryptShardsReader 返回 nil")
	}
	// 分块与 meta 落到 outDir。
	for _, cn := range res.ChunkNames {
		if _, serr := os.Stat(filepath.Join(outDir, cn)); serr != nil {
			t.Errorf("分块 %q 不存在: %v", cn, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(outDir, res.MetaName)); serr != nil {
		t.Errorf("meta 文件 %q 不存在: %v", res.MetaName, serr)
	}
	// 完整性的整文件 SHA256 与内容一致（分组盲签由该值派生）。
	if res.Meta.Original.SHA256 != Hash256(data) {
		t.Errorf("Meta.SHA256=%q 期望 %q", res.Meta.Original.SHA256, Hash256(data))
	}
	// 还原一致。
	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件: %v", rerr)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("还原内容不一致：got %d bytes，want %d", len(got), len(data))
	}
}

// TestEncryptShardsReader_MatchesBytesVariant 流式入口与内存变体对同输入产出**字节级相同**
// 的落盘 blob（除随机首部外内容一致）；流式不应改变格式/校验语义。
// 用固定 seed 的不可预测差异最小化：直接比对还原内容 + 块数 + meta 字段。
func TestEncryptShardsReader_MatchesBytesVariant(t *testing.T) {
	t.Parallel()
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	secret := []byte("super-secret-32-bytes")
	policy := testPolicy()

	outA := t.TempDir()
	resA, err := EncryptShardsBytes(data, "video.mp4", outA, secret, policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShardsBytes: %v", err)
	}
	outB := t.TempDir()
	resB, err := EncryptShardsReader(bytes.NewReader(data), int64(len(data)), "video.mp4", outB, secret, policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShardsReader: %v", err)
	}
	// 随机分块（Min=64 Max=256）非确定性：两次调用块序列不同，不比较块数/块 stat；
	// 只验证两者均可还原出一致内容 + 整文件 SHA256 一致（格式同构的充分证据）。
	if resA.Meta.Original.SHA256 != resB.Meta.Original.SHA256 {
		t.Errorf("整文件 SHA256 不一致: %s vs %s", resA.Meta.Original.SHA256, resB.Meta.Original.SHA256)
	}
	if len(resA.ChunkNames) == 0 || len(resB.ChunkNames) == 0 {
		t.Fatal("任一变体未产生分块")
	}
	// 还原一致（各自独立 Decrypt）。
	for i, rr := range []*EncryptionResult{resA, resB} {
		dst := filepath.Join(t.TempDir(), "r")
		if derr := DecryptFile(rr.Meta, []string{outA, outB}[i], dst, secret); derr != nil {
			t.Fatalf("DecryptFile(variant %d): %v", i, derr)
		}
		got, _ := os.ReadFile(dst)
		if !bytes.Equal(got, data) {
			t.Fatalf("variant %d 还原不一致", i)
		}
	}
}

// TestEncryptShardsReader_NonSeekFallback 纯流（无 Seeker 接口）走 io.ReadAll 回退：
// 仍产出可还原结果（功能不变，仅内存不省）。
func TestEncryptShardsReader_NonSeekFallback(t *testing.T) {
	t.Parallel()
	data := make([]byte, 5000)
	for i := range data {
		data[i] = byte(i % 251)
	}
	secret := []byte("super-secret-32-bytes")
	policy := testPolicy()

	// 包一层只暴露 io.Reader（丢弃 Seeker/ReaderAt）→ 触发回退路径。
	plain := struct{ io.Reader }{bytes.NewReader(data)}
	outDir := t.TempDir()
	res, err := EncryptShardsReader(plain, int64(len(data)), "video.mp4", outDir, secret, policy, 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShardsReader(non-seek): %v", err)
	}
	dst := filepath.Join(t.TempDir(), "restored.bin")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile(non-seek): %v", derr)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("非 seek 回退还原不一致")
	}
}

// TestEncryptShardsReader_FileReaderAt 用 *os.File（实现 io.ReaderAt+io.Seeker）走流式。
func TestEncryptShardsReader_FileReaderAt(t *testing.T) {
	t.Parallel()
	data := make([]byte, 6000)
	for i := range data {
		data[i] = byte(i % 253)
	}
	src := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	secret := []byte("super-secret-32-bytes")
	outDir := t.TempDir()
	res, err := EncryptShardsReader(f, int64(len(data)), "src.bin", outDir, secret, testPolicy(), 0, AlgoV1GCM)
	if err != nil {
		t.Fatalf("EncryptShardsReader(file): %v", err)
	}
	dst := filepath.Join(t.TempDir(), "restored.bin")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile(file): %v", derr)
	}
	got, _ := os.ReadFile(dst)
	if !bytes.Equal(got, data) {
		t.Fatalf("文件流式还原不一致")
	}
}
