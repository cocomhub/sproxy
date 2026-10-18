// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"testing"
)

// TestEncryptShardsReaderToChunks_SinkReceivesChunksInOrder 钉住 sink 形态
// （EncryptShardsReaderToChunks，块级交错的落点抽象，secretdata 生产入口）：
//   - **逐块**回调 sink，顺序与 res.ChunkNames 完全一致（每块 1 次，无重复/无遗漏）；
//   - **不落任何 blob**（分块与 meta 都只经 sink / res.MetaBlob 交付）——调用方因此可以
//     「加密一块 → 上传一块 → 丢弃」，本地峰值 O(1 块)；
//   - 产物与落盘变体**同格式**：由 shardseal_sink_nometa_test.go 覆盖（调用方自行加密 meta 后
//     经真实读路径 DecryptFile 还原）。
//
// 变异验证：让 sink 分支回落到写 outDir（或漏调 sink）⇒ 本用例必红。
func TestEncryptShardsReaderToChunks_SinkReceivesChunksInOrder(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-key-000")
	policy := BlockPolicy{Mode: "random", Min: 64 << 10, Max: 64 << 10}
	payload := bytes.Repeat([]byte("sink-chunk-interleave-"), 12*1024) // 240KiB（4 块）

	var names []string
	blobs := map[string][]byte{}
	res, err := EncryptShardsReaderToChunks(bytes.NewReader(payload), int64(len(payload)), "f.bin", secret, policy, AlgoV1GCMLow,
		func(name string, blob []byte) error {
			names = append(names, name)
			blobs[name] = append([]byte(nil), blob...)
			return nil
		})
	if err != nil {
		t.Fatalf("EncryptShardsReaderToChunks: %v", err)
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
	// skipMeta 变体：meta **不由 shardseal 加密**（MetaBlob/MetaName 留空），由调用方按最终
	// meta 明文自行加密落盘（见 shardseal_sink_nometa_test.go）。
	if res.MetaBlob != nil || res.MetaName != "" {
		t.Fatal("ToChunks 变体不得产出 meta blob（skipMeta：由调用方自行加密）")
	}
	if res.Meta == nil || len(res.Meta.Chunks) != len(res.ChunkNames) {
		t.Fatal("仍须返回内存 meta（分块信息齐备）")
	}
}

// TestEncryptShardsReaderToChunks_NilSinkProducesMetaOnly 钉住 sink=nil（只取 meta 信息的调用方）：
// 不报错、不 panic，内存 meta/分块名齐备（无 blob 交付）。
func TestEncryptShardsReaderToChunks_NilSinkProducesMetaOnly(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("meta-only-"), 8*1024)
	res, err := EncryptShardsReaderToChunks(bytes.NewReader(payload), int64(len(payload)), "f.bin",
		[]byte("test-secret-key-000"), BlockPolicy{Mode: "random", Min: 16 << 10, Max: 16 << 10}, AlgoV1GCMLow, nil)
	if err != nil {
		t.Fatalf("EncryptShardsReaderToChunks(nil sink): %v", err)
	}
	if len(res.ChunkNames) == 0 || res.Meta == nil {
		t.Fatal("nil sink 仍须产出分块名与内存 meta")
	}
}
