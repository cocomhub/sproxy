// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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

// TestShareResolver_CaptchaRefreshOnStaleness 锁定 round-8：token 超过 2/3 TTL 主动刷新
// （长下载中途 re-resolve 不再用陈旧 token 触发不必要降级）。
func TestShareResolver_CaptchaRefreshOnStaleness(t *testing.T) {
	t.Parallel()
	var initCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		initCalls.Add(1)
		writeJSON(w, map[string]any{"captcha_token": fmt.Sprint("cap-", initCalls.Load()), "expires_in": 3600})
	})
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": "share-f1", "name": "movie.mp4", "size": "100"},
		}})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": "share-f1", "name": "movie.mp4", "web_content_link": "https://dl.example/f",
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})

	if _, err := r.Resolve(context.Background(), "https://mypikpak.com/s/abc123"); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if initCalls.Load() != 1 {
		t.Fatalf("captcha init calls = %d, want 1", initCalls.Load())
	}
	// 模拟 token 已过期（TTL 1h，2/3 = 40min；制造 2h 前获取）
	r.mu.Lock()
	r.captchaFetchedAt = time.Now().Add(-2 * time.Hour)
	r.captchaTTL = time.Hour
	r.mu.Unlock()
	if _, err := r.Resolve(context.Background(), "https://mypikpak.com/s/abc123"); err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if initCalls.Load() != 2 {
		t.Fatalf("stale token should trigger refresh, captcha init calls = %d, want 2", initCalls.Load())
	}
}

// TestShareResolver_RefreshLink 锁定 round-8：轻量重取单文件直链（shareDetail 一次 + 单 file_info，
// 不全量重列）。
func TestShareResolver_RefreshLink(t *testing.T) {
	t.Parallel()
	var shareCalls, fileInfoCalls atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap", "expires_in": 3600})
	})
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		shareCalls.Add(1)
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": "share-f1", "name": "movie.mp4", "size": "100"},
			{"id": "share-f2", "name": "sub.srt", "size": "50"},
		}})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		fileInfoCalls.Add(1)
		fid := r.URL.Query().Get("file_id")
		if fid != "share-f1" {
			t.Errorf("file_info should only query share-f1, got %q", fid)
		}
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": fid, "name": "movie.mp4", "hash": "h1", "web_content_link": "https://dl.example/new",
		}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	r := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})

	link, hash, err := r.RefreshLink(context.Background(), "abc123", "share-f1")
	if err != nil {
		t.Fatalf("RefreshLink: %v", err)
	}
	if link != "https://dl.example/new" || hash != "h1" {
		t.Errorf("link=%q hash=%q", link, hash)
	}
	if shareCalls.Load() != 1 || fileInfoCalls.Load() != 1 {
		t.Errorf("shareDetail=%d fileInfo=%d, want 1/1 (轻量，非全量重列)", shareCalls.Load(), fileInfoCalls.Load())
	}
	// 分享被换（fileID 消失）→ 报错
	if _, _, err := r.RefreshLink(context.Background(), "abc123", "gone-file"); err == nil {
		t.Fatal("gone file should error (share changed)")
	}
}
