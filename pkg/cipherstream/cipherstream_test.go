// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cipherstream

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"
)

func testKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

const fixturePlain = "sproxy-cipherstream-fixture: quick brown fox 0123456789"

// 三个历史实现的产物样本（2026-11-04 由旧代码生成后固化）：证明**已落盘数据仍可读**。
// 每个使用方保留各自的魔数（域分隔），但帧布局必须一致。
const (
	legacyFilesHex = "5350524f58592d4145532d47434d2d76310001000000000053210d9395cbb4e5cd78dc6f169bc6f49a97102df5f45c70f8a74b59e195de0f885ce1236022fe78ed1b6a548d7fe97a7ef24497c83aa8b18315a617eb12c3c9b843ddfdfc093300d524a0f6c9566e0650540684"
	legacySyncHex  = "5350524f58592d46532d454e432d7631000100000000005317083932ed1a5899cf9787e3ca2edf05a6f5c89b29faa9cb9c08e373b01e297efe0eb008e9a02cbfa0cb310343367d8a5696d19fa583076d6880892e75088deadd48fac4486958fc2db6fed9285250ba7ff3f5"
	legacyE2EEHex  = "5350524f58592d4349504845522d763100010000000000530a4d4b95a8a88938d5082e61ca7d68e36693bd1587db62e96b7d55e0242d59c4b1adb7e39ce189f732d31b792f2f043c63bc75ae9ed0ecc75bb8e87d3d3b00fe234398a08a9379bb1f8adf1a053d18423f985e"
)

// TestReader_LegacyFixtures 锁**向后兼容**：改造前生成的密文必须仍能解出。
func TestReader_LegacyFixtures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		hexStr string
		cfg    Config
	}{
		{"files", legacyFilesHex, Config{Magic: []byte("SPROXY-AES-GCM-v1"), ChunkSize: 64 << 10, VerifyChunkSize: true, Label: "cipher"}},
		{"sync", legacySyncHex, Config{Magic: []byte("SPROXY-FS-ENC-v1"), ChunkSize: 64 << 10, Label: "encrypted"}},
		{"e2ee", legacyE2EEHex, Config{Magic: []byte("SPROXY-CIPHER-v1"), ChunkSize: 64 << 10, Label: "e2ee"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			raw, err := hex.DecodeString(c.hexStr)
			if err != nil {
				t.Fatalf("hex: %v", err)
			}
			r, rerr := NewReader(testKey(), bytes.NewReader(raw), c.cfg)
			if rerr != nil {
				t.Fatalf("NewReader: %v", rerr)
			}
			got, rerr := io.ReadAll(r)
			if rerr != nil {
				t.Fatalf("ReadAll: %v", rerr)
			}
			if string(got) != fixturePlain {
				t.Fatalf("解出内容 = %q, want %q（历史密文不可读 = 数据损坏）", got, fixturePlain)
			}
		})
	}
}

func TestRoundTrip_MultiChunk(t *testing.T) {
	t.Parallel()
	cfg := Config{Magic: []byte("TEST-MAGIC"), ChunkSize: 1024, Label: "test"}
	plain := bytes.Repeat([]byte("abcdefgh"), 500) // 4000B → 4 块
	var buf bytes.Buffer
	w, werr := NewWriter(testKey(), &buf, cfg)
	if werr != nil {
		t.Fatalf("NewWriter: %v", werr)
	}
	if _, werr = w.Write(plain); werr != nil {
		t.Fatalf("Write: %v", werr)
	}
	if werr = w.Close(); werr != nil {
		t.Fatalf("Close: %v", werr)
	}
	r, err := NewReader(testKey(), bytes.NewReader(buf.Bytes()), cfg)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("roundtrip 不一致（got %d B, want %d B）", len(got), len(plain))
	}
}

// TestReader_Rejects 覆盖拒绝路径：魔数不符、块大小不符（严格模式）、密文篡改、截断头。
func TestReader_Rejects(t *testing.T) {
	t.Parallel()
	key := testKey()
	cfg := Config{Magic: []byte("MAGIC-A"), ChunkSize: 1024, VerifyChunkSize: true, Label: "test"}
	var buf bytes.Buffer
	w, _ := NewWriter(key, &buf, cfg)
	_, _ = w.Write([]byte("hello"))
	_ = w.Close()
	sealed := buf.Bytes()

	// 魔数不符。
	if _, err := NewReader(key, bytes.NewReader(sealed), Config{Magic: []byte("MAGIC-B"), ChunkSize: 1024, Label: "test"}); err == nil {
		t.Fatal("魔数不符应报错")
	}
	// 严格模式下块大小不符。
	if _, err := NewReader(key, bytes.NewReader(sealed), Config{Magic: []byte("MAGIC-A"), ChunkSize: 2048, VerifyChunkSize: true, Label: "test"}); err == nil {
		t.Fatal("块大小不符（严格模式）应报错")
	}
	// 篡改密文末字节 ⇒ GCM 认证失败。
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xFF
	r, err := NewReader(key, bytes.NewReader(tampered), Config{Magic: []byte("MAGIC-A"), ChunkSize: 1024, Label: "test"})
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if _, rerr := io.ReadAll(r); rerr == nil {
		t.Fatal("密文被篡改应报错")
	}
	// 截断头。
	if _, err := NewReader(key, bytes.NewReader([]byte("MAG")), Config{Magic: []byte("MAGIC-A"), ChunkSize: 1024, Label: "test"}); err == nil {
		t.Fatal("截断头应报错")
	}
	// 错误密钥。
	r2, _ := NewReader([]byte("12345678901234567890123456789012"), bytes.NewReader(sealed), Config{Magic: []byte("MAGIC-A"), ChunkSize: 1024, Label: "test"})
	if _, rerr := io.ReadAll(r2); rerr == nil {
		t.Fatal("错误密钥应报错")
	}
	_ = errors.Is
}
