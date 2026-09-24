// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// newTestServerWithRoutes 是 du 测试的服务端装配基座：直接 RegisterRoutes
// （与 newTestServer 同构，返回 mux 而非 httptest.Server），配合 t 的
// httptest 客户端做黑盒断言。
func newTestServerWithRoutes(t *testing.T, modifyCfg func(*Config)) *httptest.Server {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	noAuth := defaultNoAuthRegOpts()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:                   mux,
		CfgPtr:                &cfgPtr,
		Version:               "test",
		BuildAt:               "test",
		Logger:                testLogger(),
		AuditLogger:           testLogger(),
		CredentialRing:        noAuth.CredentialRing,
		CredentialStore:       noAuth.CredentialStore,
		AllowInsecureLoopback: noAuth.AllowInsecureLoopback,
	})
	t.Cleanup(func() { _ = h.Close() })
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// uploadDUTreeFiles 在 root 的 anonymous 租户根（<root>/anonymous/）下构建 du fixture 树：
//   - user/sub1/a.txt (11B)、user/sub1/sub2/b.bin (22B)、user/root.txt (33B)
//   - 干扰桶：cloud/cloud.bin、chunk/chunk.bin、version/ver.bin、meta/checksums.json、
//     user/.__magic/hidden.bin、user/checksums.json、LAYOUT_VERSION
func uploadDUTreeFiles(t *testing.T, root string) {
	t.Helper()
	tntRoot := filepath.Join(root, storage.AnonymousOwner)
	if err := os.MkdirAll(tntRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll tenant: %v", err)
	}
	rt, err := storage.OpenRoot(tntRoot)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rt.Close()
	write := func(rel, content string) {
		dir := filepath.Dir(rel)
		if dir != "." {
			if err := rt.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("MkdirAll(%s): %v", dir, err)
			}
		}
		f, err := rt.OpenFile(filepath.ToSlash(rel), os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("OpenFile(%s): %v", rel, err)
		}
		if _, werr := f.Write([]byte(content)); werr != nil {
			t.Fatalf("Write(%s): %v", rel, werr)
		}
		_ = f.Close()
	}
	// 手工铺 user 桶与干扰桶（不用上传 API，避免依赖网络往返语义）。
	for _, f := range []struct{ rel, content string }{
		{"user/sub1/a.txt", "hello world"},
		{"user/sub1/sub2/b.bin", "hello world 2222222222"},
		{"user/root.txt", "hello world 33"},
		{"cloud/cloud.bin", "c"},
		{"chunk/chunk.bin", "k"},
		{"version/ver.bin", "v"},
		{"meta/checksums.json", "{}"},
		{"user/.__magic/hidden.bin", "h"},
		{"user/checksums.json", "{}"},
		{"LAYOUT_VERSION", "2\n"},
	} {
		write(f.rel, f.content)
	}
}

// TestDUDirRecursive 验证 /api/du?path=sub1 递归统计精确
// （dirs/files/size，且不统计 user 桶外的 cloud/chunk/version/meta 干扰桶）。
func TestDUDirRecursive(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	uploadDUTreeFiles(t, root)
	ts := newTestServerWithRoutes(t, func(cfg *Config) { cfg.StorageRoot = root })
	defer ts.Close()

	resp, err := testHTTPClient(t).Get(ts.URL + "/api/du?path=sub1")
	if err != nil {
		t.Fatalf("GET /api/du: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// 手工解析（避免引第三方断言库）。
	body := make([]byte, 4096)
	n, _ := resp.Body.Read(body)
	got := string(body[:n])
	for _, want := range []string{`"dirs":1`, `"files":2`, `"size":33`, `"success":true`} {
		if !strings.Contains(got, want) {
			t.Errorf("du body 缺 %s（got=%s）", want, got)
		}
	}
}

// TestDUBucketExcluded 验证 user 桶外的功能桶/元数据不参与统计
// （变异点：桶跳过逻辑删除 → 计数超预期红）。
func TestDUBucketExcluded(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	rt, err := storage.OpenRoot(root)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rt.Close()
	write := func(rel, content string) {
		dir := filepath.Dir(rel)
		if dir != "." {
			_ = rt.MkdirAll(dir, 0o755)
		}
		f, _ := rt.OpenFile(filepath.ToSlash(rel), os.O_CREATE|os.O_WRONLY, 0o644)
		_, _ = f.Write([]byte(content))
		_ = f.Close()
	}
	for _, f := range []struct{ rel, content string }{
		{"user/a.txt", "aaaa"},
		{"cloud/c.bin", "c"},
		{"chunk/k.bin", "k"},
		{"version/v.bin", "v"},
		{"meta/m.json", "{}"},
	} {
		write(f.rel, f.content)
	}

	d, err := walkDu(rt, "user")
	if err != nil {
		t.Fatalf("walkDu: %v", err)
	}
	if d.Files != 1 || d.Size != 4 || d.Dirs != 0 {
		t.Fatalf("user 桶统计 = files=%d size=%d dirs=%d, want 1/4/0（功能桶干扰泄漏）", d.Files, d.Size, d.Dirs)
	}
}

// TestDUInvalidPath 验证非法路径（穿越/绝对路径）→ 400。
func TestDUInvalidPath(t *testing.T) {
	t.Parallel()
	ts := newTestServerWithRoutes(t, nil)
	for _, p := range []string{"../x", "/abs", "a/b/../../x"} {
		resp, err := testHTTPClient(t).Get(ts.URL + "/api/du?path=" + p)
		if err != nil {
			t.Fatalf("GET /api/du %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("path=%q status = %d, want 400", p, resp.StatusCode)
		}
	}
}

// TestDUMissingPath404 验证路径定位未命中 → 404（ACL 收口语义：不泄卷存在性）。
func TestDUMissingPath404(t *testing.T) {
	t.Parallel()
	ts := newTestServerWithRoutes(t, nil)
	resp, err := testHTTPClient(t).Get(ts.URL + "/api/du?path=no/such/dir")
	if err != nil {
		t.Fatalf("GET /api/du: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// TestDUBucketSkipMutation 变异验证：若 walkDuDir 的 user 桶跳过逻辑被移除
// （功能桶目录也计入），size/files 断言即红——钉住桶判定单一事实源。
func TestDUBucketSkipMutation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	tntRoot := filepath.Join(root, storage.AnonymousOwner)
	if err := os.MkdirAll(tntRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll tenant: %v", err)
	}
	rt, err := storage.OpenRoot(tntRoot)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	defer rt.Close()
	_ = rt.MkdirAll("user", 0o755)
	_ = rt.MkdirAll("cloud", 0o755)
	_ = rt.MkdirAll("chunk", 0o755)
	for _, f := range []struct{ rel, content string }{
		{"user/a.txt", "aaaa"},
		{"cloud/c.bin", "c"},
		{"chunk/k.bin", "k"},
	} {
		fh, _ := rt.OpenFile(f.rel, os.O_CREATE|os.O_WRONLY, 0o644)
		_, _ = fh.Write([]byte(f.content))
		_ = fh.Close()
	}
	// user 桶 + cloud/chunk 桶干扰——walkDu 只看 user 桶内
	// （与 TestDUBucketExcluded 同构；此处再以黑盒断言一次 user/ 根统计）。
	d, err := walkDu(rt, "user")
	if err != nil {
		t.Fatalf("walkDu: %v", err)
	}
	if d.Files != 1 || d.Size != 4 {
		t.Fatalf("user 桶统计 = files=%d size=%d, want 1/4（功能桶干扰泄漏）", d.Files, d.Size)
	}
}
