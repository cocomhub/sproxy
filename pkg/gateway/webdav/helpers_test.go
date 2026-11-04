// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memFS 是 syncpkg.FS 的内存实现（测试夹具）。
type memFS struct {
	mu      sync.Mutex
	entries map[string][]byte
	dirs    map[string]bool
}

func newMemFS() *memFS {
	return &memFS{entries: map[string][]byte{}, dirs: map[string]bool{"": true}}
}

func (m *memFS) ensureParents(p string) {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			m.dirs[p[:i]] = true
		}
	}
}

func (m *memFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.dirs[p] {
		return nil, osErrNotExist
	}
	prefix := ""
	if p != "" {
		prefix = p + "/"
	}
	seen := map[string]bool{}
	out := m.collectFileEntries(p, prefix, seen, nil)
	out = m.collectDirEntries(p, prefix, seen, out)
	return out, nil
}

// collectFileEntries 收集 entries 表中最浅层文件/目录条目（按首段去重）。
func (m *memFS) collectFileEntries(p, prefix string, seen map[string]bool, out []syncpkg.Entry) []syncpkg.Entry {
	for k := range m.entries {
		if !strings.HasPrefix(k, prefix) || k == p {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		first := rest
		if i := strings.IndexByte(first, '/'); i >= 0 {
			first = first[:i]
		}
		if seen[first] {
			continue
		}
		seen[first] = true
		full := prefix + first
		if m.dirs[full] {
			out = append(out, syncpkg.Entry{Name: first, Path: full, IsDir: true})
		} else {
			data := m.entries[full]
			out = append(out, syncpkg.Entry{Name: first, Path: full, Size: int64(len(data))})
		}
	}
	return out
}

// collectDirEntries 补齐只含子目录（entries 表无记录）的深层目录条目。
func (m *memFS) collectDirEntries(p, prefix string, seen map[string]bool, out []syncpkg.Entry) []syncpkg.Entry {
	for d := range m.dirs {
		if d == "" || d == p || !strings.HasPrefix(d, prefix) {
			continue
		}
		rest := strings.TrimPrefix(d, prefix)
		if strings.Contains(rest, "/") {
			continue
		}
		if !seen[rest] {
			seen[rest] = true
			full := prefix + rest
			out = append(out, syncpkg.Entry{Name: rest, Path: full, IsDir: true})
		}
	}
	return out
}

func (m *memFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p == "" {
		return &syncpkg.Entry{Name: "", Path: "", IsDir: true}, nil
	}
	if m.dirs[p] {
		return &syncpkg.Entry{Name: pathBase(p), Path: p, IsDir: true}, nil
	}
	if data, ok := m.entries[p]; ok {
		return &syncpkg.Entry{Name: pathBase(p), Path: p, Size: int64(len(data))}, nil
	}
	return nil, nil
}

func (m *memFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.entries[p]
	if !ok {
		return nil, osErrNotExist
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *memFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.ensureParents(p)
	m.entries[p] = data
	return nil
}

func (m *memFS) Rename(ctx context.Context, from, to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if from == to {
		return nil
	}
	if data, ok := m.entries[from]; ok {
		m.ensureParents(to)
		m.entries[to] = data
		delete(m.entries, from)
		return nil
	}
	if m.dirs[from] {
		// 目录重命名：迁移所有前缀条目。
		m.dirs[to] = true
		delete(m.dirs, from)
		prefix := from + "/"
		for k := range m.entries {
			if after, ok := strings.CutPrefix(k, prefix); ok {
				m.ensureParents(to + "/" + after)
				m.entries[to+"/"+strings.TrimPrefix(k, prefix)] = m.entries[k]
				delete(m.entries, k)
			}
		}
		for d := range m.dirs {
			if d != "" && strings.HasPrefix(d, prefix) {
				m.dirs[to+"/"+strings.TrimPrefix(d, prefix)] = true
				delete(m.dirs, d)
			}
		}
		return nil
	}
	return osErrNotExist
}

func (m *memFS) Delete(ctx context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.entries[p]; ok {
		delete(m.entries, p)
		return nil
	}
	if m.dirs[p] {
		prefix := p + "/"
		for k := range m.entries {
			if strings.HasPrefix(k, prefix) {
				delete(m.entries, k)
			}
		}
		for d := range m.dirs {
			if d != "" && strings.HasPrefix(d, prefix) {
				delete(m.dirs, d)
			}
		}
		delete(m.dirs, p)
		return nil
	}
	return osErrNotExist
}

func (m *memFS) MakeDir(ctx context.Context, p string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureParents(p)
	m.dirs[p] = true
	return nil
}

func pathBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

// osErrNotExist 统一不存在哨兵（syncpkg.FS 契约：Stat 缺失返回 (nil,nil)，其余返回 os.ErrNotExist）。
var osErrNotExist = osErrorNotExist()

func osErrorNotExist() error {
	// 直接引用 os.ErrNotExist（纯标准库，无依赖）。
	return errNotExist
}

var errNotExist = notExistError{}

type notExistError struct{}

func (notExistError) Error() string        { return "file does not exist" }
func (notExistError) Is(target error) bool { return target.Error() == "file does not exist" }

// doRequest 发一个 WebDAV 请求并返回响应。
func doRequest(t *testing.T, h http.Handler, method, urlPath string, body []byte, headers map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(method, urlPath, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

// TestWebDAVHandler_PropfindGetPut 验证 WebDAV 全链路：PUT→GET→PROPFIND→MKCOL→DELETE→MOVE。
