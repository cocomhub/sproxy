// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// fakeResolverRegistry 是 ResolverRegistry 的测试实现（可控结果/错误/未装配）。
type fakeResolverRegistry struct {
	info *downloader.ResolveInfo
	err  error
	got  string
}

func (f *fakeResolverRegistry) ResolveURL(ctx context.Context, rawURL string) (*downloader.ResolveInfo, error) {
	f.got = rawURL
	if f.err != nil {
		return nil, f.err
	}
	return f.info, nil
}

// TestResolveURLHandler_OK 验证 POST /api/resolve 返回扩展解析结果。
func TestResolveURLHandler_OK(t *testing.T) {
	t.Parallel()
	h := &Handlers{urlResolver: &fakeResolverRegistry{info: &downloader.ResolveInfo{
		Kind: "pikpak_share", Filename: "NHDTA-513A.mp4", Size: 1000,
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", strings.NewReader(`{"url":"https://mypikpak.com/s/abc"}`))
	w := httptest.NewRecorder()
	h.resolveURL(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got downloader.ResolveInfo
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "pikpak_share" || got.Filename != "NHDTA-513A.mp4" {
		t.Fatalf("got = %+v", got)
	}
}

// TestResolveURLHandler_NotAssembled 验证未装配解析器时返回 503（而非静默 200 假结果）。
func TestResolveURLHandler_NotAssembled(t *testing.T) {
	t.Parallel()
	h := &Handlers{} // urlResolver nil
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", strings.NewReader(`{"url":"x"}`))
	w := httptest.NewRecorder()
	h.resolveURL(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
}

// TestResolveURLHandler_ResolverError 验证扩展解析失败映射 502（上游资源问题）。
func TestResolveURLHandler_ResolverError(t *testing.T) {
	t.Parallel()
	h := &Handlers{urlResolver: &fakeResolverRegistry{err: errors.New("share not found")}}
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", strings.NewReader(`{"url":"https://mypikpak.com/s/dead"}`))
	w := httptest.NewRecorder()
	h.resolveURL(w, req)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", w.Code)
	}
}

// TestResolveURLHandler_BadBody 验证坏请求体 400。
func TestResolveURLHandler_BadBody(t *testing.T) {
	t.Parallel()
	h := &Handlers{urlResolver: &fakeResolverRegistry{}}
	req := httptest.NewRequest(http.MethodPost, "/api/resolve", strings.NewReader(`not-json`))
	w := httptest.NewRecorder()
	h.resolveURL(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// TestWithShareTarget 验证 spx_target 参数叠加（已有 query 保留、同键覆盖）。
func TestWithShareTarget(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, target, want string }{
		{"https://mypikpak.com/s/abc", "NHDTA-513A.mp4", "spx_target=NHDTA-513A.mp4"},
		{"https://mypikpak.com/s/abc?x=1", "b.mp4", "spx_target=b.mp4"},
		{"https://keepshare.org/k/magnet:?xt=urn:btih:ab", "m.mp4", "spx_target=m.mp4"},
	}
	for _, c := range cases {
		got := withShareTarget(c.in, c.target)
		if !strings.Contains(got, c.want) {
			t.Fatalf("withShareTarget(%q, %q) = %q, want contains %q", c.in, c.target, got, c.want)
		}
		if !strings.HasPrefix(got, strings.SplitN(c.in, "?", 2)[0]) {
			t.Fatalf("withShareTarget 改变了 URL 路径: %q -> %q", c.in, got)
		}
	}
}
