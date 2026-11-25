// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netutil

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGatewayForwarder_Rewrite 验证 URL 改写规则（含必须保留的查询串）。
func TestGatewayForwarder_Rewrite(t *testing.T) {
	t.Parallel()
	fw, err := ParseGatewayForwarder("http://sr.example:18081/")
	if err != nil || fw == nil {
		t.Fatalf("解析失败: %v", err)
	}
	cases := []struct{ target, want string }{
		{
			"https://keepshare.org/p1/magnet:?xt=urn:btih:abc&dn=X",
			"http://sr.example:18081/keepshare.org/p1/magnet:?xt=urn:btih:abc&dn=X",
		},
		{
			"https://api-drive.mypikpak.com/drive/v1/share",
			"http://sr.example:18081/api-drive.mypikpak.com/drive/v1/share",
		},
	}
	for _, c := range cases {
		tu, perr := url.Parse(c.target)
		if perr != nil {
			t.Fatalf("解析目标失败: %v", perr)
		}
		if got := fw.Rewrite(tu).String(); got != c.want {
			t.Fatalf("改写不符:\n got=%s\nwant=%s", got, c.want)
		}
	}
}

// TestGatewayForwarder_EmptyAndInvalid 验证空配置=直连、非法配置报错。
func TestGatewayForwarder_EmptyAndInvalid(t *testing.T) {
	t.Parallel()
	if fw, err := ParseGatewayForwarder("  "); fw != nil || err != nil {
		t.Fatalf("空串应返回 (nil,nil)，got (%v,%v)", fw, err)
	}
	if _, err := ParseGatewayForwarder("not-a-url"); err == nil {
		t.Fatal("非法 URL 应报错")
	}
}

// TestGatewayHTTPClient_RewritesAndPreservesQuery 端到端：请求应打到转发器路径，
// 且目标域名+路径+查询串原样出现在路径/查询中。
func TestGatewayHTTPClient_RewritesAndPreservesQuery(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var gotPath, gotQuery, gotHost string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath, gotQuery, gotHost = r.URL.Path, r.URL.RawQuery, r.Host
		mu.Unlock()
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	cli, err := NewGatewayHTTPClient(srv.URL+"/", 5*time.Second)
	if err != nil {
		t.Fatalf("构造 client 失败: %v", err)
	}
	resp, err := cli.Get("https://keepshare.org/p1/magnet:?xt=urn:btih:abc&dn=X")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if gotPath != "/keepshare.org/p1/magnet:" {
		t.Fatalf("路径改写不符: %q", gotPath)
	}
	if !strings.Contains(gotQuery, "xt=urn:btih:abc") || !strings.Contains(gotQuery, "dn=X") {
		t.Fatalf("查询串丢失: %q", gotQuery)
	}
	if !strings.Contains(gotHost, strings.TrimPrefix(srv.URL, "http://")) {
		t.Fatalf("Host 应指向转发器: %q", gotHost)
	}
}

// TestGatewayHTTPClient_FollowsRedirectThroughGateway 验证重定向每一跳都经转发器。
func TestGatewayHTTPClient_FollowsRedirectThroughGateway(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		n := len(paths)
		mu.Unlock()
		if n == 1 {
			// 第一跳：重定向到另一个真实站点（应被重新改写并经转发器）
			http.Redirect(w, r, "https://mypikpak.com/s/SHARE1", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "done")
	}))
	defer srv.Close()

	cli, err := NewGatewayHTTPClient(srv.URL+"/", 5*time.Second)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	resp, err := cli.Get("https://keepshare.org/p1/magnet:?xt=urn:btih:x")
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(paths) < 2 {
		t.Fatalf("应发生 2 跳（实际 %d: %v）", len(paths), paths)
	}
	if paths[1] != "/mypikpak.com/s/SHARE1" {
		t.Fatalf("第二跳未经转发器改写: %q", paths[1])
	}
}
