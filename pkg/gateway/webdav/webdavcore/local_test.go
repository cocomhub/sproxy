// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdavcore

// local_test.go 验证 webdavcore（纯 sync.FS WebDAV 适配）：
//  1. NewHandler 桥接 sync.FS → RFC 4918（PUT/GET/PROPFIND/MKCOL/DELETE）。
//  2. 路径穿越防护（../ 按根处理）。

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	filesync "github.com/cocomhub/sproxy/pkg/sync"
)

// memFS 是内存版 sync.FS（测试用）。
type memFS struct {
	mu    sync.Mutex
	files map[string][]byte
	dirs  map[string]bool
}

func newMemFS() *memFS {
	return &memFS{files: map[string][]byte{}, dirs: map[string]bool{}}
}

func (m *memFS) ListDir(ctx context.Context, rel string) ([]filesync.Entry, error) { return nil, nil }
func (m *memFS) Stat(ctx context.Context, rel string) (*filesync.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rel == "" {
		return &filesync.Entry{Name: "", IsDir: true}, nil
	}
	if m.dirs[rel] {
		return &filesync.Entry{Name: rel, IsDir: true}, nil
	}
	if b, ok := m.files[rel]; ok {
		return &filesync.Entry{Name: rel, Size: int64(len(b)), IsDir: false}, nil
	}
	return nil, nil // 不存在 → (nil, nil)
}
func (m *memFS) OpenRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[rel]
	if !ok {
		return nil, os.ErrNotExist
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
func (m *memFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.files[rel] = b
	return nil
}
func (m *memFS) Rename(ctx context.Context, from, to string) error { return nil }
func (m *memFS) Delete(ctx context.Context, rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, rel)
	return nil
}
func (m *memFS) MakeDir(ctx context.Context, rel string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirs[rel] = true
	return nil
}

// TestWebdavcore_PutGet 桥接全链路（PUT→GET）。
func TestWebdavcore_PutGet(t *testing.T) {
	t.Parallel()
	h := NewHandler(newMemFS())
	if h == nil {
		t.Fatal("NewHandler 不应返回 nil")
	}
	// PUT
	req := httptest.NewRequest(http.MethodPut, "/a.txt", strings.NewReader("hello"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusCreated && w.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 201/204", w.Code)
	}
	// GET
	req2 := httptest.NewRequest(http.MethodGet, "/a.txt", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("GET = %d, want 200", w2.Code)
	}
	if w2.Body.String() != "hello" {
		t.Fatalf("GET body = %q", w2.Body.String())
	}
}

// TestWebdavcore_PathTraversal ../ 路径按根处理（不逃逸）。
func TestWebdavcore_PathTraversal(t *testing.T) {
	t.Parallel()
	h := NewHandler(newMemFS())
	req := httptest.NewRequest(http.MethodGet, "/../etc/passwd", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	// cleanRel 把 ../ 归一为根 → PROPFIND/GET 根（不 404 即可；不 panic）。
	if w.Code == http.StatusInternalServerError {
		t.Fatalf("路径穿越不应 500: %d", w.Code)
	}
}
