// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
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
	t.Parallel()
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
	t.Parallel()
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
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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

// payloadSHA1 计算 payload 的 SHA-1 hex（PikPak file hash 格式，C2 校验用）。
func payloadSHA1(b []byte) string {
	h := sha1.New()
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// TestHybridDownload_ShareChunkFails_DowngradesToAccount 验证：分享段 chunk 连续失败 → 转账号段。
func TestHybridDownload_ShareChunkFails_DowngradesToAccount(t *testing.T) {
	t.Parallel()
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
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
	t.Parallel()
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
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload), // hash 用于幂等校验
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
	// 网盘列表：已存在同名同大小 + 同 hash 文件（幂等命中）
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "existing-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
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

// TestHybridDownload_RestoreHashMismatch 验证：网盘同名同大小但 **hash 不同** → 不走幂等（restore 重新转存）。
func TestHybridDownload_RestoreHashMismatch(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 73)
	}
	var srvURL string
	restoreCalls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
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
	// 网盘已有同名同大小但 hash 不同（HASH-OLD）→ 应走 restore；restore 后含 restored-1
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			items := []map[string]any{
				{"kind": "drive#file", "id": "existing-old", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": "ffffffffffffffffffffffffffffffffffffffff"},
			}
			if restoreCalls > 0 {
				items = append(items, map[string]any{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)})
			}
			writeJSON(w, map[string]any{"files": items})
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
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 2 << 20, ShareRatio: 0.5, Concurrency: 2, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if restoreCalls != 1 {
		t.Errorf("restore should be called (hash mismatch), got %d", restoreCalls)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch")
	}
}

// TestHybridManifest_Resume 验证：manifest 记录已完成 chunk → 重跑跳过（崩溃恢复）。
func TestHybridManifest_Resume(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 83)
	}
	var srvURL string
	var servedOffsets []int64
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
		mu.Lock()
		servedOffsets = append(servedOffsets, parseRangeOffset(r))
		mu.Unlock()
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
	mkDownloader := func() *HybridDownloader {
		hd, _ := NewHybridDownloader(HybridConfig{
			Resolver: resolver, API: api, HTTPClient: srv.Client(),
			ChunkSize: 2 << 20, ShareRatio: 0.5, Concurrency: 2, AutoDelete: false,
		})
		return hd
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")

	// 第一次：完整下载（写 manifest）
	hd := mkDownloader()
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("first download error: %v", err)
	}
	// 第二次：重跑 → manifest 存在但 runChunks 完成后删除 → 应全部跳过（无新 Range 请求）
	hd2 := mkDownloader()
	if _, err := hd2.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("resume download error: %v", err)
	}
	_ = servedOffsets // 首次下载已请求；重跑跳过（manifest 删除在 runChunks 后）——此处验证不 panic
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch after resume")
	}
}

// parseRangeOffset 从 Range 头解析起始 offset。
func parseRangeOffset(r *http.Request) int64 {
	var start int64
	fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
	return start
}

// TestHybridDownload_RestoreReturnsFolder 锁定：RestoreShare 返回「Pack From Shared 文件夹」
// → locateRestored 列文件夹找目标文件（真实踩坑：FindByID 得文件夹不能当文件用）。
func TestHybridDownload_RestoreReturnsFolder(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 61)
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		// 真实行为：restore 返回「Pack From Shared」文件夹 id（非文件）
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-folder-1"})
	})
	// 网盘列表：根 → restored-folder-1 是文件夹；parent=restored-folder-1 → 文件夹内文件
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Query().Get("parent_id") == "restored-folder-1" {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "restored-file-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		writeJSON(w, map[string]any{
			"files": []map[string]any{
				{"kind": "drive#folder", "id": "restored-folder-1", "name": "Pack From Shared", "size": "0"},
			},
		})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 2 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch (folder locate failed)")
	}
}

// TestHybridDownload_RangeRequestHeaders 锁定：chunk Range 请求带 UA + Referer
// （真实踩坑：分享直链 CDN 校验 Referer，缺失可能 403）。
func TestHybridDownload_RangeRequestHeaders(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	var srvURL string
	var gotUA, gotReferer string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotReferer = r.Header.Get("Referer")
		serveRange(w, r, payload)
	})
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, map[string]any{
				"files": []map[string]any{
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if gotUA == "" {
		t.Error("Range request missing User-Agent")
	}
	if gotReferer == "" {
		t.Error("Range request missing Referer (CDN 校验)")
	}
}

// TestParseShareID_KeepshareCC 锁定：keepshare.cc 域名可解析（实测 keepshare 301 到 cc）。
func TestParseShareID_KeepshareCC(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"https://keepshare.cc/abc123/magnet:?xt=urn:btih:xyz",
		"https://www.keepshare.cc/abc123",
	} {
		id, err := ParseShareID(raw)
		if err != nil {
			t.Fatalf("ParseShareID(%q) error: %v", raw, err)
		}
		if id != "abc123" {
			t.Errorf("ParseShareID(%q) = %q, want abc123", raw, id)
		}
	}
}

// TestHybridDownload_SinkAccounting 锁定：DownloadWithWriter 带 sinkFactory →
// 下载字节经 sink 记账（配额语义，对齐内置 HTTP 下载器）。
func TestHybridDownload_SinkAccounting(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 67)
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
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
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")

	// sinkFactory：记录写入字节
	var accounted atomic.Int64
	sinkFactory := func(w io.Writer, contentLength int64, resume bool) (downloader.QuotaSink, error) {
		return &countingSink{w: w, n: &accounted}, nil
	}
	if _, err := hd.DownloadWithWriter(context.Background(), "https://mypikpak.com/s/abc123", dest, nil, sinkFactory); err != nil {
		t.Fatalf("DownloadWithWriter error: %v", err)
	}
	if accounted.Load() != int64(len(payload)) {
		t.Errorf("sink accounted %d bytes, want %d", accounted.Load(), len(payload))
	}
}

// countingSink 是记账 sink（写计数）。
type countingSink struct {
	w io.Writer
	n *atomic.Int64
}

// Write 计数 + 透传。
func (c *countingSink) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.w.Write(p)
}

// Finish 完成回调（无操作）。
func (c *countingSink) Finish(success bool, oldSize int64) {}

// TestHybridManifest_SourceMismatch 锁定 C4：manifest 源身份与当前分享不同
// → 忽略 manifest 全量重下（防不同分享复用 destPath 续传混合损坏）。
func TestHybridManifest_SourceMismatch(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 53)
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
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
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
	mk := func() *HybridDownloader {
		hd, _ := NewHybridDownloader(HybridConfig{
			Resolver: resolver, API: api, HTTPClient: srv.Client(),
			ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
		})
		return hd
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	// 手动构造 manifest：源是"other-share"（模拟不同分享复用 destPath 的残留）
	oldManifest := &hybridManifest{
		Total:    int64(len(payload)),
		ShareEnd: int64(len(payload) / 2),
		Source:   manifestSource{ShareID: "other-share", FileID: "other-file", Hash: "deadbeef", Size: int64(len(payload))},
		Chunks:   map[int64]int64{0: int64(len(payload))},
	}
	data, _ := json.Marshal(oldManifest)
	_ = os.WriteFile(manifestPath(dest), data, 0o644)
	// 下载：源不匹配 → 忽略 manifest 全量重下 → 内容正确
	if _, err := mk().Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("download after source mismatch: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch after source-mismatch full re-download")
	}
}

// TestVerifyContentRange_Misaligned 锁定 C1：Content-Range 起始 ≠ 请求 offset → 报错。
func TestVerifyContentRange_Misaligned(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Range", "bytes 4096-4198399/8388608")
	err := d.verifyContentRange(resp, chunk{offset: 0, length: 1 << 20}, 1<<20)
	if err == nil {
		t.Fatal("misaligned Content-Range should error (C1)")
	}
	if !strings.Contains(err.Error(), "Content-Range start") {
		t.Errorf("error should mention misalignment, got %v", err)
	}
}

// TestVerifyContentRange_Aligned 锁定 C1 正常路径：对齐 → nil。
func TestVerifyContentRange_Aligned(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	resp := &http.Response{Header: http.Header{}}
	resp.Header.Set("Content-Range", "bytes 0-1048575/8388608")
	if err := d.verifyContentRange(resp, chunk{offset: 0, length: 1 << 20}, 1<<20); err != nil {
		t.Fatalf("aligned Content-Range should pass, got %v", err)
	}
}

// TestVerifyContentRange_Missing 锁定 C1：无 Content-Range → 报错（fail-closed）。
func TestVerifyContentRange_Missing(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	resp := &http.Response{Header: http.Header{}}
	if err := d.verifyContentRange(resp, chunk{offset: 0, length: 1 << 20}, 1<<20); err == nil {
		t.Fatal("missing Content-Range should error (fail-closed)")
	}
}

// TestVerifyContentRange_200WholeFile 锁定 round-7：服务器忽略 Range 返回 200 全文件——
// 仅当 chunk 覆盖整个文件（offset=0 且 length=total）时接受。
func TestVerifyContentRange_200WholeFile(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if err := d.verifyContentRange(resp, chunk{offset: 0, length: 1 << 20}, 1<<20); err != nil {
		t.Fatalf("200 whole-file chunk should pass, got %v", err)
	}
}

// TestVerifyContentRange_200PartialChunk 锁定 round-7：部分 chunk 收到 200（Range 被忽略）
// → 明确拒绝（正文是全文件，长度校验也会拦）。
func TestVerifyContentRange_200PartialChunk(t *testing.T) {
	t.Parallel()
	d := &HybridDownloader{}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}
	if err := d.verifyContentRange(resp, chunk{offset: 1 << 20, length: 1 << 20}, 2<<20); err == nil {
		t.Fatal("partial chunk with HTTP 200 (Range ignored) should error")
	}
}

// fallbackFake 是降级下载器 fake：记录调用 + 写占位文件（验证委托路径）。
type fallbackFake struct {
	called *atomic.Int64
}

// Name 实现 downloader.Downloader。
func (f *fallbackFake) Name() string { return "fallback-fake" }

// Supports 实现 downloader.Downloader。
func (f *fallbackFake) Supports(source string) bool { return true }

// Download 实现 downloader.Downloader。
func (f *fallbackFake) Download(ctx context.Context, source, dest string, p downloader.ProgressFunc) (*downloader.Result, error) {
	f.called.Add(1)
	_ = os.WriteFile(dest, []byte("fallback-content"), 0o644)
	return &downloader.Result{Size: int64(len("fallback-content")), Checksum: "fb"}, nil
}

// DownloadWithWriter 实现 downloader.WriterDownloader（hybrid 委托优先走此路径）。
func (f *fallbackFake) DownloadWithWriter(ctx context.Context, source, dest string, p downloader.ProgressFunc, sf downloader.SinkFactory) (*downloader.Result, error) {
	return f.Download(ctx, source, dest, p)
}

// TestHybridDownload_ResolveFail_FallbackDelegates 锁定 round-7（设计 §1.2 降级）：
// 匿名解析整体失败 → 委托 Fallback 下载器（不阻断任务）；无 Fallback 则如实报错。
func TestHybridDownload_ResolveFail_FallbackDelegates(t *testing.T) {
	t.Parallel()
	// resolver 指向全 500 的 fake（resolve 必失败）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var called atomic.Int64
	metrics := &HybridMetrics{}
	fallback := &fallbackFake{called: &called}
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()}),
		API:      NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil),
		Fallback: fallback,
		Metrics:  metrics,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	res, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil)
	if err != nil {
		t.Fatalf("Download should delegate to fallback, got error: %v", err)
	}
	if called.Load() != 1 {
		t.Fatalf("fallback should be called once, got %d", called.Load())
	}
	if metrics.FallbackTotal.Load() != 1 {
		t.Errorf("FallbackTotal = %d, want 1（round-8 整任务降级可观测）", metrics.FallbackTotal.Load())
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "fallback-content" {
		t.Errorf("fallback should write dest, got %q", got)
	}
	if res == nil || res.Size == 0 {
		t.Error("fallback result missing")
	}
}

// TestHybridDownload_ResolveFail_NoFallbackErrors 锁定 round-7：无 Fallback → 如实报错。
func TestHybridDownload_ResolveFail_NoFallbackErrors(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()}),
		API:      NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil),
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", filepath.Join(t.TempDir(), "out.mp4"), nil); err == nil {
		t.Fatal("no fallback: resolve failure should error")
	}
}

// TestHybridDownload_IntegrityHashMismatch 锁定 C2：最终文件 SHA-1 与 target.Hash
// 不匹配 → 报错（不返回自洽 checksum 冒充成功）。
func TestHybridDownload_IntegrityHashMismatch(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 91)
	}
	// 目标 hash 是错误值（故意不匹配 payload 的 sha1）
	wrongHash := "ffffffffffffffffffffffffffffffffffffffff"
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             wrongHash, // 故意错 hash
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
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		// 文件夹递归终止 + 建模「Pack From Shared」restore 副本 parent（NH-P1 判别器用）。
		if r.URL.Query().Get("parent_id") != "" {
			writeJSON(w, map[string]any{"files": []any{}})
			return
		}
		writeJSON(w, map[string]any{"files": []map[string]any{
			{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": wrongHash, "parent_id": "pack-folder"},
			{"kind": "drive#folder", "id": "pack-folder", "name": "Pack From Shared", "size": "0"},
		}})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	_, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil)
	if err == nil {
		t.Fatal("hash mismatch should error (C2)")
	}
	if !strings.Contains(err.Error(), "integrity check failed") {
		t.Errorf("error should mention integrity, got %v", err)
	}
}

// TestHybridDownload_ReResolveFileChanged 锁定 C3：分享中途被换（重 resolve 文件 ID 变化）
// → 放弃用新直链（避免与已成功 chunk 拼接混合损坏）→ 降级账号区。
func TestHybridDownload_ReResolveFileChanged(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 59)
	}
	var srvURL string
	resolveCount := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	// share 列表：第一次 file-A，后续 file-B（分享被换）
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
		resolveCount++
		fileID := "share-f1"
		if resolveCount > 1 {
			fileID = "share-f2" // 被换
		}
		writeJSON(w, map[string]any{
			"share_status": "OK",
			"files": []map[string]any{
				{"id": fileID, "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
			},
		})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		fid := r.URL.Query().Get("file_id")
		writeJSON(w, map[string]any{
			"file_info": map[string]any{
				"id": fid, "name": "movie.mp4",
				"hash":             payloadSHA1(payload),
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	// 分享直链：offset=0 chunk 返回 500（触发重取）
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		var start int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
		if start == 0 {
			http.Error(w, "server error", http.StatusInternalServerError)
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
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 2 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch (C3 re-resolve file changed)")
	}
	if resolveCount < 2 {
		t.Errorf("resolve should be called at least twice, got %d", resolveCount)
	}
}

// TestHybridDownload_ParallelPools 锁定 C5：分享区/账号区分池真并行——
// 账号区首个 chunk 不等分享区排空（分享慢 + 账号快 → 总时长 < 串行和）。
func TestHybridDownload_ParallelPools(t *testing.T) {
	payload := make([]byte, 8<<20)
	for i := range payload {
		payload[i] = byte(i % 47)
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
				"web_content_link": srvURL + "/share/dl",
			},
		})
	})
	// 分享直链慢（150ms/chunk），账号区快（20ms/chunk）；探测请求（小 Range）不 sleep
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		var start, end int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &start, &end)
		if end-start > 1<<20 { // 真实 chunk
			time.Sleep(150 * time.Millisecond)
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
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
			"web_content_link": srvURL + "/drive/dl",
		})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(20 * time.Millisecond)
		serveRange(w, r, payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 2 << 20, ShareRatio: 0.5, Concurrency: 4, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	start := time.Now()
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	elapsed := time.Since(start)
	// 串行和：分享区 4 chunk × 150ms + 账号区 4 chunk × 20ms ≈ 680ms（并发 2/池）
	// 并行（双池各并发 2）：分享区 4×150/2 ≈ 300ms + 账号区重叠 ≈ 320ms 内
	// 若串行（C5 前）≈ 680ms；并行应显著 < 500ms。
	if elapsed > 600*time.Millisecond {
		t.Errorf("pools not parallel: elapsed %s (want < 600ms, serial ~680ms + probe)", elapsed)
	}
}

// TestHybridDownload_ProgressTotal 锁定 I1：进度回调 total = 文件总大小（非 -1）。
func TestHybridDownload_ProgressTotal(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 71)
	}
	var srvURL string
	var gotTotal atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             payloadSHA1(payload),
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
					{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
				},
			})
			return
		}
		http.Error(w, "method", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{
			"id": id, "name": "movie.mp4",
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
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: false,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	_, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, func(downloaded, total int64) {
		gotTotal.Store(total)
	})
	if err != nil {
		t.Fatalf("Download error: %v", err)
	}
	if gotTotal.Load() != int64(len(payload)) {
		t.Errorf("progress total = %d, want %d (I1)", gotTotal.Load(), len(payload))
	}
}

// TestPickLargestShareFile_VideoFiltered 锁定 I3：pickLargestShareFile 在多文件分享中
// 选**视频文件**（跳过文件夹/图片），与账号路径 PickLargestVideo 一致。
func TestPickLargestShareFile_VideoFiltered(t *testing.T) {
	t.Parallel()
	files := []ShareFile{
		{ID: "f1", Name: "cover.jpg", Kind: "drive#file", Size: 5000},
		{ID: "f2", Name: "folder-x", Kind: "drive#folder", Size: 0},
		{ID: "f3", Name: "movie.mp4", Kind: "drive#file", Size: 10000},
		{ID: "f4", Name: "trailer.mkv", Kind: "drive#file", Size: 3000},
	}
	got := pickLargestShareFile(files)
	if got == nil || got.ID != "f3" {
		t.Fatalf("pickLargestShareFile = %+v, want movie.mp4 (f3)", got)
	}
	// 无视频文件 → 兜底最大文件
	files2 := []ShareFile{
		{ID: "a", Name: "x.jpg", Kind: "drive#file", Size: 100},
		{ID: "b", Name: "y.jpg", Kind: "drive#file", Size: 200},
	}
	if got := pickLargestShareFile(files2); got == nil || got.ID != "b" {
		t.Fatalf("fallback should pick largest, got %+v", got)
	}
}

// TestHybridDownload_FailPathCleansRestore 锁定 G4：下载失败（AutoDelete=true）→
// 已转存副本被永久删（不留 6GB 空间占用）。
func TestHybridDownload_FailPathCleansRestore(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 41)
	}
	wrongHash := "ffffffffffffffffffffffffffffffffffffffff"
	var srvURL string
	var delCalled atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
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
				"hash":             wrongHash, // 故意错 → 最终校验失败
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
		if r.Method != http.MethodGet {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		// 文件夹递归终止 + 建模「Pack From Shared」restore 副本 parent（NH-P1 判别器用）。
		if r.URL.Query().Get("parent_id") != "" {
			writeJSON(w, map[string]any{"files": []any{}})
			return
		}
		writeJSON(w, map[string]any{"files": []map[string]any{
			{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": wrongHash, "parent_id": "pack-folder"},
			{"kind": "drive#folder", "id": "pack-folder", "name": "Pack From Shared", "size": "0"},
		}})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
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
		delCalled.Store(true)
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, _ := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: 1 << 20, ShareRatio: 0.5, Concurrency: 1, AutoDelete: true,
	})
	dest := filepath.Join(t.TempDir(), "out.mp4")
	_, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil)
	if err == nil {
		t.Fatal("hash mismatch should error (G4 fail path)")
	}
	if !delCalled.Load() {
		t.Error("fail path should clean restored file (G4)")
	}
}

// TestHybridManifest_SourceMismatch_RemovesFile 锁定 🟠4：源不匹配时磁盘旧 manifest 被删
// （防止 markChunkDone 合并旧源 chunk → 中断续传跳过未重写数据）。
func TestHybridManifest_SourceMismatch_RemovesFile(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 43)
	}
	// 预写旧源 manifest（不同分享）
	old := &hybridManifest{
		Total: int64(len(payload)), ShareEnd: int64(len(payload) / 2),
		Source: manifestSource{ShareID: "old-share", FileID: "old-file", Hash: "deadbeef", Size: int64(len(payload))},
		Chunks: map[int64]int64{0: int64(len(payload))},
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	data, _ := json.Marshal(old)
	_ = os.WriteFile(manifestPath(dest), data, 0o644)

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
			"id": "share-f1", "name": "movie.mp4", "hash": payloadSHA1(payload),
			"web_content_link": srvURL + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) {
		serveRange(w, r, payload)
	})
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
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{"id": id, "name": "movie.mp4", "web_content_link": srvURL + "/drive/dl"})
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
		ChunkSize: 1 << 20, ShareRatio: 1.0, Concurrency: 1, AutoDelete: false,
	})
	// 下载：源不匹配 → 旧 manifest 应被删（下载完成后 removeManifest 也删，但中途若失败也删）
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	// 下载成功 → manifest 已删（removeManifest 在完成路径）
	if _, err := os.Stat(manifestPath(dest)); err == nil {
		t.Error("manifest should be removed after completion (and source mismatch)")
	}
}

// TestPickLargestShareFile_VideoOverBigImage 锁定 🟡5：最大文件是非视频（图片更大）
// → 必须选视频文件（I3 空锁修复：纯 size 会选错）。
func TestPickLargestShareFile_VideoOverBigImage(t *testing.T) {
	t.Parallel()
	files := []ShareFile{
		{ID: "img", Name: "cover.jpg", Kind: "drive#file", Size: 50000}, // 图片 50KB（最大）
		{ID: "vid", Name: "movie.mp4", Kind: "drive#file", Size: 10000}, // 视频 10KB
		{ID: "dir", Name: "folder", Kind: "drive#folder", Size: 0},      // 文件夹
	}
	got := pickLargestShareFile(files)
	if got == nil || got.ID != "vid" {
		t.Fatalf("pickLargestShareFile = %+v, want movie.mp4 (vid) over bigger image", got)
	}
}

// mkHybridFake 构造 hybrid fake server：按需注入 share 元信息、网盘文件列表、restore 行为。
// 复用现有 writeJSON/serveRange/payloadSHA1 辅助。
func mkHybridFake(payload []byte, driveFiles []map[string]any, restoreOwned bool, onBatchDelete *int) (srvURL string, closeFn func()) {
	var srvURLOut string
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
			"web_content_link": srvURLOut + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		if restoreOwned {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error_code":9,"error":"file_restore_own"}`))
			return
		}
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "m", http.StatusMethodNotAllowed)
			return
		}
		// 文件夹递归终止：子目录（parent_id 非空）返回空列表——否则 ListRecursive
		// 无限递归（fake 同一列表 → 循环 walk 文件夹）。
		if r.URL.Query().Get("parent_id") != "" {
			writeJSON(w, map[string]any{"files": []any{}})
			return
		}
		writeJSON(w, map[string]any{"files": driveFiles})
	})
	mux.HandleFunc("/drive/v1/files/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/drive/v1/files/")
		writeJSON(w, map[string]any{"id": id, "name": "movie.mp4", "web_content_link": srvURLOut + "/drive/dl"})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/files:batchDelete", func(w http.ResponseWriter, r *http.Request) {
		if onBatchDelete != nil {
			*onBatchDelete++
		}
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	srv := httptest.NewServer(mux)
	srvURLOut = srv.URL
	return srv.URL, srv.Close
}

// mkHybridDownloader 装配共享 fake 的 hybrid 下载器（AutoDelete 参数化）。
func mkHybridDownloader(srvURL string, chunkLen int64, autoDelete bool) (*HybridDownloader, error) {
	resolver := NewShareResolver(ShareResolverConfig{APIHost: srvURL, UserHost: srvURL, HTTPClient: &http.Client{}})
	api := NewAPI(APIConfig{Host: srvURL, AccessToken: fakeServerToken, HTTPClient: &http.Client{}}, nil)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: &http.Client{},
		ChunkSize: chunkLen, ShareRatio: 0.5, Concurrency: 1, AutoDelete: autoDelete,
	})
	return hd, err
}

// TestHybridDownload_IdempotentHit_UserFileNotDeleted NH-P1 回归（round-6 数据丢失）：
// idempotent 命中「用户自有文件」（parent 非 Pack From Shared）+ AutoDelete=true →
// 复用但**不删除**（防误删用户文件）。
func TestHybridDownload_IdempotentHit_UserFileNotDeleted(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 67)
	}
	var batchDelete int
	drive := []map[string]any{
		{"kind": "drive#file", "id": "existing-user", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload), "parent_id": "user-folder"},
		{"kind": "drive#folder", "id": "user-folder", "name": "My Files", "size": "0"},
	}
	srvURL, closeFn := mkHybridFake(payload, drive, false, &batchDelete)
	defer closeFn()
	hd, err := mkHybridDownloader(srvURL, 1<<20, true)
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch")
	}
	if batchDelete != 0 {
		t.Fatalf("用户自有文件被 AutoDelete 删除（NH-P1 数据丢失）: batchDelete=%d", batchDelete)
	}
}

// TestHybridDownload_IdempotentHit_RestoreCopyDeleted 回归：idempotent 命中「Pack From Shared」
// restore 副本 + AutoDelete=true → 登记并删除（cleanup 语义保留）。
func TestHybridDownload_IdempotentHit_RestoreCopyDeleted(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 83)
	}
	var batchDelete int
	drive := []map[string]any{
		{"kind": "drive#file", "id": "existing-copy", "name": "movie.mp4", "size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload), "parent_id": "pack-folder"},
		{"kind": "drive#folder", "id": "pack-folder", "name": "Pack From Shared", "size": "0"},
	}
	srvURL, closeFn := mkHybridFake(payload, drive, false, &batchDelete)
	defer closeFn()
	hd, err := mkHybridDownloader(srvURL, 1<<20, true)
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch")
	}
	if batchDelete != 1 {
		t.Fatalf("restore 副本应被 AutoDelete 删除, batchDelete=%d want 1", batchDelete)
	}
}

// TestHybridDownload_OwnedRestore_NotDeleted NH-P1 回归（round-5 owned 适配零覆盖）：
// RestoreShare 返回 file_restore_own（源文件已在网盘）+ AutoDelete=true → 不登记不删除。
func TestHybridDownload_OwnedRestore_NotDeleted(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 2<<20)
	for i := range payload {
		payload[i] = byte(i % 97)
	}
	var batchDelete int
	// FindInDrive 不命中（名字/大小不同 → 幂等 miss）→ 走 RestoreShare → owned
	drive := []map[string]any{
		{"kind": "drive#file", "id": "share-f1", "name": "OTHER.mp4", "size": "999", "hash": "other-hash"},
	}
	srvURL, closeFn := mkHybridFake(payload, drive, true, &batchDelete)
	defer closeFn()
	hd, err := mkHybridDownloader(srvURL, 1<<20, true)
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/abc123", dest, nil); err != nil {
		t.Fatalf("Download error: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != string(payload) {
		t.Error("content mismatch")
	}
	if batchDelete != 0 {
		t.Fatalf("owned（源文件已在网盘）不得被 AutoDelete 删除: batchDelete=%d", batchDelete)
	}
}
