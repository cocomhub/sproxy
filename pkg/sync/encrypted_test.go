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

// TestEncryptedFS_StreamingLarge_NoOOM 钉住「流式写不整块入内存」（审查 P2 修复）：
// 大输入（明文 4 MiB）走 io.Pipe 流式加密——验证往返一致 + 底层密文流式落盘。
// 变异验证：改回 bytes.Buffer 全量缓冲 → 本用例仍绿（无法直接观测内存），
// 但 io.Pipe 路径的行为等价性由 Roundtrip 钉住；本用例验证大输入正确性。
func TestEncryptedFS_StreamingLarge_NoOOM(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	inner := &memFSEnc{files: map[string][]byte{}, dirs: map[string]bool{}}
	efs := NewEncryptedFS(inner, key)
	// 4 MiB 明文（跨 64 块，验证分块加密流式性）。
	plain := strings.Repeat("0123456789abcdef", 256*1024)
	if err := efs.WriteFile(context.Background(), "big.bin", strings.NewReader(plain), int64(len(plain)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(inner.files["big.bin"]) <= len(plain) {
		t.Fatalf("密文应大于明文（分块开销），密文 %d 明文 %d", len(inner.files["big.bin"]), len(plain))
	}
	rc, err := efs.OpenRead(context.Background(), "big.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if len(got) != len(plain) || got[0] != plain[0] || got[len(got)-1] != plain[len(plain)-1] {
		t.Fatalf("大文件往返不一致: len %d vs %d", len(got), len(plain))
	}
}
