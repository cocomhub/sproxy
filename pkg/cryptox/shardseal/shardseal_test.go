// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testPolicy 返回一个可用于测试的小块策略（默认 1MB 对 2KB 文件不适用）。
func testPolicy() BlockPolicy {
	return BlockPolicy{Mode: "random", Min: 64, Max: 256}
}

// writeTestFile 写一个确定的字节模式文件到临时目录，返回路径与期望内容。
func writeTestFile(t *testing.T) (string, []byte) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "video.mp4")
	var data []byte
	for i := range 5000 { // 5000B，切开多块（min=64）
		data = append(data, byte(i%251))
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写测试文件失败: %v", err)
	}
	return p, data
}

func TestEncryptShards_DecryptRoundtrip(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	outDir := t.TempDir()
	secret := []byte("super-secret-32-bytes")

	res, err := EncryptShards(src, outDir, secret, testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	if res == nil {
		t.Fatal("EncryptShards 返回 nil")
	}
	if len(res.ChunkNames) < 2 {
		t.Fatalf("期望 ≥2 个分块，got %d", len(res.ChunkNames))
	}
	if res.Meta == nil {
		t.Fatal("Meta 为 nil")
	}
	// 分块与 meta 文件确实落到 outDir。
	for _, cn := range res.ChunkNames {
		if _, serr := os.Stat(filepath.Join(outDir, cn)); serr != nil {
			t.Errorf("分块 %q 不存在: %v", cn, serr)
		}
	}
	if _, serr := os.Stat(filepath.Join(outDir, res.MetaName)); serr != nil {
		t.Errorf("meta 文件 %q 不存在: %v", res.MetaName, serr)
	}

	dst := filepath.Join(t.TempDir(), "restored.mp4")
	if derr := DecryptFile(res.Meta, outDir, dst, secret); derr != nil {
		t.Fatalf("DecryptFile: %v", derr)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("读还原文件失败: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("还原内容不一致：len(got)=%d len(want)=%d", len(got), len(want))
	}
}

func TestDecryptFile_WrongSecretFails(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	outDir := t.TempDir()
	res, err := EncryptShards(src, outDir, []byte("right-secret"), testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "x.bin")
	if err := DecryptFile(res.Meta, outDir, dst, []byte("wrong-secret")); err == nil {
		t.Fatal("期望错误密钥解密失败，却成功")
	}
}

func TestMeta_HasFullStat(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	st, err := os.Stat(src)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	o := res.Meta.Original
	if o.Name != "video.mp4" {
		t.Errorf("original.name=%q", o.Name)
	}
	if o.Size != st.Size() {
		t.Errorf("original.size=%d want %d", o.Size, st.Size())
	}
	if res.Meta.Algorithm != AlgorithmName {
		t.Errorf("algorithm=%q", res.Meta.Algorithm)
	}
	if res.Meta.KDF != "scrypt" {
		t.Errorf("kdf=%q", res.Meta.KDF)
	}
	if len(res.ChunkNames) != len(res.Meta.Chunks) {
		t.Errorf("chunk 数与 meta.chunks 数不一致：%d vs %d", len(res.ChunkNames), len(res.Meta.Chunks))
	}
	sum := int64(0)
	for i, cn := range res.Meta.Chunks {
		if cn.Index != i {
			t.Errorf("chunks[%d].index=%d", i, cn.Index)
		}
		if len(cn.OrigSHA256) != 16 {
			t.Errorf("orig_sha256 长度=%d，应为 16", len(cn.OrigSHA256))
		}
		if len(cn.EncSHA256) != 16 {
			t.Errorf("enc_sha256 长度=%d，应为 16", len(cn.EncSHA256))
		}
		if cn.OrigSize <= 0 {
			t.Errorf("chunks[%d].orig_size=%d", i, cn.OrigSize)
		}
		sum += cn.OrigSize
	}
	if sum != st.Size() {
		t.Errorf("分块原始大小合计=%d want %d", sum, st.Size())
	}
	// salt 应 base64 可解码。
	if _, err := base64.StdEncoding.DecodeString(res.Meta.Salt); err != nil {
		t.Errorf("salt 非 base64: %v", err)
	}
}

func TestEncryptShards_NamingConvention(t *testing.T) {
	t.Parallel()
	src, want := writeTestFile(t)
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	total := sha256.Sum256(want)
	totalHex := to16Hex(total[:])
	for _, cn := range res.ChunkNames {
		// 三段 16hex，中段 = 原始总校验和前 16。
		if !strings.Contains(cn, totalHex) {
			t.Errorf("分块名 %q 缺原始总校验和 %q（命名规则三段中间段）", cn, totalHex)
		}
		if strings.ContainsAny(cn, "-_") {
			t.Errorf("分块名 %q 不应含 -/_（仅 meta 可含）", cn)
		}
	}
	// meta 名必含 - 或 _（可识别标记）。
	if !strings.ContainsAny(res.MetaName, "-_") {
		t.Errorf("meta 名 %q 应含 - 或 _", res.MetaName)
	}
	// meta 名也含原始总 16 hex。
	if !strings.Contains(res.MetaName, totalHex) {
		t.Errorf("meta 名 %q 应含原始总校验和段 %q", res.MetaName, totalHex)
	}
}

func TestMetaJSONRoundtrip(t *testing.T) {
	t.Parallel()
	src, _ := writeTestFile(t)
	res, err := EncryptShards(src, t.TempDir(), []byte("secret"), testPolicy())
	if err != nil {
		t.Fatalf("EncryptShards: %v", err)
	}
	data, err := json.Marshal(res.Meta)
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	var back Meta
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if back.Version != res.Meta.Version || back.Algorithm != res.Meta.Algorithm ||
		back.Original.Name != res.Meta.Original.Name || back.Original.Size != res.Meta.Original.Size {
		t.Errorf("roundtrip 不一致: %+v vs %+v", back, res.Meta)
	}
	if len(back.Chunks) != len(res.Meta.Chunks) {
		t.Errorf("chunks 数量 roundtrip 不一致")
	}
}
