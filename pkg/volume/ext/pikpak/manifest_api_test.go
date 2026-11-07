// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// newLimitedRangeServer 构造「匿名只可读 [0,limit)」的 Range 服务器：
// 起点 >= limit 返回 416（模拟 PikPak 匿名边界）。
func newLimitedRangeServer(t *testing.T, payload []byte, limit int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseTestRange(r.Header.Get("Range"), int64(len(payload)))
		if !ok {
			http.Error(w, "range", http.StatusBadRequest)
			return
		}
		if start >= limit {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		if end >= limit {
			end = limit - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(payload)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(payload[start : end+1])
	}))
	t.Cleanup(srv.Close)
	return srv
}

// parseTestRange 解析 "bytes=start-end"（end 越界钳到文件末）。
func parseTestRange(h string, total int64) (start, end int64, ok bool) {
	h = strings.TrimPrefix(strings.TrimSpace(h), "bytes=")
	a, b, found := strings.Cut(h, "-")
	if !found {
		return 0, 0, false
	}
	s, err := strconv.ParseInt(a, 10, 64)
	if err != nil || s < 0 || s >= total {
		return 0, 0, false
	}
	e := total - 1
	if b != "" {
		if v, perr := strconv.ParseInt(b, 10, 64); perr == nil {
			e = min(v, total-1)
		}
	}
	return s, e, true
}

// TestManifestAPI_RoundTrip 覆盖 manifest 摘要/源 URL/续传基准/离线扫描（CLI 逻辑已收敛到此）。
func TestManifestAPI_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.mp4")
	total := int64(8 << 20)
	chunkSize := int64(4 << 20)
	// 构造工作文件：第一个分片正常数据；第二个分片全零（模拟未写完 ⇒ 零尾可疑）
	working := WorkingPath(dest)
	data := make([]byte, total)
	for i := range chunkSize {
		data[i] = 0xAB
	}
	if err := os.WriteFile(working, data, 0o600); err != nil {
		t.Fatalf("write working: %v", err)
	}
	writeManifest(dest, &hybridManifest{
		Total: total, ShareEnd: total / 2,
		Chunks: map[int64]int64{0: chunkSize},
		Source: manifestSource{ShareID: "SHARE-EXAMPLE", FileID: "f-1", Hash: "GCID-EXAMPLE",
			Size: total, URL: "https://example.com/s/EXAMPLE-SHARE"},
	})

	sum, ok := ReadManifestSummary(dest)
	if !ok || sum.Total != total || sum.Done != 1 || sum.Chunks[0] != chunkSize {
		t.Fatalf("ManifestSummary = %+v ok=%v", sum, ok)
	}
	if url, ok := SourceURLFromManifest(dest); !ok || url != "https://example.com/s/EXAMPLE-SHARE" {
		t.Fatalf("SourceURLFromManifest = %q ok=%v", url, ok)
	}
	if got := ResumeBytes(dest); got != chunkSize {
		t.Fatalf("ResumeBytes = %d, want %d", got, chunkSize)
	}
	// 工作文件路径也能解析（CLI 常直接给 *.hybrid.downloading）
	if got := ResumeBytes(working); got != chunkSize {
		t.Fatalf("ResumeBytes(working) = %d", got)
	}

	rep, err := OfflineScan(dest, chunkSize, 64<<10)
	if err != nil {
		t.Fatalf("OfflineScan: %v", err)
	}
	if rep.ChunksTotal != 2 || rep.ChunksTrust != 1 {
		t.Fatalf("扫描统计异常: %+v", rep)
	}
	// 第二片（offset=4MiB）尾部全零 ⇒ 可疑/待重建
	if len(rep.Suspects) != 1 || rep.Suspects[0].Offset != chunkSize {
		t.Fatalf("零尾可疑分片异常: %+v", rep.Suspects)
	}
	if len(rep.Rebuild) != 1 || rep.Rebuild[0].Offset != chunkSize {
		t.Fatalf("待重建分片异常: %+v", rep.Rebuild)
	}
}

// TestStageWorkFiles_NoOverwrite 异常命名文件归位：目标已存在拒绝覆盖（防静默丢数据）。
func TestStageWorkFiles_NoOverwrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mp4")
	data := filepath.Join(dir, "out.error")
	if err := os.WriteFile(data, []byte("partial"), 0o600); err != nil {
		t.Fatalf("write data: %v", err)
	}
	if err := StageWorkFiles(dest, data, ""); err != nil {
		t.Fatalf("StageWorkFiles: %v", err)
	}
	if _, err := os.Stat(WorkingPath(dest)); err != nil {
		t.Fatalf("数据未归位: %v", err)
	}
	// 目标已存在 → 拒绝覆盖
	other := filepath.Join(dir, "out2.error")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatalf("write other: %v", err)
	}
	if err := StageWorkFiles(dest, other, ""); err == nil {
		t.Fatal("目标工作文件已存在时应拒绝覆盖")
	}
}

// TestProbeAnonymousLimit_FullAndPartial 探测能力：整文件可匿名 ⇒ total；有上限 ⇒ 边界。
func TestProbeAnonymousLimit_FullAndPartial(t *testing.T) {
	t.Parallel()
	total := int64(6 << 20)
	for _, c := range []struct {
		name  string
		limit int64
	}{
		{"全量匿名", total},
		{"部分匿名", 3 << 20},
	} {
		limit := c.limit
		payload := make([]byte, total)
		srv := newLimitedRangeServer(t, payload, limit)
		got, err := ProbeAnonymousLimit(t.Context(), srv.Client(), srv.URL, total)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// 二分以 1MB 步进，结果落在 [limit-1MB, limit]
		if got < limit-(1<<20) || got > limit {
			t.Fatalf("%s: probe = %d, want ≈%d", c.name, got, limit)
		}
	}
}

// TestComputeShareEnd_AutoProbe 验证 **auto 模式**（ShareRatio=0，默认）：匿名区上限以探测为准，
// 不再固定按 50% 截断；探测失败才回落 50%。
func TestComputeShareEnd_AutoProbe(t *testing.T) {
	t.Parallel()
	total := int64(6 << 20)
	payload := make([]byte, total)

	// 部分匿名（3MB）：auto 模式 shareEnd ≈ 3MB（而非 50%*6MB=3MB 的巧合值——用 4MB 区分）
	partial := newLimitedRangeServer(t, payload, 4<<20)
	hd := newAutoHybrid(t, partial.Client())
	got := hd.computeShareEnd(t.Context(), &ShareFile{Size: total, DirectLink: partial.URL}, total)
	if got < 3<<20 || got > 4<<20 {
		t.Fatalf("auto 模式 shareEnd = %d, want ≈4MB（探测边界，不是 50%%）", got)
	}

	// 全量匿名：shareEnd = total（不浪费账号配额）
	full := newLimitedRangeServer(t, payload, total)
	hd2 := newAutoHybrid(t, full.Client())
	if got := hd2.computeShareEnd(t.Context(), &ShareFile{Size: total, DirectLink: full.URL}, total); got != total {
		t.Fatalf("全量匿名应 shareEnd=total, got %d", got)
	}

	// 探测失败（非法 URL）→ 回落 50%
	hd3 := newAutoHybrid(t, partial.Client())
	if got := hd3.computeShareEnd(t.Context(), &ShareFile{Size: total, DirectLink: "://bad"}, total); got != total/2 {
		t.Fatalf("探测失败应回落 50%%, got %d", got)
	}
}

// TestComputeShareEnd_ExplicitRatioCaps 显式比例模式：min(探测, ratio×total)（配置即上限）。
func TestComputeShareEnd_ExplicitRatioCaps(t *testing.T) {
	t.Parallel()
	total := int64(8 << 20)
	payload := make([]byte, total)
	full := newLimitedRangeServer(t, payload, total) // 探测=total
	hd, err := NewHybridDownloader(HybridConfig{
		API: NewAPI(APIConfig{}, nil), HTTPClient: full.Client(), ShareRatio: 0.25,
	})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	if got := hd.computeShareEnd(t.Context(), &ShareFile{Size: total, DirectLink: full.URL}, total); got != total/4 {
		t.Fatalf("显式 0.25 应 shareEnd=%d, got %d", total/4, got)
	}
}

// newAutoHybrid 构造 auto 模式（ShareRatio 未设置）的下载器。
func newAutoHybrid(t *testing.T, client *http.Client) *HybridDownloader {
	t.Helper()
	hd, err := NewHybridDownloader(HybridConfig{API: NewAPI(APIConfig{}, nil), HTTPClient: client})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	return hd
}

// TestVerifyLocalFile 本地文件 GCID 比对（下载前「已完成/冲突」判定的领域判据）。
func TestVerifyLocalFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.bin")
	payload := make([]byte, 1<<20)
	for i := range payload {
		payload[i] = byte(i % 253)
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// 计算真实 GCID（与 PikPak 官方算法一致）
	gcid := payloadGCID(payload)
	m, err := VerifyLocalFile(path, gcid)
	if err != nil {
		t.Fatalf("VerifyLocalFile: %v", err)
	}
	if !m.Matched || m.Size != int64(len(payload)) {
		t.Fatalf("应命中: %+v (gcid=%s)", m, gcid)
	}
	// 大小写无关
	upper := strings.ToUpper(gcid)
	if m, err = VerifyLocalFile(path, upper); err != nil || !m.Matched {
		t.Fatalf("大写 GCID 也应命中: %+v err=%v", m, err)
	}
	// 不匹配
	if m, err = VerifyLocalFile(path, "0000000000000000000000000000000000000000"); err != nil || m.Matched {
		t.Fatalf("不同 GCID 不应命中: %+v err=%v", m, err)
	}
	// 无权威 hash：只报告大小
	if m, err = VerifyLocalFile(path, ""); err != nil || m.Matched || m.Size == 0 {
		t.Fatalf("无期望 hash 应只报告大小: %+v err=%v", m, err)
	}
	// 不存在 → 零值
	if m, err = VerifyLocalFile(filepath.Join(dir, "nope.bin"), gcid); err != nil || m.Size != 0 {
		t.Fatalf("不存在应为零值: %+v err=%v", m, err)
	}
}

// TestProbeAnonymousLimit_EdgeCases 边界场景：整文件匿名不可用（offset 0 即 416）、
// 服务端异常状态（500 → 显式错误）、小于 1MB 的小文件。
func TestProbeAnonymousLimit_EdgeCases(t *testing.T) {
	t.Parallel()
	t.Run("整文件不可匿名", func(t *testing.T) {
		t.Parallel()
		total := int64(4 << 20)
		srv := newLimitedRangeServer(t, make([]byte, total), 0) // limit=0 → 全部 416
		got, err := ProbeAnonymousLimit(t.Context(), srv.Client(), srv.URL, total)
		if err != nil {
			t.Fatalf("ProbeAnonymousLimit: %v", err)
		}
		if got != 0 {
			t.Fatalf("不可匿名应返回 0, got %d", got)
		}
	})
	t.Run("服务端异常状态显式报错", func(t *testing.T) {
		t.Parallel()
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		t.Cleanup(srv.Close)
		if _, err := ProbeAnonymousLimit(t.Context(), srv.Client(), srv.URL, 4<<20); err == nil {
			t.Fatal("非 206/416 状态应返回错误（调用方据此回退比例）")
		}
	})
	t.Run("小文件全量匿名", func(t *testing.T) {
		t.Parallel()
		total := int64(512 << 10) // < 1MB 探测步进
		srv := newLimitedRangeServer(t, make([]byte, total), total)
		got, err := ProbeAnonymousLimit(t.Context(), srv.Client(), srv.URL, total)
		if err != nil || got != total {
			t.Fatalf("小文件全量匿名应返回 total, got %d err=%v", got, err)
		}
	})
	t.Run("小文件不可匿名", func(t *testing.T) {
		t.Parallel()
		total := int64(512 << 10)
		srv := newLimitedRangeServer(t, make([]byte, total), 0)
		got, err := ProbeAnonymousLimit(t.Context(), srv.Client(), srv.URL, total)
		if err != nil || got != 0 {
			t.Fatalf("小文件不可匿名应返回 0, got %d err=%v", got, err)
		}
	})
	t.Run("非法入参", func(t *testing.T) {
		t.Parallel()
		if _, err := ProbeAnonymousLimit(t.Context(), nil, "", 4<<20); err == nil {
			t.Fatal("空直链应报错")
		}
		if _, err := ProbeAnonymousLimit(t.Context(), nil, "https://example.com/x", 0); err == nil {
			t.Fatal("total<=0 应报错")
		}
	})
}

// TestHybridDriveFile_SourceTimes 验证 drive 模式把源文件时间写入 Result
// （账号网盘文件同样不得用下载完成时刻冒充 mtime）。
func TestHybridDriveFile_SourceTimes(t *testing.T) {
	t.Parallel()
	payload, _, _, api := newHybridFixture(t, 4<<20)
	hd, err := NewHybridDownloader(HybridConfig{API: api, ChunkSize: 4 << 20, Concurrency: 1})
	if err != nil {
		t.Fatalf("NewHybridDownloader: %v", err)
	}
	created := time.Date(2026, 10, 20, 0, 30, 0, 0, time.UTC)
	modified := time.Date(2026, 10, 21, 1, 0, 0, 0, time.UTC)
	dest := filepath.Join(t.TempDir(), "drive.mp4")
	res, err := hd.DownloadDriveFile(t.Context(), DriveFileRequest{
		FileID: "restored-1", FileName: "movie.mp4", Size: int64(len(payload)),
		DestPath: dest, CreatedTime: created, ModifiedTime: modified,
	})
	if err != nil {
		t.Fatalf("DownloadDriveFile: %v", err)
	}
	if !res.SourceCreatedTime.Equal(created) {
		t.Fatalf("SourceCreatedTime = %v, want %v", res.SourceCreatedTime, created)
	}
	if !res.ModTime.Equal(modified) {
		t.Fatalf("ModTime = %v, want 源 modified %v（不得用完成时刻）", res.ModTime, modified)
	}
	if res.AccountBytes != int64(len(payload)) {
		t.Fatalf("drive 模式全部字节应计账号通道, got %d", res.AccountBytes)
	}
}
