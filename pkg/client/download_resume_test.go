// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// 本文件覆盖「弱网下载大文件」三处缺陷的回归：
//  1. 单流续传（Range）：失败保留断点 + sidecar，重入/重试从断点继续；
//  2. 分块续传：已完成分块位图落盘，重入跳过已完成块；
//  3. 客户端级整请求 Timeout（默认 300s）不得掐断长下载（WithNoClientTimeout）。

const testResumeMTime = int64(1700000000000000000)

// writeResumeHead 让 mock 服务端在 HEAD /api/files/stat 返回远端文件身份。
func writeResumeHead(w http.ResponseWriter, content []byte, sum string) {
	w.Header().Set("X-File-Size", strconv.Itoa(len(content)))
	w.Header().Set("X-File-Checksum", sum)
	w.Header().Set("X-File-MTime", strconv.FormatInt(testResumeMTime, 10))
	w.WriteHeader(http.StatusOK)
}

// serveRangeContent 按 Range 头返回 content（无 Range ⇒ 200 全量；有 ⇒ 206 从 start 起）。
func serveRangeContent(w http.ResponseWriter, r *http.Request, content []byte, sum string) {
	w.Header().Set("X-File-Checksum", sum)
	raw := strings.TrimSpace(r.Header.Get("Range"))
	if raw == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
		return
	}
	start := parseRangeStart(raw)
	if start < 0 || start >= int64(len(content)) {
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	body := content[start:]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(content)-1, len(content)))
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(body)
}

// parseRangeStart 解析 `bytes=N-` 的起始偏移（非法返回 -1）。
func parseRangeStart(h string) int64 {
	raw, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return -1
	}
	startS, _, _ := strings.Cut(raw, "-")
	n, err := strconv.ParseInt(strings.TrimSpace(startS), 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// resumeTestContent 生成可复现的测试内容（非全零，避免零窗兑底干扰）。
func resumeTestContent(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%251) + 1
	}
	return b
}

// newStreamResumeServer 构造单流续传 mock：HEAD stat + GET /download（Range）。
// recordRanges 记录收到的 Range 头；truncateFirst=true 时首次 GET 只写一半后中断。
func newStreamResumeServer(t *testing.T, content []byte, recordRanges *[]string, mu *sync.Mutex, truncateFirst *atomic.Bool,
	gets *atomic.Int32) *httptest.Server {
	t.Helper()
	sum := sha256hex(content)
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		writeResumeHead(w, content, sum)
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, r *http.Request) {
		if recordRanges != nil {
			mu.Lock()
			*recordRanges = append(*recordRanges, r.Header.Get("Range"))
			mu.Unlock()
		}
		if gets != nil {
			gets.Add(1)
		}
		if truncateFirst != nil && gets != nil && gets.Load() == 1 && truncateFirst.Load() {
			// 声明全长却只写一半 ⇒ 客户端读到 unexpected EOF（弱网断流）。
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content[:len(content)/2])
			return
		}
		serveRangeContent(w, r, content, sum)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

// TestDownloadResumable_ContinuesFromPartial 验证存在部分文件 + sidecar 时，从断点用
// Range 续传，且成功后删除 sidecar。
func TestDownloadResumable_ContinuesFromPartial(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(3000)
	sum := sha256hex(content)
	var ranges []string
	var mu sync.Mutex
	ts := newStreamResumeServer(t, content, &ranges, &mu, nil, nil)

	out := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(out, content[:1000], 0o644); err != nil {
		t.Fatal(err)
	}
	st := downloadResumeState{
		Version: downloadResumeVersion, Filename: "f.bin", Size: int64(len(content)),
		Checksum: sum, MTime: testResumeMTime, Offset: 1000,
	}
	if err := writeDownloadResumeState(out, &st); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err != nil {
		t.Fatalf("DownloadResumable: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("续传后内容不符：got %d bytes, want %d", len(got), len(content))
	}
	mu.Lock()
	first := ""
	if len(ranges) > 0 {
		first = ranges[0]
	}
	mu.Unlock()
	if first != "bytes=1000-" {
		t.Fatalf("首个请求应带 Range bytes=1000-，实际 %q（全部 %v）", first, ranges)
	}
	if _, serr := os.Stat(downloadResumePath(out)); !os.IsNotExist(serr) {
		t.Fatalf("成功后应删除 sidecar，stat err=%v", serr)
	}
}

// TestDownloadResumable_RetriesTruncatedBody 验证单次调用内遇断流后自动重试并从断点续传。
func TestDownloadResumable_RetriesTruncatedBody(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(4000)
	var ranges []string
	var mu sync.Mutex
	var truncate atomic.Bool
	truncate.Store(true)
	var gets atomic.Int32
	ts := newStreamResumeServer(t, content, &ranges, &mu, &truncate, &gets)

	out := filepath.Join(t.TempDir(), "f.bin")
	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err != nil {
		t.Fatalf("DownloadResumable 应自动重试成功，得到: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("重试续传后内容不符：got %d bytes, want %d", len(got), len(content))
	}
	if gets.Load() < 2 {
		t.Fatalf("应至少发出 2 次 GET（断流 + 续传），实际 %d", gets.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) < 2 || ranges[1] != "bytes=2000-" {
		t.Fatalf("第二次请求应从断点（2000）续传，实际 %v", ranges)
	}
}

// TestDownloadResumable_MismatchRestarts 验证 sidecar 与远端不一致时从头下载（不拼接）。
func TestDownloadResumable_MismatchRestarts(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(2500)
	var ranges []string
	var mu sync.Mutex
	ts := newStreamResumeServer(t, content, &ranges, &mu, nil, nil)

	out := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(out, resumeTestContent(500), 0o644); err != nil {
		t.Fatal(err)
	}
	// checksum 与远端不符 ⇒ 必须判为陈旧并从头下载。
	st := downloadResumeState{
		Version: downloadResumeVersion, Filename: "f.bin", Size: int64(len(content)),
		Checksum: strings.Repeat("0", 64), MTime: testResumeMTime, Offset: 500,
	}
	if err := writeDownloadResumeState(out, &st); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err != nil {
		t.Fatalf("DownloadResumable: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("陈旧断点应触发全量重下：got %d bytes, want %d", len(got), len(content))
	}
	mu.Lock()
	first := ""
	if len(ranges) > 0 {
		first = ranges[0]
	}
	mu.Unlock()
	if first != "" {
		t.Fatalf("陈旧断点首个请求不应带 Range，实际 %q", first)
	}
}

// TestChunkedDownloadResumable_SkipsCompletedChunks 验证分块续传跳过已完成块并复用位图。
func TestChunkedDownloadResumable_SkipsCompletedChunks(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(3000)
	sum := sha256hex(content)
	const chunkSize = 1000
	var offsets []string
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		writeResumeHead(w, content, sum)
	})
	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		off := r.URL.Query().Get("offset")
		lengthStr := r.URL.Query().Get("length")
		mu.Lock()
		offsets = append(offsets, off)
		mu.Unlock()
		start, _ := strconv.ParseInt(off, 10, 64)
		length, _ := strconv.ParseInt(lengthStr, 10, 64)
		if start >= int64(len(content)) {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		end := min(start+length, int64(len(content)))
		body := content[start:end]
		chunkHash := sha256.Sum256(body)
		w.Header().Set("X-Chunk-Checksum", hex.EncodeToString(chunkHash[:]))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	out := filepath.Join(t.TempDir(), "f.bin")
	// 预置：文件已预分配到全长，第 0 块已写入正确内容。
	f, cerr := os.Create(out)
	if cerr != nil {
		t.Fatal(cerr)
	}
	if err := f.Truncate(int64(len(content))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(content[:chunkSize], 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	st := downloadResumeState{
		Version: downloadResumeVersion, Filename: "f.bin", Size: int64(len(content)),
		Checksum: sum, MTime: testResumeMTime, ChunkSize: chunkSize,
		Completed: encodeChunkBitmap([]bool{true, false, false}),
	}
	if err := writeDownloadResumeState(out, &st); err != nil {
		t.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.ChunkedDownload(t.Context(), "f.bin", out,
		WithChunkedChunkSize(chunkSize), WithChunkedResume(true)); err != nil {
		t.Fatalf("ChunkedDownload 续传: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("分块续传后内容不符：got %d bytes, want %d", len(got), len(content))
	}
	mu.Lock()
	defer mu.Unlock()
	seen := map[string]bool{}
	for _, o := range offsets {
		seen[o] = true
	}
	if seen["0"] {
		t.Fatalf("不应重复下载已完成块 0，实际 offsets=%v", offsets)
	}
	if !seen["1000"] || !seen["2000"] {
		t.Fatalf("应下载剩余块 1000/2000，实际 offsets=%v", offsets)
	}
	if _, serr := os.Stat(downloadResumePath(out)); !os.IsNotExist(serr) {
		t.Fatalf("成功后应删除 sidecar，stat err=%v", serr)
	}
}

// TestDownloadResumable_IgnoresClientLevelTimeout 验证下载请求不受 http.Client.Timeout
// （默认 300s）约束——否则弱网大文件必然在中途被掐断（decrypt stream: unexpected EOF）。
func TestDownloadResumable_IgnoresClientLevelTimeout(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(64)
	sum := sha256hex(content)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	var gets atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		writeResumeHead(w, content, sum)
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, _ *http.Request) {
		gets.Add(1)
		w.Header().Set("X-File-Checksum", sum)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-release // 响应体挂起，直到测试确认已超过客户端级 Timeout
		_, _ = w.Write(content)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	out := filepath.Join(t.TempDir(), "f.bin")
	c := NewFileClient(ts.URL, WithTimeout(50*time.Millisecond))
	c.logger = testLogger()
	done := make(chan error, 1)
	go func() { done <- c.DownloadResumable(t.Context(), "f.bin", out) }()
	<-started
	// 持续超过客户端级 Timeout（50ms）后再放行：若下载尊重它，早已中途失败。
	start := time.Now()
	testutil.WaitFor(t, 5*time.Second, func() bool { return time.Since(start) >= 200*time.Millisecond },
		"等待超过客户端级超时窗口")
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("下载不应受客户端级 Timeout 影响，得到: %v", err)
	}
	// 关键断言：若首请求被客户端级 Timeout 提前掐断，续传逻辑会再发一次（gets≥2）。
	// 只有在「长下载不受该 Timeout 约束」时才会恰好 1 次。
	if n := gets.Load(); n != 1 {
		t.Fatalf("首请求应一次读完（不受 Timeout 影响），实际发出 %d 次 GET", n)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("慢响应下载内容不符")
	}
}

// TestChunkedDownloadResumable_KeepsSidecarOnFailureThenResumes 验证真实失败后保留
// 部分文件 + sidecar（含已完成位图），下一次调用可从断点续传完成。
func TestChunkedDownloadResumable_KeepsSidecarOnFailureThenResumes(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(2000)
	sum := sha256hex(content)
	const chunkSize = 1000
	var healthy atomic.Bool // false = 分块接口故障；true = 正常
	var offset0Requests atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		writeResumeHead(w, content, sum)
	})
	mux.HandleFunc("GET /download/chunk", func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			// 仅块 0 可用（让部分进度落盘），块 1 恒 500 ⇒ 首轮失败但保留断点。
			if r.URL.Query().Get("offset") != "0" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		off := r.URL.Query().Get("offset")
		if off == "0" {
			offset0Requests.Add(1)
		}
		start, _ := strconv.ParseInt(off, 10, 64)
		length, _ := strconv.ParseInt(r.URL.Query().Get("length"), 10, 64)
		end := min(start+length, int64(len(content)))
		body := content[start:end]
		chunkHash := sha256.Sum256(body)
		w.Header().Set("X-Chunk-Checksum", hex.EncodeToString(chunkHash[:]))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	out := filepath.Join(t.TempDir(), "f.bin")
	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	opts := []ChunkedOption{WithChunkedChunkSize(chunkSize), WithChunkedResume(true), WithChunkedConcurrency(1)}

	if err := c.ChunkedDownload(t.Context(), "f.bin", out, opts...); err == nil {
		t.Fatal("首轮应因块 1 持续失败而报错")
	}
	// 失败后必须保留 sidecar + 部分文件（可续传）。
	if _, serr := os.Stat(downloadResumePath(out)); serr != nil {
		t.Fatalf("失败后应保留 sidecar 供续传，stat err=%v", serr)
	}
	fi, serr := os.Stat(out)
	if serr != nil || fi.Size() != int64(len(content)) {
		t.Fatalf("失败后应保留预分配的部分文件，size err=%v", serr)
	}

	// 恢复网络：再次调用应从断点续传（块 0 不再请求）并完成。
	healthy.Store(true)
	if err := c.ChunkedDownload(t.Context(), "f.bin", out, opts...); err != nil {
		t.Fatalf("续传应成功: %v", err)
	}
	got, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("续传后内容不符")
	}
	if offset0Requests.Load() != 1 {
		t.Fatalf("块 0 只应在首轮成功下载一次，实际请求 %d 次", offset0Requests.Load())
	}
	if _, serr := os.Stat(downloadResumePath(out)); !os.IsNotExist(serr) {
		t.Fatalf("成功后应删除 sidecar")
	}
}

// TestDownloadResumable_FatalStatusKeepsExistingFile 验证在 GET 致命失败（4xx）时不会
// 预先删除既有输出文件（此前的实现会在请求前就截断/删除）。
func TestDownloadResumable_FatalStatusKeepsExistingFile(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(128)
	sum := sha256hex(content)
	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /api/files/stat", func(w http.ResponseWriter, _ *http.Request) {
		writeResumeHead(w, content, sum)
	})
	mux.HandleFunc("GET /download", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // 文件在 HEAD 后被删的竞态
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	out := filepath.Join(t.TempDir(), "f.bin")
	const existing = "OLD-COMPLETE-FILE"
	if err := os.WriteFile(out, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err == nil {
		t.Fatal("GET 404 应返回错误")
	}
	got, rerr := os.ReadFile(out)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != existing {
		t.Fatalf("致命失败不应破坏既有文件：got %q, want %q", string(got), existing)
	}
}

// midStreamProbeReader 在读到指定剩余量时触发一次探测（模拟「流进行中」观察磁盘状态）。
type midStreamProbeReader struct {
	remaining int64
	probeAt   int64
	probed    bool
	probe     func()
}

func (r *midStreamProbeReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := min(int64(len(p)), r.remaining)
	for i := range n {
		p[i] = 1
	}
	r.remaining -= n
	if !r.probed && r.remaining <= r.probeAt {
		r.probed = true
		r.probe()
	}
	return int(n), nil
}

// TestCopyStreamToFile_PersistsOffsetMidStream 是「强杀续传」回归：单流进度必须在流
// 进行中持续落盘。旧实现用 io.Copy ⇒ 只有整体返回（成功/出错）才 addOffset，进程被
// kill/断电时 offset 恒为 0，重入只能从 0 重下（E2E 实测：中断后 offset=0）。
func TestCopyStreamToFile_PersistsOffsetMidStream(t *testing.T) {
	t.Parallel()
	out := filepath.Join(t.TempDir(), "f.bin")
	const total = 40 << 20 // 40 MiB
	var observed atomic.Int64
	reader := &midStreamProbeReader{
		remaining: total,
		probeAt:   8 << 20, // 读到 32MiB 时探测
		probe: func() {
			if st, ok := loadDownloadResumeState(out); ok {
				observed.Store(st.Offset)
			}
		},
	}
	c := NewFileClient("http://127.0.0.1:1")
	c.logger = testLogger()
	rs := newDownloadResume(out, downloadResumeState{Filename: "f", Size: total})
	if err := c.copyStreamToFile(out, reader, 0, total, rs); err != nil {
		t.Fatalf("copyStreamToFile: %v", err)
	}
	if got := observed.Load(); got <= 0 {
		t.Fatalf("流进行中 sidecar offset 应已落盘 >0（实际 %d）——强杀场景会从头重下", got)
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() != total {
		t.Fatalf("输出大小不符: err=%v", err)
	}
}

// TestDownloadResumable_CheckpointLagResumesFromCheckpoint 回归 E2E 发现：强杀时
// 已写文件常大于最后一次落盘的 checkpoint（checkpoint 落后）。此时必须从 checkpoint
// 续传，而不是因“大小不等”判陈旧从头重下。
func TestDownloadResumable_CheckpointLagResumesFromCheckpoint(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(3000)
	sum := sha256hex(content)
	var ranges []string
	var mu sync.Mutex
	ts := newStreamResumeServer(t, content, &ranges, &mu, nil, nil)

	out := filepath.Join(t.TempDir(), "f.bin")
	// 文件已写 1500B，但 sidecar 只记到 1000B（模拟 checkpoint 落后 500B）。
	if err := os.WriteFile(out, content[:1500], 0o644); err != nil {
		t.Fatal(err)
	}
	st := downloadResumeState{
		Version: downloadResumeVersion, Filename: "f.bin", Size: int64(len(content)),
		Checksum: sum, MTime: testResumeMTime, Offset: 1000,
	}
	if err := writeDownloadResumeState(out, &st); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err != nil {
		t.Fatalf("DownloadResumable: %v", err)
	}
	got, _ := os.ReadFile(out)
	if !bytes.Equal(got, content) {
		t.Fatalf("checkpoint 落后场景内容不符")
	}
	mu.Lock()
	first := ""
	if len(ranges) > 0 {
		first = ranges[0]
	}
	mu.Unlock()
	if first != "bytes=1000-" {
		t.Fatalf("应从 checkpoint(1000) 续传，实际 %q（all=%v）", first, ranges)
	}
}

// TestDownloadResumable_CompleteButNotFinalized 回归：强杀在收尾前（文件已收满但
// sidecar 未更新/未删除）时，直接校验通过即可完成，不再发任何 GET。
func TestDownloadResumable_CompleteButNotFinalized(t *testing.T) {
	t.Parallel()
	content := resumeTestContent(2000)
	sum := sha256hex(content)
	var gets atomic.Int32
	var mu sync.Mutex
	var ranges []string
	ts := newStreamResumeServer(t, content, &ranges, &mu, nil, &gets)

	out := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(out, content, 0o644); err != nil { // 已是完整内容
		t.Fatal(err)
	}
	st := downloadResumeState{
		Version: downloadResumeVersion, Filename: "f.bin", Size: int64(len(content)),
		Checksum: sum, MTime: testResumeMTime, Offset: 1000, // checkpoint 落后但文件已满
	}
	if err := writeDownloadResumeState(out, &st); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient(ts.URL)
	c.logger = testLogger()
	if err := c.DownloadResumable(t.Context(), "f.bin", out); err != nil {
		t.Fatalf("DownloadResumable: %v", err)
	}
	if n := gets.Load(); n != 0 {
		t.Fatalf("已收满且校验通过时不应再发 GET，实际 %d 次", n)
	}
	if _, serr := os.Stat(downloadResumePath(out)); !os.IsNotExist(serr) {
		t.Fatalf("成功收尾应删除 sidecar")
	}
}
