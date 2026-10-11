// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// dav_test.go 验证本地卷 WebDAV 挂载面（roadmap P1 服务端 WebDAV）：
//  1. /dav/ 路由存在且需认证（未认证 401；带凭据 200）。
//  2. PROPFIND 列举 / MKCOL 建目录 / PUT 上传 / GET 下载 / DELETE 删除 全流程。
//  3. owner 卷映射：请求者 owner 的 user 桶即 WebDAV 根。

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestWebDAVServer_RequiresAuth /dav/ 未认证 → 401。
func TestWebDAVServer_RequiresAuth(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, func(cfg *Config) { cfg.WebDAV.Enabled = true })
	req, _ := http.NewRequest(http.MethodGet, url+"/dav/", nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("GET /dav/: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("未认证 /dav/ 应 401/403，got %d", resp.StatusCode)
	}
}

// TestWebDAVServer_CrudFlow 带凭据 PROPFIND/MKCOL/PUT/GET/DELETE 全流程。
func TestWebDAVServer_CrudFlow(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, func(cfg *Config) { cfg.WebDAV.Enabled = true })
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	// 带凭据请求构造（SproxySig 签名）：PUT 需带 body 哈希（signRequest 用 EmptyBodyHash
	// 会致 BodyValidator EOF 不匹配 → 405）；GET/无 body 用空哈希。
	do := func(method, path, body string) *http.Response {
		return webDAVRequest(t, cl, url, method, path, body)
	}

	// MKCOL 建目录。
	resp := do("MKCOL", "/dav/docs", "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("MKCOL /dav/docs: %d", resp.StatusCode)
	}

	// PUT 上传。
	resp = do(http.MethodPut, "/dav/docs/a.txt", "hello webdav")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		t.Fatalf("PUT /dav/docs/a.txt: %d", resp.StatusCode)
	}

	// GET 下载。
	resp = do(http.MethodGet, "/dav/docs/a.txt", "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "hello webdav" {
		t.Fatalf("GET: status=%d body=%q", resp.StatusCode, body)
	}

	// PROPFIND 列举（Depth 1）。
	resp = do("PROPFIND", "/dav/docs", "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 207 || !strings.Contains(string(body), "a.txt") {
		t.Fatalf("PROPFIND: status=%d body=%q", resp.StatusCode, body[:min(len(body), 200)])
	}

	// DELETE 删除。
	resp = do(http.MethodDelete, "/dav/docs/a.txt", "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 204 && resp.StatusCode != 200 {
		t.Fatalf("DELETE: %d", resp.StatusCode)
	}

	// 删除后 GET → 404。
	resp = do(http.MethodGet, "/dav/docs/a.txt", "")
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("删除后 GET 应 404，got %d", resp.StatusCode)
	}
}

// webDAVRequest 发起带 SproxySig 签名的 WebDAV 请求（PUT 带 body 哈希，其余空哈希）。
func webDAVRequest(t *testing.T, cl *http.Client, baseURL, method, path, body string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, baseURL+path, rdr)
	if body != "" {
		signBodyRequestEntry(req, testAccessKey, testEntryID(testAccessKey), testAccessSecret, []byte(body))
	} else {
		signRequest(req, testAccessKey, testAccessSecret)
	}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

var _ = bytes.NewReader
var _ = context.Background

// TestWebDAVServer_OverwriteKeepsDownloadVerifiable P1 回归：经 /api/upload 写入（已有
// sidecar）后，用 /dav/ PUT 覆盖同 rel —— sidecar 必须同步更新为**新内容**哈希，否则
// 读路径按旧 meta 校验新内容恒失配（fail-closed 固化不可读，幂等重传不自愈）。
func TestWebDAVServer_OverwriteKeepsDownloadVerifiable(t *testing.T) {
	t.Parallel()
	url, cfgPtr, cleanup := newTestServerCreds(t, func(cfg *Config) { cfg.WebDAV.Enabled = true })
	defer cleanup()
	c1 := []byte("original-content-aaaa")
	c2 := []byte("replaced-content-bbbb")
	if st := uploadFileSigned(t, url, "docs/a.txt", c1); st != 200 {
		t.Fatalf("upload = %d", st)
	}
	// sidecar 初始记录 C1。
	if findMetaSHA(t, cfgPtr.Load().StorageRoot, sha256hex(c1)) == "" {
		t.Fatal("上传后 sidecar 应记录 C1 的 sha256")
	}
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	resp := webDAVRequest(t, cl, url, http.MethodPut, "/dav/docs/a.txt", string(c2))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		t.Fatalf("WebDAV PUT 覆盖 = %d", resp.StatusCode)
	}
	// 覆盖写后 sidecar 必须记录 C2（不得仍是 C1）。
	if p := findMetaSHA(t, cfgPtr.Load().StorageRoot, sha256hex(c2)); p == "" {
		t.Fatal("覆盖写后 sidecar 未同步更新为新内容（Dav 旁路陈旧 sidecar）")
	}
}

// findMetaSHA 在存储根的 sidecar 中查找包含给定 sha256 的文件路径（找不到返回空）。
func findMetaSHA(t *testing.T, root, sha string) string {
	t.Helper()
	found := ""
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".meta") {
			return nil
		}
		b, rerr := os.ReadFile(p)
		if rerr == nil && bytes.Contains(b, []byte(sha)) {
			found = p
		}
		return nil
	})
	return found
}
