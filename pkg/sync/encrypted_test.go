// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// encrypted_test.go 验证 at-rest 加密卷（roadmap P2 at-rest 加密）：
//  1. EncryptedFS.WriteFile → 底层存密文（不含明文特征）。
//  2. OpenRead → 解密 = 原文（透明）。
//  3. ListDir/Stat/MakeDir/Delete/Rename 转发（零回归）。

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"strings"
	"testing"
)

// memFS 内存 sync.FS（测试）。
type memFSEnc struct {
	files map[string][]byte
	dirs  map[string]bool
}

func (m *memFSEnc) ListDir(ctx context.Context, path string) ([]Entry, error) { return nil, nil }
func (m *memFSEnc) Stat(ctx context.Context, path string) (*Entry, error) {
	if b, ok := m.files[path]; ok {
		return &Entry{Name: path, Size: int64(len(b))}, nil
	}
	return nil, nil
}
func (m *memFSEnc) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	b, ok := m.files[path]
	if !ok {
		return nil, errNotFoundEnc
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memFSEnc) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	b, _ := io.ReadAll(r)
	m.files[path] = b
	return nil
}
func (m *memFSEnc) Rename(ctx context.Context, from, to string) error {
	m.files[to] = m.files[from]
	delete(m.files, from)
	return nil
}
func (m *memFSEnc) Delete(ctx context.Context, path string) error {
	delete(m.files, path)
	return nil
}
func (m *memFSEnc) MakeDir(ctx context.Context, path string) error {
	m.dirs[path] = true
	return nil
}

var errNotFoundEnc = io.EOF

// TestEncryptedFS_Roundtrip 写加密 → 读解密 = 原文。
func TestEncryptedFS_Roundtrip(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	inner := &memFSEnc{files: map[string][]byte{}, dirs: map[string]bool{}}
	efs := NewEncryptedFS(inner, key)
	plain := strings.Repeat("secret data ", 1000)

	if err := efs.WriteFile(context.Background(), "a.txt", strings.NewReader(plain), int64(len(plain)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 底层密文不含明文特征。
	if bytes.Contains(inner.files["a.txt"], []byte("secret data")) {
		t.Fatal("底层应存密文（不含明文）")
	}
	// 解密读回。
	rc, err := efs.OpenRead(context.Background(), "a.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != plain {
		t.Fatalf("解密 != 原文（len %d vs %d）", len(got), len(plain))
	}
}

// TestEncryptedFS_Forward 非加密路径转发。
func TestEncryptedFS_Forward(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	inner := &memFSEnc{files: map[string][]byte{}, dirs: map[string]bool{}}
	efs := NewEncryptedFS(inner, key)
	if err := efs.MakeDir(context.Background(), "d"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if !inner.dirs["d"] {
		t.Fatal("MakeDir 应转发")
	}
	if err := efs.Delete(context.Background(), "d"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}
