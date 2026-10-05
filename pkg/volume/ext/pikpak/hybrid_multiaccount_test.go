// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// TestHybridDownload_MultiAccountSharding 多账号分片：账号区 chunk 经 Select round-robin
// 分摊到 N 账号，每账号只转存一次（accounted 去重），直链并行下载，文件完整。
//
// 断言核心：
//  1. restore 调用次数 == 账号数（每账号一次转存副本）
//  2. 文件内容与 payload 一致（多账号分片不损坏）
//  3. 单账号路径（pool=nil）零回归（现有测试覆盖）
func TestHybridDownload_MultiAccountSharding(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 8<<20) // 8MB
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	chunkLen := int64(len(payload) / 4) // 4 chunks × 2MB → 分享区 2 + 账号区 2

	var restoreCalls atomic.Int64
	var dlCalls atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap-1", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl":
			dlCalls.Add(1)
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/files/restored-1" || (len(r.URL.Path) > 20 && r.URL.Path[:21] == "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/drive/dl"})
		case r.URL.Path == "/drive/dl":
			dlCalls.Add(1)
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/files:batchDelete":
			writeJSON(w, map[string]any{"task_id": "del-1"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)

	// 2 账号池（quota 足够放 8MB；fake secrets + 临时凭据目录）
	now := time.Now()
	pool, sec := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-a", `{"access_token":"tok-a"}`, 1<<30)
	addAcct(t, pool, "acct-b", `{"access_token":"tok-b"}`, 1<<30)
	_ = sec

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 4,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, dlErr := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); dlErr != nil {
		t.Fatalf("Download: %v", dlErr)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("content mismatch: got %d want %d bytes", len(got), len(payload))
	}
	// 多账号：账号区 2 chunk 分摊到 2 账号 → restore 恰好 2 次（每账号一次副本）
	if restoreCalls.Load() != 2 {
		t.Fatalf("restore calls = %d, want 2 (each account restores once)", restoreCalls.Load())
	}
	// 关键差异断言：多账号版**每账号记账**（RecordUsage）——变异回单账号路径时
	// 账号池用量全 0（不经过 pool），此处红。
	acctA, aerr := pool.findLocked("acct-a")
	acctB, berr := pool.findLocked("acct-b")
	if aerr == nil && berr == nil {
		if acctA.DailyUsed == 0 || acctB.DailyUsed == 0 {
			t.Fatalf("multi-account usage not recorded: acct-a=%d acct-b=%d (单账号路径不记账 → 变异)", acctA.DailyUsed, acctB.DailyUsed)
		}
	} else {
		t.Fatalf("pool accounts not found: %v %v", aerr, berr)
	}
	// 下载请求 = 分享区 2 + 账号区 2 = 4（Range 下载）
	if dlCalls.Load() < 4 {
		t.Fatalf("dl calls = %d, want >=4 (2 share + 2 account chunks)", dlCalls.Load())
	}
}

// TestHybridDownload_MultiAccountSingleAccountFallback 账号池只有 1 个账号（或配额不足）时
// 退化为单账号行为：仍能完成下载。
func TestHybridDownload_MultiAccountSingleAccountFallback(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 31)
	}
	chunkLen := int64(len(payload) / 2) // 2 chunks → 分享区 1 + 账号区 1

	var restoreCalls atomic.Int64
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/shield/captcha/init":
			writeJSON(w, map[string]any{"captcha_token": "cap", "expires_in": 3600})
		case r.URL.Path == "/drive/v1/share/detail":
			writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/share/file_info":
			writeJSON(w, map[string]any{"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4", "web_content_link": srv.URL + "/share/dl",
			}})
		case r.URL.Path == "/share/dl" || r.URL.Path == "/drive/dl":
			serveRange(w, r, payload)
		case r.URL.Path == "/drive/v1/share/restore":
			restoreCalls.Add(1)
			writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
		case r.URL.Path == "/drive/v1/files":
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
		case r.URL.Path == "/drive/v1/files/restored-1" || (len(r.URL.Path) > 20 && r.URL.Path[:21] == "/drive/v1/files/restored"):
			writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srv.URL + "/drive/dl"})
		default:
			http.Error(w, "unexpected "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	now := time.Now()
	pool, _ := newTestPool(t, 1<<30, &now)
	addAcct(t, pool, "acct-only", `{"access_token":"tok"}`, 1<<30)

	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: false, AccountPool: pool,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Fatalf("content mismatch")
	}
	if restoreCalls.Load() != 1 {
		t.Fatalf("restore calls = %d, want 1 (single account restores once)", restoreCalls.Load())
	}
}
