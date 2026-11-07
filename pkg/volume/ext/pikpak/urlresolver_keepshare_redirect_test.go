// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFollowKeepshareRedirect 验证 keepshare 镜像 URL 经重定向链拿到**真实** mypikpak
// 分享 id（keepshare.org/<容器id>/magnet:... 的 parts[0] 是 keepshare 容器 id，
// 不是 PikPak 分享 id；必须 301→302 跟随到 mypikpak.com/s/<真实id>）。
func TestFollowKeepshareRedirect(t *testing.T) {
	t.Parallel()
	// 模拟：keepshare.org 301 → keepshare.cc 302 → mypikpak.com/s/REALID?act=play
	mux := http.NewServeMux()
	mux.HandleFunc("/redir1", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redir2", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/redir2", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://mypikpak.com/s/REALSHAREID/token123?act=play", http.StatusFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := &http.Client{}
	got, err := followKeepshareShareID(context.Background(), client, srv.URL+"/redir1")
	if err != nil {
		t.Fatalf("followKeepshareShareID: %v", err)
	}
	if got != "REALSHAREID" {
		t.Fatalf("share id = %q, want REALSHAREID", got)
	}
}

// TestFollowKeepshareRedirect_NotFound 验证重定向终点不是 mypikpak 分享时返回错误
// （keepshare 的「分享不存在」控制台页），fail-closed 而不是猜 id。
func TestFollowKeepshareRedirect_NotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/console/shared/status?id=1", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := followKeepshareShareID(context.Background(), &http.Client{}, srv.URL+"/x"); err == nil {
		t.Fatal("非我的网盘分享终点应报错")
	}
}

// TestShareIDFromFinalURL 验证从重定向终点提取分享 id（含 token 段）。
func TestShareIDFromFinalURL(t *testing.T) {
	t.Parallel()
	cases := []struct{ url, want string }{
		{"https://mypikpak.com/s/ABC123", "ABC123"},
		{"https://mypikpak.com/s/ABC123/tok?act=play", "ABC123"},
		{"https://mypikpak.net/s/XYZ?x=1", "XYZ"},
		{"https://keepshare.cc/console/shared/status?id=1", ""},
		{"https://example.com/s/ABC", ""},
	}
	for _, c := range cases {
		if got := shareIDFromFinalURL(c.url); got != c.want {
			t.Errorf("shareIDFromFinalURL(%q) = %q, want %q", c.url, got, c.want)
		}
	}
}
