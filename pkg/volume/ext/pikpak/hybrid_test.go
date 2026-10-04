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
	"strings"
	"testing"
)

// serveRange 处理 Range 请求：返回 206 指定字节段（fake 下载源）。
func serveRange(w http.ResponseWriter, r *http.Request, payload []byte) {
	rangeHeader := r.Header.Get("Range")
	if rangeHeader == "" {
		w.Header().Set("Content-Length", fmt.Sprint(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
		return
	}
	var start, end int64
	if _, err := fmt.Sscanf(rangeHeader, "bytes=%d-%d", &start, &end); err != nil {
		http.Error(w, "bad range", http.StatusBadRequest)
		return
	}
	if start < 0 || end >= int64(len(payload)) || end < start {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", len(payload)))
		http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
	w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(payload[start : end+1])
}

// TestPlanChunks 验证分片规划（分享区/账号区分界）。
func TestPlanChunks(t *testing.T) {
	// 100MB 文件，chunk 30MB，shareEnd=50MB → 4 chunks
	chunks := planChunks(0, 100<<20, 50<<20, 30<<20)
	if len(chunks) != 4 {
		t.Fatalf("want 4 chunks, got %d", len(chunks))
	}
	if chunks[0].offset != 0 || chunks[0].length != 30<<20 {
		t.Errorf("chunk0 wrong: %+v", chunks[0])
	}
	if chunks[2].offset != 60<<20 || chunks[2].length != 30<<20 {
		t.Errorf("chunk2 (account) wrong: %+v", chunks[2])
	}
	if chunks[3].offset != 90<<20 || chunks[3].length != 10<<20 {
		t.Errorf("chunk3 (tail) wrong: %+v", chunks[3])
	}
}

// TestHybridDownload_ShareAndAccount 端到端：fake 分享直链 + fake 转存网盘直链，
// 验证分片并行下载后文件完整（内容与源一致）。
func TestHybridDownload_ShareAndAccount(t *testing.T) {
	// 8MB 测试数据（chunk 4MB → 2 chunks：分享区 0-4MB + 账号区 4-8MB）
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 251)
	}

	// fake server：所有端点（captcha/share/file_info/restore/FETCH/delete + 两个下载源）
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"share_status": "OK",
			"files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			},
		})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4",
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/restored-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": "restored-1", "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		// FindByID 的 /drive/v1/files/<id>（非 restored-1 也回退这里）
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/files:batchDelete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	// resolver（匿名）
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	// api（转存/FETCH/删除；token 模式）
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)

	metrics := &HybridMetrics{}
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: true, Metrics: metrics,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}

	dest := filepath.Join(t.TempDir(), "out.mp4")
	res, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil)
	if err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if res.Size != int64(len(payload)) {
		t.Errorf("size = %d, want %d", res.Size, len(payload))
	}
	// 内容校验：逐字节与 payload 一致
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("content mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	// 指标：分享区下载了 4MB（前 1 个 chunk）
	if metrics.ShareBytesSaved.Load() != 4<<20 {
		t.Errorf("ShareBytesSaved = %d, want %d", metrics.ShareBytesSaved.Load(), 4<<20)
	}
	if metrics.DowngradeTotal.Load() != 0 {
		t.Errorf("DowngradeTotal = %d, want 0", metrics.DowngradeTotal.Load())
	}
	// checksum 应等于 payload 的 sha256
	if res.Checksum != sha256Hex(payload) {
		t.Errorf("checksum mismatch: %s", res.Checksum)
	}
}

// sha256Hex 计算 payload 的 sha256 hex。
func sha256Hex(b []byte) string {
	h := newSHA256()
	_, _ = h.Write(b)
	return h.Hex()
}

// TestHybridDownload_ShareChunkFails_DowngradesToAccount 验证：分享段 chunk 连续失败 → 转账号段。
func TestHybridDownload_ShareChunkFails_DowngradesToAccount(t *testing.T) {
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 199)
	}
	var srvURL string
	shareChunkCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"share_status": "OK",
			"files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			},
		})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4",
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	// 分享直链：分享区 chunk（offset < 4MB）返回 416（模拟 416 限制）
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		shareChunkCalls++
		if strings.HasPrefix(r.Header.Get("Range"), "bytes=0-") {
			http.Error(w, "range not satisfiable", http.StatusRequestedRangeNotSatisfiable)
			return
		}
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/restored-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": "restored-1", "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		// FindByID 的 /drive/v1/files/<id>（非 restored-1 也回退这里）
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) {
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
	metrics := &HybridMetrics{}
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 2,
		AutoDelete: true, Metrics: metrics,
	})

	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Errorf("content mismatch after downgrade")
	}
	if metrics.DowngradeTotal.Load() != 1 {
		t.Errorf("DowngradeTotal = %d, want 1", metrics.DowngradeTotal.Load())
	}
	if shareChunkCalls < 2 {
		t.Errorf("share chunk should be tried at least twice, got %d calls", shareChunkCalls)
	}
}

// TestHybridDownload_RestoreIdempotent 验证：网盘已有同名同大小转存 → 跳过 RestoreShare（幂等）。
func TestHybridDownload_RestoreIdempotent(t *testing.T) {
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 137)
	}
	var srvURL string
	restoreCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"share_status": "OK",
			"files": []map[string]any{
				{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			},
		})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"file_info": map[string]any{
				"id": "share-f1", "name": "movie.mp4",
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		restoreCalls++
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	// 网盘列表：已存在同名同大小文件（幂等命中）
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "existing-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/existing-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{
			"id": "existing-1", "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 4 << 20, ShareRatio: 0.5, Concurrency: 2, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if restoreCalls != 0 {
		t.Errorf("restore should be skipped (idempotent), got %d restore calls", restoreCalls)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch")
	}
}
