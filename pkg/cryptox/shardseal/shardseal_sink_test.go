// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptShardsReaderTo_SinkReceivesChunksAndStaysDecryptable 钉住 sink 形态（
// EncryptShardsReaderTo，块级交错的落点抽象）：
//   - **逐块**回调 sink，顺序与 res.ChunkNames 完全一致（每块 1 次，无重复/无遗漏）；
//   - **不落任何 blob**（分块与 meta 都只经 sink / res.MetaBlob 交付）——调用方因此可以
//     「加密一块 → 上传一块 → 丢弃」，本地峰值 O(1 块)；
//   - 产物与落盘变体**同格式**：把 sink 收到的 blob 落盘后 `DecryptFile` 能还原原文（走真实
//     读路径验证命名/布局/meta 兼容）。
//
// 变异验证：让 sink 分支回落到写 outDir（或跳过 meta 的 MetaBlob 赋值）⇒ 本用例必红。
func TestEncryptShardsReaderTo_SinkReceivesChunksAndStaysDecryptable(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-key-000")
	policy := BlockPolicy{Mode: "random", Min: 64 << 10, Max: 64 << 10}
	payload := bytes.Repeat([]byte("sink-chunk-interleave-"), 12*1024) // 240KiB（4 块）

	var names []string
	blobs := map[string][]byte{}
	res, err := EncryptShardsReaderTo(bytes.NewReader(payload), int64(len(payload)), "f.bin", secret, policy, 0, AlgoV1GCMLow,
		func(name string, blob []byte) error {
			names = append(names, name)
			blobs[name] = append([]byte(nil), blob...)
			return nil
		})
	if err != nil {
		t.Fatalf("EncryptShardsReaderTo: %v", err)
	}
	if len(names) < 2 || len(names) != len(res.ChunkNames) {
		t.Fatalf("sink 回调次数 %d != 分块数 %d（应逐块且无遗漏/重复）", len(names), len(res.ChunkNames))
	}
	for i, want := range res.ChunkNames {
		if names[i] != want {
			t.Fatalf("第 %d 块回调顺序不一致：%s != %s", i, names[i], want)
		}
		if int64(len(blobs[want])) != res.Meta.Chunks[i].EncSize {
			t.Fatalf("第 %d 块密文字节 %d != meta EncSize %d", i, len(blobs[want]), res.Meta.Chunks[i].EncSize)
		}
	}
	if len(res.MetaBlob) == 0 || res.MetaName == "" {
		t.Fatal("sink 形态必须经 res.MetaBlob/MetaName 交付 meta（meta 不落盘）")
	}

	// 把 sink 收到的分块 + meta blob 落盘，经真实读路径还原（验证格式与落盘变体一致）。
	dir := t.TempDir()
	for name, blob := range blobs {
		if werr := os.WriteFile(filepath.Join(dir, name), blob, 0o600); werr != nil {
			t.Fatalf("写分块 %s: %v", name, werr)
		}
	}
	if werr := os.WriteFile(filepath.Join(dir, res.MetaName), res.MetaBlob, 0o600); werr != nil {
		t.Fatalf("写 meta: %v", werr)
	}
	dst := filepath.Join(t.TempDir(), "out.bin")
	if derr := DecryptFile(res.Meta, dir, dst, secret); derr != nil {
		t.Fatalf("sink 产物经真实读路径还原失败: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("还原内容不一致（%d vs %d 字节）", len(got), len(payload))
	}
}

// TestEncryptShardsReaderTo_NilSinkProducesMetaOnly 钉住 sink=nil（只取 meta 信息的调用方）：
// 不报错、不 panic，meta/分块名齐备（无 blob 交付）。
func TestEncryptShardsReaderTo_NilSinkProducesMetaOnly(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("meta-only-"), 8*1024)
	res, err := EncryptShardsReaderTo(bytes.NewReader(payload), int64(len(payload)), "f.bin",
		[]byte("test-secret-key-000"), BlockPolicy{Mode: "random", Min: 16 << 10, Max: 16 << 10}, 0, AlgoV1GCMLow, nil)
	if err != nil {
		t.Fatalf("EncryptShardsReaderTo(nil sink): %v", err)
	}
	if len(res.ChunkNames) == 0 || res.MetaBlob == nil {
		t.Fatal("nil sink 仍须产出分块名与 meta")
	}
}
