// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// recoveryHarness 是恢复/韧性测试的 fake PikPak 环境（分享链 + 转存链 + FETCH 直链）。
type recoveryHarness struct {
	payload   []byte
	chunkSize int64
	srv       *httptest.Server
	// fetchFail 是前 N 次 FETCH 链接请求的故障注入（403）；fetchHits 是 FETCH 请求计数。
	fetchFail int32
	fetchHits atomic.Int32
	failedMu  sync.Mutex
	failed    []ChunkInfo
	cancelled []ChunkInfo
}

// newRecoveryHarness 起 fake 服务。hash 为空时使用 payload 的真实 GCID（校验可通过）。
func newRecoveryHarness(t *testing.T, payload []byte, chunkSize int64, fileHash string, fetchFail int32) *recoveryHarness {
	t.Helper()
	h := &recoveryHarness{payload: payload, chunkSize: chunkSize, fetchFail: fetchFail}
	if fileHash == "" {
		fileHash = payloadGCID(payload)
	}
	var srvURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/shield/captcha/init", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"captcha_token": "cap-1"})
	})
	mux.HandleFunc("/drive/v1/share/detail", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"share_status": "OK", "files": []map[string]any{
			{"id": "share-f1", "name": "movie.mp4", "size": fmt.Sprint(len(payload))},
		}})
	})
	mux.HandleFunc("/drive/v1/share/file_info", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"file_info": map[string]any{
			"id": "share-f1", "name": "movie.mp4", "hash": fileHash,
			"web_content_link": srvURL + "/share/dl",
		}})
	})
	mux.HandleFunc("/share/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/share/restore", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "t1", "file_id": "restored-1"})
	})
	mux.HandleFunc("/drive/v1/files", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"files": []map[string]any{
			{"kind": "drive#file", "id": "restored-1", "name": "movie.mp4",
				"size": fmt.Sprint(len(payload)), "hash": payloadSHA1(payload)},
		}})
	})
	mux.HandleFunc("/drive/v1/files/restored-1", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("usage") == "FETCH" {
			n := h.fetchHits.Add(1)
			if h.fetchFail > 0 && n <= h.fetchFail {
				http.Error(w, `{"error":"forbidden"}`, http.StatusForbidden)
				return
			}
		}
		writeJSON(w, map[string]any{"id": "restored-1", "name": "movie.mp4", "web_content_link": srvURL + "/drive/dl"})
	})
	mux.HandleFunc("/drive/dl", func(w http.ResponseWriter, r *http.Request) { serveRange(w, r, payload) })
	mux.HandleFunc("/drive/v1/files:batchDelete", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"task_id": "del-1"})
	})
	h.srv = httptest.NewServer(mux)
	srvURL = h.srv.URL
	t.Cleanup(h.srv.Close)
	return h
}

// newDownloader 基于 harness 装配 HybridDownloader（小退避，测试快）。
func (h *recoveryHarness) newDownloader(t *testing.T, shareRatio float64) *HybridDownloader {
	t.Helper()
	resolver := NewShareResolver(ShareResolverConfig{
		APIHost: h.srv.URL, UserHost: h.srv.URL, HTTPClient: h.srv.Client(), RetryBackoff: time.Millisecond,
	})
	api := NewAPI(APIConfig{
		Host: h.srv.URL, AccessToken: fakeServerToken, HTTPClient: h.srv.Client(),
		RetryBackoff: time.Millisecond, Logger: apiTestLogger(),
	}, nil)
	hd, err := NewHybridDownloader(HybridConfig{
		Resolver: resolver, API: api, HTTPClient: h.srv.Client(),
		ChunkSize: h.chunkSize, ShareRatio: shareRatio, Concurrency: 2,
		ChunkRetryBackoff: time.Millisecond,
		ChunkProgress: func(info ChunkInfo) {
			h.failedMu.Lock()
			defer h.failedMu.Unlock()
			switch info.Phase {
			case "failed":
				h.failed = append(h.failed, info)
			case "cancelled":
				h.cancelled = append(h.cancelled, info)
			}
		},
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	return hd
}

// recoveryPayload 生成测试载荷（4MB，可复现）。
func recoveryPayload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

// TestHybridDownload_AccountLinkTransientRetry 回归（用户实测故障）：
// 账号区取 FETCH 直链偶发失败（403/瞬断）→ 分片级重试（新链接）后成功，
// 不再让「单次取链失败」终止整个下载。
func TestHybridDownload_AccountLinkTransientRetry(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(4 << 20) // 4MB；chunk 1MB → 分享区 2 块 + 账号区 2 块
	h := newRecoveryHarness(t, payload, 1<<20, "", 2)
	hd := h.newDownloader(t, 0.5)
	dest := filepath.Join(t.TempDir(), "out.mp4")

	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); err != nil {
		t.Fatalf("取链瞬断应在分片重试后成功: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("内容不一致: %d bytes", len(got))
	}
	if hits := h.fetchHits.Load(); hits < 3 {
		t.Errorf("FETCH 请求 = %d, want >= 3（含 2 次故障）", hits)
	}
}

// TestHybridDownload_FailFastCancelsSiblings 首个分片永久失败 → 立即取消其余分片
// （不再白耗带宽/配额），返回带 offset 的 *ChunkError 供 CLI 提示续传。
func TestHybridDownload_FailFastCancelsSiblings(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(4 << 20)                  // 4 块：0,1 分享区；2,3 账号区
	h := newRecoveryHarness(t, payload, 1<<20, "", 1000) // FETCH 恒 403
	hd := h.newDownloader(t, 0.5)
	dest := filepath.Join(t.TempDir(), "out.mp4")

	start := time.Now()
	_, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil)
	if err == nil {
		t.Fatal("err = nil, want chunk failure")
	}
	var ce *ChunkError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v (%T), want *ChunkError", err, err)
	}
	if ce.Offset < 2<<20 {
		t.Errorf("失败 offset = %d, want 账号区（>= 2MB）", ce.Offset)
	}
	h.failedMu.Lock()
	failed, cancelled := len(h.failed), len(h.cancelled)
	h.failedMu.Unlock()
	if failed != 1 {
		t.Errorf("failed 分片 = %d, want 1（fail-fast 后其余账号分片不再尝试）", failed)
	}
	_ = cancelled
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("fail-fast 耗时 %v 过长", elapsed)
	}
	// 已完成的分享区分片仍记录在 manifest（可续传），账号区未记入。
	m := loadManifest(dest)
	if m == nil {
		t.Fatal("manifest 应保留（失败路径可续传）")
	}
	for off := range m.Chunks {
		if off >= h.chunkSize*2 {
			t.Errorf("账号区 offset %d 不应被标记完成", off)
		}
	}
}

// TestHybridDownload_ManifestKeptOnVerifyFailure 最终权威校验失败时 manifest 必须保留
// （此前 removeManifest 在 verify 之前 → 校验失败后进度全丢，重跑只能全量重下）。
func TestHybridDownload_ManifestKeptOnVerifyFailure(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(2 << 20)
	h := newRecoveryHarness(t, payload, 1<<20, strings.Repeat("ab", 20), 0) // 故意错误的权威 hash
	hd := h.newDownloader(t, 0.5)
	dest := filepath.Join(t.TempDir(), "out.mp4")

	_, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil)
	if err == nil {
		t.Fatal("err = nil, want integrity failure")
	}
	if !strings.Contains(err.Error(), "gcid") {
		t.Errorf("err = %v, want 含 gcid 校验失败", err)
	}
	m := loadManifest(dest)
	if m == nil {
		t.Fatal("校验失败必须保留 manifest（否则续传信息丢失，只能全量重下）")
	}
	if len(m.Chunks) == 0 {
		t.Error("manifest 应记录已完成分片")
	}
}

// TestLoadValidManifest_ArchivesWhenWorkingFileMissing 「manifest 有记录但工作文件不在」
// （用户实测：数据文件被改名成最终名）→ 归档 manifest 并全量重下，绝不凭 manifest 假装续传。
func TestLoadValidManifest_ArchivesWhenWorkingFileMissing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	target := &ShareFile{ID: "share-f1", Size: 4 << 20, Hash: payloadGCID(recoveryPayload(4 << 20))}
	writeManifestFile(t, dest, &hybridManifest{
		Total: 4 << 20, Chunks: map[int64]int64{0: 1 << 20},
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})
	hd := &HybridDownloader{log: apiTestLogger()}

	if got := hd.loadValidManifest(dest, "EXAMPLE-001123", target); got != nil {
		t.Fatalf("工作文件缺失时不应信任 manifest: %+v", got)
	}
	if _, err := os.Stat(ManifestPath(dest)); !os.IsNotExist(err) {
		t.Error("原 manifest 应被归档（移走），不再当作有效进度")
	}
	if _, err := os.Stat(StaleManifestPath(dest)); err != nil {
		t.Errorf("归档 manifest 应存在: %v", err)
	}
}

// TestLoadValidManifest_KeepsWhenWorkingFileConsistent 工作文件齐备 → 正常续传（不归档）。
func TestLoadValidManifest_KeepsWhenWorkingFileConsistent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	payload := recoveryPayload(4 << 20)
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	writeManifestFile(t, dest, &hybridManifest{
		Total: int64(len(payload)), Chunks: map[int64]int64{0: 1 << 20},
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})
	if err := preallocate(WorkingPath(dest), target.Size); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	// 写入真实数据（否则零尾扫描会判定该块未写完 → 剔除）
	f, err := os.OpenFile(WorkingPath(dest), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open working: %v", err)
	}
	if _, err := f.WriteAt(payload[:1<<20], 0); err != nil {
		t.Fatalf("write working: %v", err)
	}
	_ = f.Close()
	hd := &HybridDownloader{log: apiTestLogger()}

	got := hd.loadValidManifest(dest, "EXAMPLE-001123", target)
	if got == nil || len(got.Chunks) != 1 {
		t.Fatalf("一致的工作文件应保留 manifest: %+v", got)
	}
	if _, err := os.Stat(StaleManifestPath(dest)); !os.IsNotExist(err) {
		t.Error("不应产生归档 manifest")
	}
}

// TestHybridDownload_RenamedFinalFileRedownloadsAll 用户实测状态（dest 已是"成品"但
// manifest 记录残留）：不得凭残留 manifest 假装续传，必须全量重下并产出正确内容。
func TestHybridDownload_RenamedFinalFileRedownloadsAll(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(4 << 20)
	h := newRecoveryHarness(t, payload, 1<<20, "", 0)
	hd := h.newDownloader(t, 0.5)
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	// 残留：dest 存在（内容残缺） + manifest 声称全部完成（但工作文件不在）
	if err := os.WriteFile(dest, payload[:1<<20], 0o644); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	writeManifestFile(t, dest, &hybridManifest{
		Total: int64(len(payload)), Chunks: map[int64]int64{0: 1 << 20, 1: 1 << 20, 2: 1 << 20, 3: 1 << 20},
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})

	if _, err := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read dest: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("全量重下后内容应正确（不得因残留 manifest 跳过缺失分片）")
	}
}

// writeManifestFile 落盘 manifest（测试用）。
func writeManifestFile(t *testing.T, dest string, m *hybridManifest) {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	if err := os.WriteFile(ManifestPath(dest), data, 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
}

// TestHybridDownload_ResumeDropsZeroTailChunks 崩溃/断电后 manifest 可能先于数据落盘：
// 「manifest 记录完成但磁盘是零洞」必须被零尾扫描剔除并补下（否则文件大小正确、内容残缺，
// 且 manifest 恒说完成 → 反复校验失败的死循环）。
func TestHybridDownload_ResumeDropsZeroTailChunks(t *testing.T) {
	t.Parallel()
	payload := recoveryPayload(4 << 20) // 4 块：0,1 分享区；2,3 账号区
	h := newRecoveryHarness(t, payload, 1<<20, "", 0)
	hd := h.newDownloader(t, 0.5)
	dest := filepath.Join(t.TempDir(), "out.mp4")
	// 工作文件：只有 chunk1 有真实数据；chunk0 为空洞（零）
	if err := preallocate(WorkingPath(dest), int64(len(payload))); err != nil {
		t.Fatalf("preallocate: %v", err)
	}
	f, ferr := os.OpenFile(WorkingPath(dest), os.O_WRONLY, 0)
	if ferr != nil {
		t.Fatalf("open: %v", ferr)
	}
	if _, werr := f.WriteAt(payload[1<<20:2<<20], 1<<20); werr != nil {
		t.Fatalf("write chunk1: %v", werr)
	}
	_ = f.Close()
	// 残留 manifest 谎报 chunk0 也完成
	target := &ShareFile{ID: "share-f1", Size: int64(len(payload)), Hash: payloadGCID(payload)}
	writeManifestFile(t, dest, &hybridManifest{
		Total: target.Size, Chunks: map[int64]int64{0: 1 << 20, 1: 1 << 20},
		Source: manifestSource{ShareID: "EXAMPLE-001123", FileID: target.ID, Hash: target.Hash, Size: target.Size},
	})

	if _, derr := hd.Download(context.Background(), "https://mypikpak.com/s/EXAMPLE-001123", dest, nil); derr != nil {
		t.Fatalf("零尾分片应被剔除补下: %v", derr)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != string(payload) {
		t.Fatalf("内容应完整: err=%v", err)
	}
}
