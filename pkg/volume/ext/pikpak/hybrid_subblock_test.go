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
	"sync"
	"testing"
)

// TestAdaptiveChunkSize 锁定「chunk 大小自适应：2^k MiB 且总行数 ≤20」（用户 2026-11-26）。
func TestAdaptiveChunkSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		total int64
		want  int64
	}{
		{"极小文件取下限 2MiB", 3 << 20, 2 << 20},
		{"8MiB→2MiB(4 行)", 8 << 20, 2 << 20},
		{"40MiB→2MiB(20 行)", 40 << 20, 2 << 20},
		{"41MiB→4MiB(11 行)", 41 << 20, 4 << 20},
		{"100MiB→8MiB(13 行)", 100 << 20, 8 << 20},
		{"1GiB→64MiB(16 行)", 1 << 30, 64 << 20},
		{"超大文件封顶 64MiB", 100 << 30, 64 << 20},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := adaptiveChunkSize(c.total)
			if got != c.want {
				t.Fatalf("adaptiveChunkSize(%d) = %d, want %d", c.total, got, c.want)
			}
			if got&(got-1) != 0 {
				t.Fatalf("%d 不是 2 的幂", got)
			}
			if rows := (c.total + got - 1) / got; rows > hybridMaxChunkRows && got < hybridMaxChunkSize {
				t.Fatalf("行数 %d 超过上限 %d（chunk=%d）", rows, hybridMaxChunkRows, got)
			}
		})
	}
}

// TestSubRuns_MergesContiguous 锁定「未完成子块按 1MiB 聚合为连续区间」（减少 Range 请求数）。
func TestSubRuns_MergesContiguous(t *testing.T) {
	t.Parallel()
	c := chunk{offset: 10 << 20, length: 8 << 20} // 8 个子块
	// 已完成 0,1,2 与 4,5；缺失 3 与 6,7 → 两个区间
	mask := uint64(0b0011_0111)
	runs := subRuns(c, mask)
	if len(runs) != 2 {
		t.Fatalf("want 2 runs, got %d: %+v", len(runs), runs)
	}
	if runs[0].offset != 13<<20 || runs[0].length != 1<<20 {
		t.Errorf("run0 = %+v, want offset 13MiB len 1MiB", runs[0])
	}
	if runs[1].offset != 16<<20 || runs[1].length != 2<<20 {
		t.Errorf("run1 = %+v, want offset 16MiB len 2MiB", runs[1])
	}
	// 全部完成 ⇒ 无区间；尾块不足 1MiB 按实际长度。
	if got := subRuns(c, 0xFF); len(got) != 0 {
		t.Errorf("全完成应无区间, got %+v", got)
	}
	tail := chunk{offset: 0, length: 1<<20 + 512}
	got := subRuns(tail, 0b01)
	if len(got) != 1 || got[0].offset != 1<<20 || got[0].length != 512 {
		t.Errorf("尾块区间 = %+v, want offset 1MiB len 512", got)
	}
}

// TestHybridDownload_ResumeOnlyMissingSubBlocks 端到端锁定「chunk 内只补缺失 1MiB 子块」：
// 预置 manifest 位图（1..3 已完成）+ 工作文件已含对应字节 ⇒ 重下只请求缺失的第 0 块。
func TestHybridDownload_ResumeOnlyMissingSubBlocks(t *testing.T) {
	t.Parallel()
	payload := make([]byte, 4<<20)
	for i := range payload {
		payload[i] = byte(i % 89)
	}
	var mu sync.Mutex
	var ranges []string
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
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		serveRange(w, r, payload)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	srvURL = srv.URL

	resolver := NewShareResolver(ShareResolverConfig{APIHost: srv.URL, UserHost: srv.URL, HTTPClient: srv.Client()})
	api := NewAPI(APIConfig{Host: srv.URL, AccessToken: fakeServerToken, HTTPClient: srv.Client()}, nil)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: srv.Client(),
		ChunkSize: int64(len(payload)), ShareRatio: 1.0, Concurrency: 1, AutoDelete: false,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "out.mp4")
	working := WorkingPath(dest)

	// 预置「中断状态」：工作文件已含 1MiB..4MiB，manifest 位图标记子块 1/2/3 完成。
	f, err := os.Create(working)
	if err != nil {
		t.Fatalf("create working: %v", err)
	}
	if _, werr := f.WriteAt(payload[1<<20:], 1<<20); werr != nil {
		t.Fatalf("seed working: %v", werr)
	}
	if terr := f.Truncate(int64(len(payload))); terr != nil {
		t.Fatalf("truncate: %v", terr)
	}
	f.Close()
	m := &hybridManifest{
		Total: int64(len(payload)), ShareEnd: int64(len(payload)),
		ChunkSize: int64(len(payload)),
		Chunks:    map[int64]int64{},
		Blocks:    map[int64]uint64{0: 0b1110}, // 子块 1,2,3 完成；0 缺失
		Source: manifestSource{
			ShareID: "EXAMPLE-001123", FileID: "share-f1",
			Hash: payloadGCID(payload), Size: int64(len(payload)),
		},
	}
	data, _ := json.Marshal(m)
	if werr := os.WriteFile(ManifestPath(dest), data, 0o644); werr != nil {
		t.Fatalf("write manifest: %v", werr)
	}

	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); err != nil {
		t.Fatalf("resume Download: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), ranges...)
	mu.Unlock()
	// 只允许请求缺失的第 0 子块（探测请求可能另有一条，但**不得**请求已完成的 1..3 块）。
	for _, rg := range got {
		if rg == "bytes=1048576-4194303" || rg == "bytes=1048576-2097151" {
			t.Fatalf("已完成的子块被重下: %v (all=%v)", rg, got)
		}
	}
	found := false
	for _, rg := range got {
		if rg == "bytes=0-1048575" {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺失子块未被请求（应请求 bytes=0-1048575）: %v", got)
	}
	out, rerr := os.ReadFile(dest)
	if rerr != nil {
		t.Fatalf("read dest: %v", rerr)
	}
	if string(out) != string(payload) {
		t.Fatalf("续传后内容不一致（%d vs %d 字节）", len(out), len(payload))
	}
}
