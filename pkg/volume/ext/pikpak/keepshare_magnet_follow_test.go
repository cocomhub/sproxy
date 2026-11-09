// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

// keepshare_magnet_follow_test.go 锁定 **keepshare 镜像跟随必须原样保留 magnet 参数**
// （2026-11-09 用户指正 + 实测）：keepshare 链接的**必要形态**是
// `https://keepshare.org/<容器id>/magnet:?xt=urn:btih:<40hex>&dn=<name>`，
// 其中 `magnet:?...`（含 `&dn=`）**是必需的** —— 去掉后 keepshare 直接 404，
// 表现为「keepshare 终点非 PikPak 分享」，而带 magnet 时会 301→302 到 mypikpak 分享 ✓。
//
// 本文件用**记录型 RoundTripper**（无需网络/无需 sproxy）复现整条跟随链，
// 断言每一跳请求都带着 magnet 参数 —— 这能精确指出参数在哪一步丢失。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// followHopRT 记录每次请求的 URL，并按预设表返回重定向响应。
type followHopRT struct {
	mu   sync.Mutex
	seen []string
	hops map[string]struct {
		code int
		loc  string
	}
}

func (rt *followHopRT) RoundTrip(req *http.Request) (*http.Response, error) {
	u := req.URL.String()
	rt.mu.Lock()
	rt.seen = append(rt.seen, u)
	h, ok := rt.hops[u]
	rt.mu.Unlock()
	if !ok {
		// 未知 URL ⇒ 200 且无 Location（充当"终点"）
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{},
			Request:    req,
		}, nil
	}
	hdr := http.Header{}
	hdr.Set("Location", h.loc)
	return &http.Response{
		StatusCode: h.code,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     hdr,
		Request:    req,
	}, nil
}

const (
	ksMagnet = "https://keepshare.org/p5uj7smo/magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=NHDTC-230"
	ksCC     = "https://keepshare.cc/p5uj7smo/magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=NHDTC-230"
	ksPIK    = "https://mypikpak.com/s/VP5omDbRWBLhBU15uGQcBGGNo2/TOKEN"
)

// TestFollowKeepshareShareID_KeepsMagnetQuery 关键回归：跟随链每一跳都必须带 magnet 参数。
func TestFollowKeepshareShareID_KeepsMagnetQuery(t *testing.T) {
	t.Parallel()
	rt := &followHopRT{hops: map[string]struct {
		code int
		loc  string
	}{
		ksMagnet: {code: http.StatusMovedPermanently, loc: ksCC},
		ksCC:     {code: http.StatusFound, loc: ksPIK},
	}}
	client := &http.Client{Transport: rt}
	id, err := followKeepshareShareID(context.Background(), client, ksMagnet)
	if err != nil {
		t.Fatalf("应成功跟随到 mypikpak 分享: %v", err)
	}
	if id != "VP5omDbRWBLhBU15uGQcBGGNo2" {
		t.Fatalf("应拿到真实分享 id，got %q", id)
	}
	if len(rt.seen) != 2 {
		t.Fatalf("应有 2 跳请求（org→cc, cc→mypikpak），got %d: %v", len(rt.seen), rt.seen)
	}
	for i, u := range rt.seen {
		if !strings.Contains(u, "magnet:?xt=urn:btih:") || !strings.Contains(u, "dn=NHDTC-230") {
			t.Fatalf("第 %d 跳请求丢失 magnet 参数（keepshare 会 404）：%s", i+1, u)
		}
	}
}

// TestFollowKeepshareShareID_TruncatedMagnetWouldFail 反向对照：**去掉 magnet** 时
// keepshare 返回 404（无 Location）⇒ 报「终点非 PikPak 分享」——这正是我此前误判"服务终止"的场景。
func TestFollowKeepshareShareID_TruncatedMagnetWouldFail(t *testing.T) {
	t.Parallel()
	noMagnet := "https://keepshare.org/p5uj7smo"
	rt := &followHopRT{hops: map[string]struct {
		code int
		loc  string
	}{}}
	client := &http.Client{Transport: rt}
	_, err := followKeepshareShareID(context.Background(), client, noMagnet)
	if err == nil {
		t.Fatal("无 magnet 的 keepshare URL 不应解析成功（上游 404）")
	}
	if !strings.Contains(err.Error(), "终点非 PikPak 分享") {
		t.Fatalf("错误信息应指明终点非 PikPak 分享，got %v", err)
	}
	if fmt.Sprint(rt.seen) == "" {
		t.Fatal("应至少发出一次请求")
	}
}

// TestURLResolver_Resolve_KeepshareKeepsMagnet 端到端（进程内）：`Resolve` 传给跟随逻辑的 URL
// **必须**仍带 magnet 参数（若丢了，上游 404 ⇒ 「终点非 PikPak 分享」= 2026-11-09 实测现象）。
// 用记录型 client + nil API ⇒ 不发真实网络请求，只看**它请求的 URL**。
func TestURLResolver_Resolve_KeepshareKeepsMagnet(t *testing.T) {
	t.Parallel()
	rt := &followHopRT{hops: map[string]struct {
		code int
		loc  string
	}{
		ksMagnet: {code: http.StatusMovedPermanently, loc: ksCC},
		ksCC:     {code: http.StatusFound, loc: ksPIK},
	}}
	r := NewURLResolverWithClient(nil, nil, &http.Client{Transport: rt})
	_, err := r.Resolve(context.Background(), ksMagnet)
	// listOwned/listAnon 因 api/resolver 为空可能报错 —— 本用例只关心**请求 URL**。
	rt.mu.Lock()
	seen := append([]string(nil), rt.seen...)
	rt.mu.Unlock()
	if len(seen) == 0 {
		t.Fatalf("Resolve 未发出任何请求（err=%v）", err)
	}
	if !strings.Contains(seen[0], "magnet:?xt=urn:btih:") {
		t.Fatalf("Resolve 首跳请求丢失 magnet 参数（keepshare 会 404）：%s", seen[0])
	}
}
