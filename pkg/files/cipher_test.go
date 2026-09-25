// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// cipher_test.go 验证加密归档（roadmap P2 加密归档插件化）：
//  1. Cipher 注册表（RegisterCipher 按算法名注册）。
//  2. 流式 EncryptWriter/DecryptReader：分块 AES-256-GCM，往返解密 = 原文。
//  3. 密文不可读（不含明文特征）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

// TestCipher_StreamRoundtrip 流式加密 → 解密往返 = 原文。
func TestCipher_StreamRoundtrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	plain := strings.Repeat("hello archive ", 1000) // > 64KiB 触发多块

	var encBuf bytes.Buffer
	ew, werr := NewCipherWriter("aes-256-gcm", key, &encBuf)
	if werr != nil {
		t.Fatalf("NewCipherWriter: %v", werr)
	}
	if _, cerr := io.Copy(ew, strings.NewReader(plain)); cerr != nil {
		t.Fatalf("encrypt copy: %v", cerr)
	}
	if cerr := ew.Close(); cerr != nil {
		t.Fatalf("encrypt close: %v", cerr)
	}
	if encBuf.Len() == 0 {
		t.Fatal("密文不应为空")
	}
	if bytes.Contains(encBuf.Bytes(), []byte("hello archive")) {
		t.Fatal("密文不应含明文特征")
	}

	dr, rerr := NewCipherReader("aes-256-gcm", key, bytes.NewReader(encBuf.Bytes()))
	if rerr != nil {
		t.Fatalf("NewCipherReader: %v", rerr)
	}
	dec, derr := io.ReadAll(dr)
	if derr != nil {
		t.Fatalf("decrypt read: %v", derr)
	}
	if string(dec) != plain {
		t.Fatalf("解密 != 原文（len %d vs %d）", len(dec), len(plain))
	}
}

// TestCipher_RegisterAndLookup 注册表按名查算法。
func TestCipher_RegisterAndLookup(t *testing.T) {
	t.Parallel()
	// 清理注册表（包级全局，测试隔离；-count=2 残留复现见 #601）。
	old := cipherRegistrySnapshot()
	cipherRegistryClear()
	t.Cleanup(func() { cipherRegistryRestore(old) })

	// aes-256-gcm 由 init 预注册——测自定义算法（非冲突）。
	if RegisterCipher("test-cipher", 32<<10) == false {
		t.Fatal("首次注册应 true")
	}
	if RegisterCipher("test-cipher", 32<<10) == true {
		t.Fatal("重复注册应 false")
	}
	cfg, ok := LookupCipher("test-cipher")
	if !ok {
		t.Fatal("已注册算法应可查")
	}
	if cfg.ChunkSize != 32<<10 {
		t.Fatalf("chunk size = %d", cfg.ChunkSize)
	}
}

var _ = context.Background
