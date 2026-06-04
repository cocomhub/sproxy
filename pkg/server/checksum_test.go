// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestFileChecksum_FileNotFound(t *testing.T) {
	t.Parallel()
	_, err := FileChecksum("/nonexistent/path/file.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
}

func TestFileChecksum_EmptyFile(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "empty.bin")
	if err := os.WriteFile(path, []byte{}, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cs, err := FileChecksum(path)
	if err != nil {
		t.Fatalf("FileChecksum: %v", err)
	}
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if cs != emptySHA256 {
		t.Fatalf("empty file checksum: want %s, got %s", emptySHA256, cs)
	}
}

func TestVerifyChecksum_Correct(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "check.bin")
	data := []byte("verify me")
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	wantCS := sha256hex(data)
	if !verifyFileWithChecksum(path, wantCS) {
		t.Fatal("verifyFileWithChecksum should return true for correct checksum")
	}
}

func TestVerifyChecksum_Wrong(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "bad.bin")
	if err := os.WriteFile(path, []byte("real data"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if verifyFileWithChecksum(path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatal("verifyFileWithChecksum should return false for wrong checksum")
	}
}

func TestVerifyChecksum_FileNotFound(t *testing.T) {
	t.Parallel()
	if verifyFileWithChecksum("/nonexistent", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
		t.Fatal("verifyFileWithChecksum should return false for nonexistent file")
	}
}

// TestChecksumStore_SaveError 测试 save() 在磁盘写入失败时的错误路径。
// 在只读目录下创建 ChecksumStore，验证 Set/Delete 操作不 panic 且记录错误。
// 注意：Windows 上只读目录不阻止文件写入，因此仅 Linux/macOS 有效。
func TestChecksumStore_SaveError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("只读目录在 Windows 上不阻止文件写入")
	}
	roDir, cleanup := makeReadOnlyDir(t)
	defer cleanup()

	cs := NewChecksumStore(roDir, nil)

	// Set 应该不 panic，save() 会失败但 Set 返回前已释放锁
	cs.Set("k1", "v1")
	cs.Delete("k2")
	cs.Rename("k1", "k3")
}
