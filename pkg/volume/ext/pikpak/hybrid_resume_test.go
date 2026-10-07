// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestHybridDownload_ResumeKeepsCompletedChunks 实证：崩溃恢复应保留已完成 chunk 只下缺失部分。
// 当前实现 preallocate（os.Create + Truncate = **O_TRUNC 清空**）在 loadValidManifest **之前**
// 执行；恢复时 manifest 又跳过"已完成"chunk → 已下数据被清零、跳过 → 文件残缺。
// 表现：hash 存在 → 完整性校验失败报错（恢复永远失败）；hash 缺失 → **静默零洞损坏**。
func TestHybridDownload_ResumeKeepsCompletedChunks(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20) // 4MB，chunk 2MB → 2 chunks
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	chunkLen := int64(len(payload) / 2)
	var srvURL string
	var shareDLCalls, driveDLCalls int
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
			"id": "share-f1", "name": "movie.mp4", "hash": payloadSHA1(payload),
			"web_content_link": srvURL + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) { shareDLCalls++; serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{"files": []map[string]any{
				{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
			}})
			return
		}
		http.Error(w, "m", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srvURL + "/drive/dl"})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) { driveDLCalls++; serveRange(w, r, payload) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")

	// ① 模拟"崩溃后"状态：临时文件 dest+".hybrid.downloading" 已含 chunk 0 数据
	// （完成块），manifest 记录 chunk 0 done。**注意**：现在下载期写盘目标已是
	// 临时文件（destPath+".hybrid.downloading"），崩溃恢复也作用在该临时文件上。
	working := dest + ".hybrid.downloading"
	f, _ := os.Create(working)
	_, _ = f.Write(payload[:chunkLen])
	_ = f.Truncate(int64(len(payload)))
	f.Close()
	m := &hybridManifest{Total: int64(len(payload)), ShareEnd: chunkLen, Chunks: map[int64]int64{0: chunkLen}, Source: manifestSource{ShareID: "abc123", FileID: "share-f1", Hash: payloadSHA1(payload), Size: int64(len(payload))}}
	data, _ := json.Marshal(m)
	_ = os.WriteFile(manifestPath(dest), data, 0o644)

	// ② 恢复：理想 = 只下 chunk 1（保留 chunk 0）→ 文件完整。
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("RESUME BROKEN: re-Download errored: %v (shareDL=%d driveDL=%d)", err, shareDLCalls, driveDLCalls)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		first := -1
		for i := range payload {
			if got[i] != payload[i] {
				first = i
				break
			}
		}
		t.Fatalf("RESUME CORRUPT: file != payload, first_diff_offset=%d (shareDL=%d driveDL=%d)", first, shareDLCalls, driveDLCalls)
	}
	// Minor2：恢复效率守卫——已下 chunk0 应被跳过，只补下缺失 chunk1（账号区 driveDL=1）。
	// 若未来被改成「恢复=全量重下」，此处红（完整性守卫在 CORRUPT 分支已覆盖，效率守卫在此）。
	if driveDLCalls != 1 {
		t.Fatalf("RESUME INEFFICIENT: expected exactly 1 driveDL (chunk1 only), got %d (shareDL=%d)", driveDLCalls, shareDLCalls)
	}
}
