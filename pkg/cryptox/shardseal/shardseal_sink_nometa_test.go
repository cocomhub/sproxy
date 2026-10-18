// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package shardseal

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptShardsReaderToChunks_SkipsMetaKeepsChunks（P2 性能修复，2026-10-19）：
// EncryptShardsReaderToChunks 不加密 meta（MetaBlob/MetaName 留空）——供「meta 归属字段
// （mtime/乐观锁版本）在加密期间才定型」的调用方自行加密一次，消除「先加密一份再重加密」的
// 冗余 AES+JSON。**但分块必须与其它变体同口径**：逐块回调、顺序与 res.ChunkNames 一致、
// 密文字节 == meta EncSize，且调用方按最终 meta 明文加密出的 meta blob 能被真实读路径还原。
//
// 变异验证：让 EncryptShardsReaderToChunks 仍加密 meta（skipMeta 未生效）⇒ MetaBlob 非空 ⇒ 红；
// 让 skipMeta 误伤数据分块（不调 sink）⇒ 分块数不符 ⇒ 红。
func TestEncryptShardsReaderToChunks_SkipsMetaKeepsChunks(t *testing.T) {
	t.Parallel()
	secret := []byte("test-secret-key-000")
	policy := BlockPolicy{Mode: "random", Min: 64 << 10, Max: 64 << 10}
	payload := bytes.Repeat([]byte("no-meta-interleave-"), 12*1024) // 240KiB（4 块）

	var names []string
	blobs := map[string][]byte{}
	res, err := EncryptShardsReaderToChunks(bytes.NewReader(payload), int64(len(payload)), "f.bin", secret, policy,
		AlgoV1GCMLow, func(name string, blob []byte) error {
			names = append(names, name)
			blobs[name] = append([]byte(nil), blob...)
			return nil
		})
	if err != nil {
		t.Fatalf("EncryptShardsReaderToChunks: %v", err)
	}
	if res.MetaBlob != nil || res.MetaName != "" {
		t.Fatalf("skipMeta 形态不得产出 meta blob（MetaBlob=%d 字节 MetaName=%q）⇒ 冗余加密未消除",
			len(res.MetaBlob), res.MetaName)
	}
	if len(names) < 2 || len(names) != len(res.ChunkNames) {
		t.Fatalf("sink 回调 %d 次 != 分块数 %d", len(names), len(res.ChunkNames))
	}
	for i, want := range res.ChunkNames {
		if names[i] != want {
			t.Fatalf("第 %d 块回调顺序不一致：%s != %s", i, names[i], want)
		}
		if int64(len(blobs[want])) != res.Meta.Chunks[i].EncSize {
			t.Fatalf("第 %d 块密文字节 %d != meta EncSize %d", i, len(blobs[want]), res.Meta.Chunks[i].EncSize)
		}
	}

	// 调用方按**最终** meta 明文加密（与 secretdata.encryptMetaBlob 同口径），落盘后经真实读路径还原。
	salt, serr := base64.StdEncoding.DecodeString(res.Meta.Salt)
	if serr != nil {
		t.Fatalf("meta salt: %v", serr)
	}
	key, kerr := DeriveKey(secret, salt, AlgoV1GCMLow)
	if kerr != nil {
		t.Fatalf("DeriveKey: %v", kerr)
	}
	metaJSON, jerr := json.Marshal(res.Meta)
	if jerr != nil {
		t.Fatalf("marshal meta: %v", jerr)
	}
	metaBlob, berr := EncryptMetaJSON(key, salt, metaJSON, 1<<20)
	if berr != nil {
		t.Fatalf("EncryptMetaJSON: %v", berr)
	}
	dir := t.TempDir()
	for name, blob := range blobs {
		if werr := os.WriteFile(filepath.Join(dir, name), blob, 0o600); werr != nil {
			t.Fatalf("写分块 %s: %v", name, werr)
		}
	}
	dst := filepath.Join(t.TempDir(), "out.bin")
	if derr := DecryptFile(res.Meta, dir, dst, secret); derr != nil {
		t.Fatalf("skipMeta 产物经真实读路径还原失败: %v", derr)
	}
	got, rerr := os.ReadFile(dst)
	if rerr != nil {
		t.Fatalf("读还原文件: %v", rerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("还原内容不一致（%d vs %d 字节）", len(got), len(payload))
	}
	_ = metaBlob
}
