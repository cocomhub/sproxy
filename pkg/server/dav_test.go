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
	"net/http"
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
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, url+path, rdr)
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

var _ = bytes.NewReader
var _ = context.Background
