// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package webdav

// webdav_fs_test.go 钉住 WebDAV 客户端（sync.FS 7 方法，RFC 4918）的协议语义。
// 用 httptest 模拟 WebDAV 服务端（纯标准库，禁共享 http.DefaultTransport——
// 仓库硬规则 17：每个 WebDAVFS 实例独立 Transport）。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// newTestServer 启动模拟 WebDAV 服务端（内存 map + 请求记录）。
// 返回服务端实例（handler 字段可在各测试注入定制行为）+ cleanup。
func newTestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// fakeWebDAVServer 是内存 WebDAV 服务端：文件 map + 目录 map + 请求记录。
// 实现 PROPFIND/GET/PUT/MOVE/DELETE/MKCOL（HTTP 方法路由，足够驱动客户端测试）。
type fakeWebDAVServer struct {
	mu      sync.Mutex
	files   map[string]string // 路径 → 内容
	dirs    map[string]bool   // 目录路径标记
	authOK  string            // 期望 Basic 凭据（空 = 不校验）；格式 "user:pass"
	lastReq struct {
		method string
		path   string
		header http.Header
	}
}

func newFakeWebDAVServer() *fakeWebDAVServer {
	return &fakeWebDAVServer{
		files: map[string]string{},
		dirs:  map[string]bool{},
	}
}

// ServeHTTP 按方法路由（WebDAV 扩展方法经 X-HTTP-Method-Override 或直接方法）。
func (s *fakeWebDAVServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.lastReq.method = r.Method
	s.lastReq.path = r.URL.Path
	s.lastReq.header = r.Header.Clone()
	s.mu.Unlock()

	// 认证校验（Basic）。
	if s.authOK != "" {
		u, p, ok := r.BasicAuth()
		if !ok || u+":"+p != s.authOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	switch r.Method {
	case "PROPFIND":
		s.handlePropfind(w, r)
	case "GET":
		s.handleGet(w, r)
	case "PUT":
		s.handlePut(w, r)
	case "MOVE":
		s.handleMove(w, r)
	case "DELETE":
		s.handleDelete(w, r)
	case "MKCOL":
		s.handleMkcol(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *fakeWebDAVServer) handlePropfind(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := r.URL.Path
	depth := r.Header.Get("Depth")
	if depth == "1" {
		// depth=1：返回自身 + 直接子条目。
		multi := s.multistatusResponse(p, 0)
		for _, child := range s.childrenOf(p) {
			multi += s.multistatusResponse(child, 0)
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusMultiStatus)
		fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">%s</d:multistatus>`, multi)
		return
	}
	// depth=0：只返回自身；不存在 → 404。
	multi := s.multistatusResponse(p, 0)
	if multi == "" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusMultiStatus)
	fmt.Fprintf(w, `<?xml version="1.0"?><d:multistatus xmlns:d="DAV:">%s</d:multistatus>`, multi)
}

// multistatusResponse 生成单个 response 的 XML；资源不存在返回空串。
func (s *fakeWebDAVServer) multistatusResponse(p string, depth int) string {
	if p == "/" {
		return `<d:response><d:href>/</d:href><d:propstat><d:prop><d:resourcetype><d:collection/></d:resourcetype><d:displayname></d:displayname></d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`
	}
	content, isFile := s.files[p]
	isDir := s.dirs[p]
	if !isFile && !isDir {
		return ""
	}
	resType := ""
	if isDir {
		resType = "<d:collection/>"
	}
	display := strings.TrimPrefix(p, "/")
	name := display
	if idx := strings.LastIndex(display, "/"); idx >= 0 {
		name = display[idx+1:]
	}
	var size string
	if isFile {
		size = fmt.Sprintf("<d:getcontentlength>%d</d:getcontentlength>", len(content))
	}
	mtime := time.Unix(1700000000, 0).UTC().Format(http.TimeFormat)
	xml := fmt.Sprintf(`<d:response><d:href>%s</d:href><d:propstat><d:prop><d:resourcetype>%s</d:resourcetype><d:displayname>%s</d:displayname><d:getlastmodified>%s</d:getlastmodified>%s</d:prop><d:status>HTTP/1.1 200 OK</d:status></d:propstat></d:response>`,
		p, resType, xmlEscape(name), mtime, size)
	return xml
}

// childrenOf 返回 p 的直接子条目（文件 + 目录），已排序。
func (s *fakeWebDAVServer) childrenOf(p string) []string {
	prefix := p
	if prefix != "/" {
		prefix += "/"
	}
	seen := map[string]bool{}
	var out []string
	for f := range s.files {
		if after, ok := strings.CutPrefix(f, prefix); ok {
			rest := after
			if !strings.Contains(rest, "/") {
				seen[f] = true
			}
		}
	}
	for d := range s.dirs {
		if d == p {
			continue
		}
		if after, ok := strings.CutPrefix(d, prefix); ok {
			rest := after
			if !strings.Contains(rest, "/") {
				seen[d] = true
			}
		}
	}
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (s *fakeWebDAVServer) handleGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	content, ok := s.files[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(content))
}

func (s *fakeWebDAVServer) handlePut(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	s.files[r.URL.Path] = string(data)
	w.WriteHeader(http.StatusCreated)
}

func (s *fakeWebDAVServer) handleMove(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dest := r.Header.Get("Destination")
	destURL, err := url.Parse(dest)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	destPath := destURL.Path
	content, ok := s.files[r.URL.Path]
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	delete(s.files, r.URL.Path)
	s.files[destPath] = content
	w.WriteHeader(http.StatusCreated)
}

func (s *fakeWebDAVServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[r.URL.Path]; ok {
		delete(s.files, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if s.dirs[r.URL.Path] {
		delete(s.dirs, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.WriteHeader(http.StatusNotFound)
}

func (s *fakeWebDAVServer) handleMkcol(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirs[r.URL.Path] {
		w.WriteHeader(http.StatusMethodNotAllowed) // RFC 4918：已存在 → 405
		return
	}
	s.dirs[r.URL.Path] = true
	w.WriteHeader(http.StatusCreated)
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// newClient 构造 WebDAVFS 客户端（测试辅助：httptest 服务端地址）。
func newClient(t *testing.T, serverURL string, opts ...ClientOption) *WebDAVFS {
	t.Helper()
	// 硬规则 17：测试必须注入**独立连接池**（IsolatedTransport），禁共享
	// netutil.DefaultTransport()——WebDAVFS.Close() 会 CloseIdleConnections 清空
	// 共享池，多个 t.Parallel() 用例会互相打断在途请求（CI 实测
	// TestWebDAVFS_Rename 报 "transport connection broken: CloseIdleConnections
	// called"）。IsolatedClient 自带 t.Cleanup 只关自己用例的池。
	opts = append(opts, WithHTTPClient(testutil.IsolatedClient(t)))
	cfg := ClientConfig{RootURL: serverURL}
	for _, o := range opts {
		o(&cfg)
	}
	fs, err := NewWebDAVFS(cfg)
	if err != nil {
		t.Fatalf("NewWebDAVFS: %v", err)
	}
	t.Cleanup(func() { _ = fs.Close() })
	return fs
}

// ---- 测试 ----

func TestWebDAVFS_WriteRead(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	if err := fs.WriteFile(ctx, "dir/file.txt", strings.NewReader("hello webdav"), 12, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "dir/file.txt")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(data) != "hello webdav" {
		t.Fatalf("读回内容 = %q, want %q", string(data), "hello webdav")
	}
}

func TestWebDAVFS_ListDir(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/a.txt"] = "aaa"
	srv.files["/sub/b.txt"] = "bbb"
	srv.dirs["/sub"] = true
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	entries, err := fs.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Path] = true
		if e.Path == "sub" && !e.IsDir {
			t.Fatalf("sub 应标记为目录")
		}
		if e.Path == "a.txt" && e.IsDir {
			t.Fatalf("a.txt 不应是目录")
		}
		if e.Path == "a.txt" && e.Size != 3 {
			t.Fatalf("a.txt size = %d, want 3", e.Size)
		}
	}
	if !got["a.txt"] || !got["sub"] {
		t.Fatalf("ListDir 应含 a.txt+sub, got %v", got)
	}
	if got["sub/b.txt"] {
		t.Fatalf("ListDir 不应递归子目录（depth=1 单层）")
	}
}

func TestWebDAVFS_Stat(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/exists.txt"] = "data"
	srv.dirs["/dir"] = true
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	e, err := fs.Stat(ctx, "exists.txt")
	if err != nil {
		t.Fatalf("Stat(exists): %v", err)
	}
	if e == nil || e.IsDir {
		t.Fatalf("Stat(exists) = %+v, want 文件条目", e)
	}
	if e.Size != 4 {
		t.Fatalf("Stat size = %d, want 4", e.Size)
	}
	d, err := fs.Stat(ctx, "dir")
	if err != nil {
		t.Fatalf("Stat(dir): %v", err)
	}
	if d == nil || !d.IsDir {
		t.Fatalf("Stat(dir) = %+v, want 目录条目", d)
	}
	// 不存在 → (nil, nil)。
	missing, err := fs.Stat(ctx, "missing.txt")
	if err != nil {
		t.Fatalf("Stat(missing): %v", err)
	}
	if missing != nil {
		t.Fatalf("Stat(missing) = %+v, want nil", missing)
	}
}

func TestWebDAVFS_Rename(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/old.txt"] = "move me"
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	if err := fs.Rename(ctx, "old.txt", "new.txt"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "new.txt")
	if err != nil {
		t.Fatalf("OpenRead(new): %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "move me" {
		t.Fatalf("移动后内容 = %q, want %q", string(data), "move me")
	}
	// 源已不存在。
	if _, err := fs.Stat(ctx, "old.txt"); err != nil {
		t.Fatalf("Stat(old): %v", err)
	}
}

func TestWebDAVFS_Delete(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/del.txt"] = "bye"
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	if err := fs.Delete(ctx, "del.txt"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := fs.Stat(ctx, "del.txt"); err != nil {
		t.Fatalf("Stat(after delete): %v", err)
	}
	// 删除不存在 → 幂等 nil。
	if err := fs.Delete(ctx, "already-gone.txt"); err != nil {
		t.Fatalf("Delete(missing) 应幂等 nil: %v", err)
	}
}

func TestWebDAVFS_MakeDir(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL)

	ctx := context.Background()
	if err := fs.MakeDir(ctx, "newdir"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	e, err := fs.Stat(ctx, "newdir")
	if err != nil {
		t.Fatalf("Stat(newdir): %v", err)
	}
	if e == nil || !e.IsDir {
		t.Fatalf("MakeDir 后 Stat = %+v, want 目录", e)
	}
	// 已存在 → 幂等 nil（RFC 4918：服务端 405，客户端幂等化）。
	if err := fs.MakeDir(ctx, "newdir"); err != nil {
		t.Fatalf("MakeDir(existing) 应幂等 nil: %v", err)
	}
}

func TestWebDAVFS_Auth_Basic(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.authOK = "alice:secret"
	srv.files["/auth.txt"] = "protected"
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL, WithBasicAuth("alice", "secret"))

	ctx := context.Background()
	rc, err := fs.OpenRead(ctx, "auth.txt")
	if err != nil {
		t.Fatalf("OpenRead(auth): %v", err)
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	if string(data) != "protected" {
		t.Fatalf("认证读回 = %q, want %q", string(data), "protected")
	}
}

func TestWebDAVFS_Auth_WrongPassword(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.authOK = "alice:secret"
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL, WithBasicAuth("alice", "wrong"))

	ctx := context.Background()
	if _, err := fs.OpenRead(ctx, "x.txt"); err == nil {
		t.Fatalf("错误密码应报错，got nil")
	}
}

func TestWebDAVFS_Auth_Bearer(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	ts := newTestServer(t, srv)
	fs := newClient(t, ts.URL, WithBearerToken("tok-123"))

	ctx := context.Background()
	if err := fs.WriteFile(ctx, "tok.txt", strings.NewReader("t"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if got := srv.lastReq.header.Get("Authorization"); got != "Bearer tok-123" {
		t.Fatalf("Authorization = %q, want %q", got, "Bearer tok-123")
	}
}

func TestWebDAVFS_PathJoin(t *testing.T) {
	t.Parallel()
	srv := newFakeWebDAVServer()
	srv.files["/"] = ""
	ts := newTestServer(t, srv)
	// 根 URL 带路径前缀（如 /remote.php/webdav——Nextcloud 风格）。
	fs := newClient(t, ts.URL+"/remote.php/webdav")

	ctx := context.Background()
	if err := fs.WriteFile(ctx, "nested/deep/file.txt", strings.NewReader("x"), 1, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.lastReq.path != "/remote.php/webdav/nested/deep/file.txt" {
		t.Fatalf("请求路径 = %q, want 根前缀拼接", srv.lastReq.path)
	}
}
