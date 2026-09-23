// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// encrypted_root_test.go 验证加密卷根（roadmap P2 at-rest 加密残余）：
//  1. 写入 → 读回一致（Open 解密）。
//  2. 密文落盘（裸读是密文，非明文）。
//  3. key 长度错 → 构造错误。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// TestEncryptedRoot_RoundTrip 写明文 → 读回明文（透明加解密）。
func TestEncryptedRoot_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	er, err := NewEncryptedRoot(base, key)
	if err != nil {
		t.Fatal(err)
	}
	defer er.Close()

	if werr := er.WriteFile("user/a.txt", []byte("hello encrypted"), 0o644); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	got, rerr := er.ReadFile("user/a.txt")
	if rerr != nil {
		t.Fatalf("ReadFile: %v", rerr)
	}
	if string(got) != "hello encrypted" {
		t.Fatalf("round-trip = %q", got)
	}
}

// TestEncryptedRoot_CiphertextOnDisk 密文落盘（裸读非明文）。
func TestEncryptedRoot_CiphertextOnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	er, err := NewEncryptedRoot(base, key)
	if err != nil {
		t.Fatal(err)
	}
	defer er.Close()

	if werr := er.WriteFile("user/a.txt", []byte("secret"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "user", "a.txt"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if bytes.Contains(raw, []byte("secret")) {
		t.Fatalf("密文不应含明文")
	}
}

// TestEncryptedRoot_BadKey key 长度错 → 构造错误。
func TestEncryptedRoot_BadKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	base, err := OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	if _, err := NewEncryptedRoot(base, make([]byte, 16)); err == nil {
		t.Fatalf("16B key 应报错")
	}
}

var _ io.Reader
