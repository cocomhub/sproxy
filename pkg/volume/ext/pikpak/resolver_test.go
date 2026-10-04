// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCaptchaSign_Deterministic 验证 captcha 签名算法确定性与格式（与 gopeed 扩展
// 的 captchaSign 语义一致：WEB_CLIENT_ID+VERSION+PACKAGE+deviceId+ts 逐轮 MD5）。
func TestCaptchaSign_Deterministic(t *testing.T) {
	t.Parallel()
	dev := "0123456789abcdef0123456789abcdef"
	ts := "1700000000000"
	s1 := captchaSign(dev, ts)
	s2 := captchaSign(dev, ts)
	if s1 != s2 {
		t.Fatalf("captchaSign not deterministic: %s vs %s", s1, s2)
	}
	if !strings.HasPrefix(s1, "1.") || len(s1) != 34 {
		t.Fatalf("captchaSign format wrong: %q (want 1.<32hex>)", s1)
	}
}

// TestCaptchaSign_DiffersByInput 验证输入不同 → 签名不同（防算法塌缩）。
func TestCaptchaSign_DiffersByInput(t *testing.T) {
	t.Parallel()
	a := captchaSign("aaaa", "1000")
	b := captchaSign("bbbb", "1000")
	if a == b {
		t.Fatalf("captchaSign collapsed for different deviceId: %s", a)
	}
}

// TestShareResolver_Resolve 验证匿名分享解析：
// captcha/init（user host）→ share detail（api host）→ file_info（拿直链）。
func TestShareResolver_Resolve(t *testing.T) {
	t.Parallel()
	// fake server：captcha/init + /drive/v1/share + /drive/v1/share/file_info
	var mux http.HandlerFunc = func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-tok-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"share_status": "OK",
				"files": []map[string]any{
					{"kind": "drive#file", "id": "share-file-1", "name": "movie.mp4", "size": "123456"},
				},
			})
		case r.URL.Path == "/drive/v1/share/file_info" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"file_info": map[string]any{
					"id":               "share-file-1",
					"name":             "movie.mp4",
					"size":             "123456",
					"web_content_link": "https://dl.mypikpak.com/download/?fid=abc",
				},
			})
		default:
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
		}
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := NewShareResolver(ShareResolverConfig{
		APIHost:  srv.URL,
		UserHost: srv.URL,
	})
	meta, err := r.Resolve(context.Background(), "https://mypikpak.com/s/abc123/xyz")
	if err != nil {
		t.Fatalf("Resolve error: %v", err)
	}
	if meta.ShareID != "abc123" {
		t.Errorf("share id = %q, want abc123", meta.ShareID)
	}
	if len(meta.Files) != 1 || meta.Files[0].Name != "movie.mp4" || meta.Files[0].Size != 123456 {
		t.Errorf("files wrong: %+v", meta.Files)
	}
	if meta.Files[0].DirectLink == "" {
		t.Errorf("expected direct link, got empty")
	}
	if !strings.HasPrefix(meta.Files[0].DirectLink, "https://dl.mypikpak.com/") {
		t.Errorf("direct link wrong: %q", meta.Files[0].DirectLink)
	}
}
