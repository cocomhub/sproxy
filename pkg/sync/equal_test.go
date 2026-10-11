// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// equal_test.go 钉住通用文件比对 Equal/IsSame：校验和路径（Checksums 交集优先 sha256）
// 与流式分段比对（无校验和兜底）；尺寸/内容差异必须检出；两侧 stat 缺失 fail-closed。

import (
	"bytes"
	"context"
	"io"
	"testing"
)

// newFS 建隔离本地 FS。
func newFS(t *testing.T) *LocalFS {
	t.Helper()
	return NewLocalFS(t.TempDir(), nil)
}

// writeFile 写本地文件（经 FS 原子写）。
func writeFile(t *testing.T, fs *LocalFS, rel string, data []byte) {
	t.Helper()
	if err := fs.WriteFile(context.Background(), rel, bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile %s: %v", rel, err)
	}
}

// TestEqual_SameChecksum 两侧同内容（LocalFS Stat 提供 sha256 Checksums）→ 校验和路径 true。
func TestEqual_SameChecksum(t *testing.T) {
	t.Parallel()
	src := newFS(t)
	dst := newFS(t)
	ctx := context.Background()
	data := []byte("same content for checksum compare 内容")
	writeFile(t, src, "a.bin", data)
	writeFile(t, dst, "b.bin", data)
	ok, err := Equal(ctx, src, "a.bin", dst, "b.bin")
	if err != nil {
		t.Fatalf("Equal: %v", err)
	}
	if !ok {
		t.Fatal("同内容经校验和路径应 equal")
	}
}

// TestEqual_DiffContent 内容不同（同尺寸）→ false（校验和路径）。
func TestEqual_DiffContent(t *testing.T) {
	t.Parallel()
	src := newFS(t)
	dst := newFS(t)
	ctx := context.Background()
	writeFile(t, src, "a.bin", []byte("AAAA"))
	writeFile(t, dst, "b.bin", []byte("AAAB"))
	ok, err := Equal(ctx, src, "a.bin", dst, "b.bin")
	if err != nil {
		t.Fatalf("Equal: %v", err)
	}
	if ok {
		t.Fatal("同尺寸不同内容应 not equal")
	}
}

// TestEqual_DiffSize 尺寸不同 → false（先尺寸守卫）。
func TestEqual_DiffSize(t *testing.T) {
	t.Parallel()
	src := newFS(t)
	dst := newFS(t)
	ctx := context.Background()
	writeFile(t, src, "a.bin", []byte("AAAA"))
	writeFile(t, dst, "b.bin", []byte("AAA"))
	ok, err := Equal(ctx, src, "a.bin", dst, "b.bin")
	if err != nil {
		t.Fatalf("Equal: %v", err)
	}
	if ok {
		t.Fatal("尺寸不同应 not equal")
	}
}

// stripChecksumFS 包装 LocalFS 但 Stat/ListDir 返回不带 Checksums 的 Entry——
// 验证 Equal 经流式分段比对仍能正确判定（无校验和兜底）。
type stripChecksumFS struct{ inner *LocalFS }

func (s *stripChecksumFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	es, err := s.inner.ListDir(ctx, path)
	for i := range es {
		es[i].Checksum = ""
		es[i].Checksums = nil
		es[i].ChecksumType = ""
	}
	return es, err
}
func (s *stripChecksumFS) Stat(ctx context.Context, path string) (*Entry, error) {
	e, err := s.inner.Stat(ctx, path)
	if e != nil {
		e.Checksum = ""
		e.Checksums = nil
		e.ChecksumType = ""
	}
	return e, err
}
func (s *stripChecksumFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return s.inner.OpenRead(ctx, path)
}
func (s *stripChecksumFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	return s.inner.WriteFile(ctx, p, r, size, mtime)
}
func (s *stripChecksumFS) Rename(ctx context.Context, f, t string) error {
	return s.inner.Rename(ctx, f, t)
}
func (s *stripChecksumFS) Delete(ctx context.Context, p string) error { return s.inner.Delete(ctx, p) }
func (s *stripChecksumFS) MakeDir(ctx context.Context, p string) error {
	return s.inner.MakeDir(ctx, p)
}

// TestEqual_StreamFallback 无校验和 FS → 流式分段比对路径 true/false（跨 1MiB 分段差异检出）。
func TestEqual_StreamFallback(t *testing.T) {
	t.Parallel()
	src := &stripChecksumFS{inner: newFS(t)}
	dst := &stripChecksumFS{inner: newFS(t)}
	ctx := context.Background()
	big := make([]byte, compareChunkSize*2+100)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := src.WriteFile(ctx, "big.bin", bytes.NewReader(big), int64(len(big)), 0); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := dst.WriteFile(ctx, "big.bin", bytes.NewReader(big), int64(len(big)), 0); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	ok, err := Equal(ctx, src, "big.bin", dst, "big.bin")
	if err != nil {
		t.Fatalf("Equal stream: %v", err)
	}
	if !ok {
		t.Fatal("流式分段比对同内容应 equal")
	}
	bad := make([]byte, len(big))
	copy(bad, big)
	bad[len(big)/2+7] ^= 0xFF // 跨分段边界差异
	if wErr := dst.WriteFile(ctx, "big.bin", bytes.NewReader(bad), int64(len(bad)), 0); wErr != nil {
		t.Fatalf("rewrite dst: %v", wErr)
	}
	ok, err = Equal(ctx, src, "big.bin", dst, "big.bin")
	if err != nil {
		t.Fatalf("Equal stream diff: %v", err)
	}
	if ok {
		t.Fatal("跨分段边界内容差异应检出")
	}
}

// TestEqual_MissingFailClosed 任一侧文件缺失 → 错误（fail-closed，不臆测一致）。
func TestEqual_MissingFailClosed(t *testing.T) {
	t.Parallel()
	src := newFS(t)
	dst := newFS(t)
	ctx := context.Background()
	writeFile(t, src, "a.bin", []byte("x"))
	if _, err := Equal(ctx, src, "a.bin", dst, "missing.bin"); err == nil {
		t.Fatal("目标缺失应 fail-closed 报错")
	}
	if _, err := Equal(ctx, src, "missing.bin", dst, "a.bin"); err == nil {
		t.Fatal("源缺失应 fail-closed 报错")
	}
}

func TestEqual_CrossAlgoSingleField_NoStringCompare(t *testing.T) {
	t.Parallel()
	src := &crossAlgoFS{inner: newFS(t), algo: "sha256", checksum: "src-sha256"}
	dst := &crossAlgoFS{inner: newFS(t), algo: "md5", checksum: "dst-md5"}
	ctx := context.Background()
	content := []byte("same-content-for-cross-algo")
	if err := src.WriteFile(ctx, "a.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("write src: %v", err)
	}
	if err := dst.WriteFile(ctx, "a.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("write dst: %v", err)
	}
	ok, err := Equal(ctx, src, "a.bin", dst, "a.bin")
	if err != nil {
		t.Fatalf("Equal: %v", err)
	}
	if !ok {
		t.Fatal("内容相同但校验和算法不同 → 应回落流式判定一致（不得跨算法字符串直比）")
	}
}

// crossAlgoFS 包装 LocalFS：Stat 只填单字段 Checksum+ChecksumType（不填 Checksums map）
// 且源填 sha256、目标填 md5——模拟"无交集 + 单字段不同算法"（A-MAJOR 修复目标）。
type crossAlgoFS struct {
	inner    *LocalFS
	algo     string
	checksum string
}

func (c *crossAlgoFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return c.inner.ListDir(ctx, path)
}
func (c *crossAlgoFS) Stat(ctx context.Context, path string) (*Entry, error) {
	e, err := c.inner.Stat(ctx, path)
	if e != nil && c.checksum != "" {
		e.Checksum = c.checksum
		e.ChecksumType = c.algo
		e.Checksums = nil // 只单字段（无交集）
	}
	return e, err
}
func (c *crossAlgoFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return c.inner.OpenRead(ctx, path)
}
func (c *crossAlgoFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	return c.inner.WriteFile(ctx, p, r, size, mtime)
}
func (c *crossAlgoFS) Rename(ctx context.Context, f, t string) error {
	return c.inner.Rename(ctx, f, t)
}
func (c *crossAlgoFS) Delete(ctx context.Context, p string) error  { return c.inner.Delete(ctx, p) }
func (c *crossAlgoFS) MakeDir(ctx context.Context, p string) error { return c.inner.MakeDir(ctx, p) }
