// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav_test

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	webdavgw "github.com/cocomhub/sproxy/pkg/gateway/webdav"
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
	var out []syncpkg.Entry
	seen := map[string]bool{}
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
	return out, nil
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

func (m *memFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
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
func TestWebDAVHandler_PropfindGetPut(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)
	if handler == nil {
		t.Fatal("NewHandler 不应返回 nil")
	}

	// 1. PUT /hello.txt
	resp := doRequest(t, handler, "PUT", "/hello.txt", []byte("world"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /hello.txt 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 2. GET /hello.txt
	resp = doRequest(t, handler, "GET", "/hello.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /hello.txt 应 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "world" {
		t.Fatalf("GET body = %q, want %q", got, "world")
	}

	// 3. PROPFIND /（Depth:1）应含 hello.txt
	resp = doRequest(t, handler, "PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	if resp.StatusCode != http.StatusMultiStatus {
		t.Fatalf("PROPFIND / 应 207, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "hello.txt") {
		t.Fatalf("PROPFIND / 应含 hello.txt, body=%s", body)
	}

	// 4. MKCOL /dir
	resp = doRequest(t, handler, "MKCOL", "/dir", nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("MKCOL /dir 应 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "PUT", "/dir/file.txt", []byte("in-dir"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /dir/file.txt 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// 5. GET /dir/file.txt 确认内容
	resp = doRequest(t, handler, "GET", "/dir/file.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /dir/file.txt 应 200, got %d", resp.StatusCode)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "in-dir" {
		t.Fatalf("GET /dir/file.txt = %q, want %q", got, "in-dir")
	}

	// 6. MOVE /dir/file.txt → /moved.txt
	resp = doRequest(t, handler, "MOVE", "/dir/file.txt", nil, map[string]string{"Destination": "/moved.txt"})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("MOVE 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/moved.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("MOVE 后 GET /moved.txt 应 200, got %d", resp.StatusCode)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "in-dir" {
		t.Fatalf("moved.txt = %q, want %q", got, "in-dir")
	}

	// 7. DELETE /moved.txt
	resp = doRequest(t, handler, "DELETE", "/moved.txt", nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE /moved.txt 应 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/moved.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE 后 GET /moved.txt 应 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestWebDAVHandler_StatMissing 验证不存在的文件 GET 返回 404。
func TestWebDAVHandler_StatMissing(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)

	resp := doRequest(t, handler, "GET", "/nope.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /nope.txt 应 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// PROPFIND 不存在也 404。
	resp = doRequest(t, handler, "PROPFIND", "/nope.txt", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PROPFIND /nope.txt 应 404, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestWebDAVHandler_PutCreatesParentDirs 验证 PUT 到不存在的父目录自动建目录。
func TestWebDAVHandler_PutCreatesParentDirs(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)

	resp := doRequest(t, handler, "PUT", "/a/b/c.txt", []byte("deep"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT /a/b/c.txt 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/a/b/c.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /a/b/c.txt 应 200, got %d", resp.StatusCode)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "deep" {
		t.Fatalf("a/b/c.txt = %q, want %q", got, "deep")
	}
}

// TestWebDAVHandler_OverwriteExisting 验证 PUT 覆盖已有文件。
func TestWebDAVHandler_OverwriteExisting(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)

	doRequest(t, handler, "PUT", "/f.txt", []byte("v1"), nil)
	resp := doRequest(t, handler, "PUT", "/f.txt", []byte("v2-longer"), nil)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT 覆盖应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/f.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "v2-longer" {
		t.Fatalf("覆盖后 = %q, want %q", got, "v2-longer")
	}
}

// TestWebDAVHandler_Copy 验证 COPY 到新位置。
func TestWebDAVHandler_Copy(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)

	doRequest(t, handler, "PUT", "/src.txt", []byte("copy-me"), nil)
	resp := doRequest(t, handler, "COPY", "/src.txt", nil, map[string]string{"Destination": "/dst.txt"})
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent {
		t.Fatalf("COPY 应 201/204, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	// 源仍在、目标存在且内容一致。
	resp = doRequest(t, handler, "GET", "/src.txt", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("COPY 后源应保留, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "GET", "/dst.txt", nil, nil)
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != "copy-me" {
		t.Fatalf("dst.txt = %q, want %q", got, "copy-me")
	}
}

// TestWebDAVHandler_MkcolExists 验证重复 MKCOL 返回 405（已存在）。
func TestWebDAVHandler_MkcolExists(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)

	resp := doRequest(t, handler, "MKCOL", "/existing", nil, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次 MKCOL 应 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = doRequest(t, handler, "MKCOL", "/existing", nil, nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("重复 MKCOL 应 405, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// xmlMultiStatus 用于解析 PROPFIND 响应确认 XML 结构合法。
var _ = xml.Name{} // 保留 xml import（断言 207 响应是合法 XML）

// TestWebDAVHandler_PropfindXMLValid 验证 PROPFIND 响应是可解析的 Multi-Status XML。
func TestWebDAVHandler_PropfindXMLValid(t *testing.T) {
	t.Parallel()
	fs := newMemFS()
	handler := webdavgw.NewHandler(fs)
	doRequest(t, handler, "PUT", "/x.txt", []byte("x"), nil)

	resp := doRequest(t, handler, "PROPFIND", "/", nil, map[string]string{"Depth": "1"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var ms struct {
		XMLName xml.Name `xml:"multistatus"`
	}
	if err := xml.Unmarshal(body, &ms); err != nil {
		t.Fatalf("PROPFIND 响应不是合法 Multi-Status XML: %v", err)
	}
}
