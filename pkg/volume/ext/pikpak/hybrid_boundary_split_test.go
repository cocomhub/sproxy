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
	"slices"
	"sync"
	"testing"
)

// TestHybridDownload_BoundarySplitWithinChunk 锁定「分享/账号边界可落在 chunk 内部」
// （用户 2026-11-26）：一个 chunk 跨越 shareEnd 时必须拆成两段——前半走分享直链、
// 后半走账号直链，且**分享优先**（分享能下的字节不交给账号）。
func TestHybridDownload_BoundarySplitWithinChunk(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20) // 4MiB，chunk=4MiB（单块跨越 2MiB 边界）
	for i := range payload {
		payload[i] = byte(i % 83)
	}
	var mu sync.Mutex
	var shareRanges, acctRanges []string
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
		}})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": "share-f1", "name": "movie.mp4", "hash": payloadGCID(payload),
			"web_content_link": srvURL + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		shareRanges = append(shareRanges, r.Header.Get("Range"))
		mu.Unlock()
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			}})
			return
		}
		http.Error(w, "m", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srvURL + "/drive/dl"})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		acctRanges = append(acctRanges, r.Header.Get("Range"))
		mu.Unlock()
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/files:batchDelete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: int64(len(payload)), ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	out, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read dest: %v", rerr)
	}
	if string(out) != string(payload) {
		t.Fatal("内容与源不一致（边界拆分写错偏移）")
	}
	mu.Lock()
	gotShare := append([]string(nil), shareRanges...)
	gotAcct := append([]string(nil), acctRanges...)
	mu.Unlock()

	// 分享段必须覆盖 [0, 2MiB)，账号段必须覆盖 [2MiB, 4MiB)——**分享优先**。
	wantShare := fmt.Sprintf("bytes=0-%d", (2<<20)-1)
	wantAcct := fmt.Sprintf("bytes=%d-%d", 2<<20, (4<<20)-1)
	if !containsStr(gotShare, wantShare) {
		t.Fatalf("分享段未按边界拆分：want %q in %v", wantShare, gotShare)
	}
	if !containsStr(gotAcct, wantAcct) {
		t.Fatalf("账号段未按边界拆分：want %q in %v", wantAcct, gotAcct)
	}
	// 反向断言：整块 4MiB 不得整体走账号（否则边界拆分没生效）。
	if containsStr(gotAcct, fmt.Sprintf("bytes=0-%d", (4<<20)-1)) {
		t.Fatalf("整块被交给账号，边界拆分失效: %v", gotAcct)
	}
}

// containsStr 判断切片是否包含目标字符串（测试辅助）。
func containsStr(list []string, want string) bool {
	return slices.Contains(list, want)
}
