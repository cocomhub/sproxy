// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
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