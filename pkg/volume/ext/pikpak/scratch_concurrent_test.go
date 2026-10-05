// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestScratch_SharedInstance_ConcurrentTasks 实证：**注册表单例实例**并发跑两个不同分享任务，
// 实例字段 restoredIDs 跨任务污染 → 任务 B 的账号区可能下载到任务 A 的文件。
// 对应生产 wiring：registerPikpakDownloader 单例 + cloud manager 并发任务（无此测试覆盖）。
func TestScratch_SharedInstance_ConcurrentTasks(t *testing.T) {
	t.Parallel()
	payloadA := makeScratchPayload(3, 2<<20) // 2MB
	payloadB := makeScratchPayload(7, 2<<20)
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap"})
	})
	// share 列表按 share_id **精确匹配**——不得对 RawQuery 做子串匹配（query 含随机
	// device_id，hex 子串 "aaa"/"bbb" 会误判路由，round-9 flake 根因）。
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		fid, size := "share-a", len(payloadA)
		switch r.URL.Query().Get("share_id") {
		case "aaa":
			fid, size = "share-a", len(payloadA)
		case "bbb":
			fid, size = "share-b", len(payloadB)
		}
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": fid, "name": "movie.mp4", "size": fmt.Sprint(size)},
		}})
	})
	// 分享直链：按文件 id 返回对应内容（hash 为空 → C2 最终校验被绕过 → 静默损坏可观测）
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		fid := r.URL.Query().Get("file_id")
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": fid, "name": "movie.mp4", "web_content_link": srvURL + "/share/dl/" + fid,
		}})
	})
	mux.HandleFunc("/share/dl/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/share/dl/")
		p := payloadA
		if id == "share-b" {
			p = payloadB
		}
		serveRange(w, r, p)
	})
	// 转存：share-a → restored-a；share-b → restored-b（从 body 解析 share_id）
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		fid := "restored-a"
		var body struct {
			ShareID string `json:"share_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body.ShareID, "bbb") {
			fid = "restored-b"
		}
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": fid})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "m", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"files": []map[string]any{
			{"kind": "drive#file", "id": "restored-a", "name": "movie.mp4", "size": fmt.Sprint(len(payloadA))},
			{"kind": "drive#file", "id": "restored-b", "name": "movie.mp4", "size": fmt.Sprint(len(payloadB))},
		}})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{"id": id, "name": "movie.mp4", "web_content_link": srvURL + "/drive/dl/" + id})
	})
	mux.HandleFunc("/drive/dl/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/dl/")
		p := payloadA
		if id == "restored-b" {
			p = payloadB
		}
		serveRange(w, r, p)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	// 共享同一实例（注册表单例形态）+ 同一 resolver/api/client
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 2, AutoDelete: false,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})

	var wg sync.WaitGroup
	errs := make([]error, 2)
	files := []string{filepath.Join(t.TempDir(), "a.mp4"), filepath.Join(t.TempDir(), "b.mp4")}
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = hd.Download(context.Background(), "https://mypikpak.com/s/aaa", files[0], nil)
	}()
	go func() {
		defer wg.Done()
		_, errs[1] = hd.Download(context.Background(), "https://mypikpak.com/s/bbb", files[1], nil)
	}()
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("task %d error: %v", i, e)
		}
	}
	gotA, _ := os.ReadFile(files[0])
	gotB, _ := os.ReadFile(files[1])
	if string(gotA) != string(payloadA) {
		t.Errorf("FILE A CORRUPTED: len=%d (payloadA=%d) prefix=%q", len(gotA), len(payloadA), gotA[:16])
	}
	if string(gotB) != string(payloadB) {
		t.Errorf("FILE B CORRUPTED: len=%d (payloadB=%d) prefix=%q", len(gotB), len(payloadB), gotB[:16])
	}
}

func makeScratchPayload(seed, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i * seed) % 251)
	}
	return b
}
