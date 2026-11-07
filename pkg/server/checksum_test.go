// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/testutil"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
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
	roDir, cleanup := testutil.MakeReadOnlyDir(t)
	defer cleanup()

	// 卷句柄开在**只读目录的父目录**（OpenRoot 需要可写：它要写卷级 LAYOUT_VERSION），
	// 台账 rel 落在只读子目录内 ⇒ 写入必然失败（保持「save 失败不得 panic」的用例意图）。
	cs := checksum.NewChecksumStore(testRootAt(t, filepath.Dir(roDir)), filepath.Base(roDir)+"/checksums.json", nil)

	// Set 应该不 panic，save() 会失败但 Set 返回前已释放锁
	cs.Set("k1", "v1")
	cs.Delete("k2")
	cs.Rename("k1", "k3")
}

// verifyFileWithChecksum 验证**绝对路径**文件的 SHA-256 checksum 是否匹配（os.Open 版本）。
//
// 本函数原是 pkg/server/list_handler.go 的生产代码，但实测其消费者只有本测试文件——属纯测试
// 辅助，故迁入此处（生产文件不再留无用代码）。它与 `pkg/files` 的
// `verifyFileWithChecksumRoot`（收 storage.Root + 相对路径）是不同入口，不是重复实现：
// 后者按 os.Root 相对语义打开、防穿越，前者直接吃绝对路径（仅测试临时文件使用）。
func verifyFileWithChecksum(filePath, expectedChecksum string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyChecksum(expectedChecksum, f)
}

// TestFileChecksumRoot_EncryptedVolumePlaintext 加密卷上 FileChecksumRoot 必须算**明文**摘要
// （raw Open 会算密文摘要——校验/冲突检测对明文内容失去意义；本修复把两侧统一为 OpenDecrypted）。
func TestFileChecksumRoot_EncryptedVolumePlaintext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	if serr := rt.SetEncryption(bytes.Repeat([]byte{0x21}, 32)); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	if merr := rt.MkdirAll("user", 0o755); merr != nil {
		t.Fatalf("MkdirAll: %v", merr)
	}
	const rel = "user/plain.bin"
	payload := []byte("checksum-me-plaintext")
	w, werr := rootfs.WriteUserVisible(rt, rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatalf("写: %v", werr)
	}
	if _, werr := w.Write(payload); werr != nil {
		t.Fatalf("写: %v", werr)
	}
	_ = w.Close()

	want := hex.EncodeToString(sha256sum(payload))
	got, gerr := FileChecksumRoot(rt, rel)
	if gerr != nil {
		t.Fatalf("FileChecksumRoot: %v", gerr)
	}
	if got != want {
		t.Fatalf("加密卷应算明文摘要: got %s want %s", got, want)
	}
}
