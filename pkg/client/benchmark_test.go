// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
)

// mockBenchUploadHandler 处理 /upload 路由，由 newMockServerBench 注册。
// 校验 X-File-Checksum、解析 multipart 表单、流式哈希上传体并与声明的 checksum 比对。
//
// **不落盘**是刻意的：`pkg/client` benchmark 测的是客户端装配 + HTTP 往返，夹具把 payload
// 写进 b.TempDir()（runner 系统盘）只会引入 runner 的磁盘回写带宽——一场跑出 GiB 级脏页后
// 单次 1 MiB 上传会从 5 ms 劣化到 6.5 s、4 MiB 到 29.4 s，Benchmark job 必然超时。
// 落盘语义由 pkg/client 的常规单测（newMockServer 的 mockUploadHandler）覆盖，不需要
// benchmark 重复。事故取证见 docs/archive/benchmark-ci-timeout-disk-io.md。
func mockBenchUploadHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cs := r.Header.Get("X-File-Checksum")
		if cs == "" {
			http.Error(w, `{"success":false,"message":"missing X-File-Checksum"}`, http.StatusBadRequest)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()

		// 消费（丢弃）+ 流式哈希：保留服务端校验语义，但不产生任何磁盘副作用。
		hasher := sha256.New()
		if _, cerr := io.Copy(hasher, f); cerr != nil {
			http.Error(w, cerr.Error(), http.StatusInternalServerError)
			return
		}
		serverCS := hex.EncodeToString(hasher.Sum(nil))
		if serverCS != cs {
			http.Error(w, `{"success":false,"message":"checksum mismatch"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success":       true,
			"message":       "ok",
			"file_checksum": serverCS,
		})
	}
}

// mockBenchDownloadHandler 处理 /download 路由，由 newMockServerBench 注册。
// 读取 dir 下文件并返回内容，响应头附带 SHA-256 checksum。
func mockBenchDownloadHandler(dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Query().Get("filename")
		if name == "" {
			http.Error(w, "missing filename", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.Base(name)))
		if err != nil {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		sum := sha256.Sum256(data)
		w.Header().Set("X-File-Checksum", hex.EncodeToString(sum[:]))
		w.Write(data)
	}
}

// newMockServerBench 是 newMockServer 的 testing.TB 版本，兼容 *testing.B。
// 逻辑与 newMockServer 相同：提供 /upload、/download、/api/files 等路由。
func newMockServerBench(tb testing.TB) (*httptest.Server, string) {
	tb.Helper()
	dir := tb.TempDir()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /upload", mockBenchUploadHandler())
	mux.HandleFunc("GET /download", mockBenchDownloadHandler(dir))

	mux.HandleFunc("GET /api/files", func(w http.ResponseWriter, r *http.Request) {
		entries, _ := os.ReadDir(dir)
		var files []FileInfo
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			info, _ := e.Info()
			files = append(files, FileInfo{Name: e.Name(), Size: info.Size()})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	})

	ts := httptest.NewServer(mux)
	tb.Cleanup(ts.Close)
	return ts, dir
}

// benchUploadRequest 构造一次针对 benchmark mock 的 /upload 请求（multipart 体 + checksum 头）。
func benchUploadRequest(tb testing.TB, baseURL, name string, payload []byte, checksum string) *http.Request {
	tb.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", name)
	if err != nil {
		tb.Fatalf("CreateFormFile: %v", err)
	}
	if _, err = part.Write(payload); err != nil {
		tb.Fatalf("写 multipart part: %v", err)
	}
	if err = mw.Close(); err != nil {
		tb.Fatalf("关闭 multipart writer: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/upload", &buf)
	if err != nil {
		tb.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", checksum)
	return req
}

// TestMockBenchUploadHandler_DoesNotPersistPayload 是「benchmark 夹具不得把上传体落盘」的
// 回归守卫：夹具一旦写盘，runner 的脏页回写带宽就进入计时路径——1 MiB 的 op 从 5 ms 劣化到
// 6.49 s、4 MiB 到 29.4 s，Benchmark job 在 6 分钟窗口内必然被 cancel（事故取证见
// docs/archive/benchmark-ci-timeout-disk-io.md）。
func TestMockBenchUploadHandler_DoesNotPersistPayload(t *testing.T) {
	t.Parallel()

	ts, dir := newMockServerBench(t)
	payload := bytes.Repeat([]byte("A"), 32*1024)
	sum := sha256.Sum256(payload)

	resp, err := ts.Client().Do(benchUploadRequest(t, ts.URL, "probe.bin", payload, hex.EncodeToString(sum[:])))
	if err != nil {
		t.Fatalf("POST /upload: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("期望 200，得到 %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"success":true`) {
		t.Fatalf("期望 success=true 的响应，得到: %s", body)
	}

	// checksum 不符必须仍然被拒绝：「不落盘」不得顺手丢掉校验语义。
	other := sha256.Sum256([]byte("other-payload"))
	badResp, err := ts.Client().Do(benchUploadRequest(t, ts.URL, "probe.bin", payload, hex.EncodeToString(other[:])))
	if err != nil {
		t.Fatalf("POST /upload（checksum 不符）: %v", err)
	}
	_, _ = io.Copy(io.Discard, badResp.Body)
	_ = badResp.Body.Close()
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("checksum 不符时期望 400，得到 %d", badResp.StatusCode)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("benchmark mock 把上传体落盘了（%d 个文件：%v）——这会把 runner 的磁盘回写带宽\n"+
			"带进计时路径，使 Benchmark job 必然超时；mock 只应流式校验 checksum 后丢弃。见\n"+
			"docs/archive/benchmark-ci-timeout-disk-io.md", len(entries), names)
	}
}

// BenchmarkUpload 测试 1MB 文件普通上传性能。
// 单次操作处理 1 MiB 数据，通过 b.SetBytes 记录吞吐量。
// benchStallLimit 是「环境 I/O 塌陷」判定阈值：单次 op 超过它就判定为 runner 级故障。
//
// 实测数据（1 MiB / 4 MiB 上传 op）：正常 5–17 ms；runner I/O 塌陷时 1 MiB 恒定 **6.45–7.33 s**、
// 4 MiB **29.4 s**（与字节数成正比），且可能持续整场不恢复 ⇒ benchmark 按 ~1 s/op 选的 N 会把
// 单个 count 拉成几十分钟，job 只能在 6 分钟里静默被杀（无诊断）。阈值取 2 s = 正常值的约 100–400 倍，
// 既能第一时间拦住塌陷（~2 s 内失败），又不会在「慢一点的 runner」上误报。
// 取证与判据：docs/archive/benchmark-ci-timeout-disk-io.md
const benchStallLimit = 2 * time.Second

// benchStallErr 返回非 nil 表示单次 op 耗时已落入「环境 I/O 塌陷」区间。
// 抽成纯函数是为了可被单测确定性覆盖（benchmark 本体无法自测失败路径）。
func benchStallErr(op string, d time.Duration, payload int) error {
	if d <= benchStallLimit {
		return nil
	}
	return fmt.Errorf("环境 I/O 塌陷：%s 单次 op 耗时 %v（> %v；正常 ~10 ms，payload=%d B）——"+
		"这不是代码回归而是 runner 级 I/O 塌陷，重跑失败的 job 即可（判据见 "+
		"docs/archive/benchmark-ci-timeout-disk-io.md）", op, d, benchStallLimit, payload)
}

// checkBenchStall 在每次 op 后调用：把「runner 塌陷」从 6 分钟静默超时变成 ~2 秒响亮失败。
func checkBenchStall(b *testing.B, op string, start time.Time, payload int) {
	b.Helper()
	if err := benchStallErr(op, time.Since(start), payload); err != nil {
		b.Fatal(err)
	}
}

// TestBenchStallErr 钉住「环境 I/O 塌陷」判定：实测塌陷值（1 MiB op = 7.28 s）必须判失败且信息可操作，
// 正常毫秒级不得误报。判据与取证：docs/archive/benchmark-ci-timeout-disk-io.md
func TestBenchStallErr(t *testing.T) {
	t.Parallel()

	if err := benchStallErr("BenchmarkUpload(1 MiB)", 20*time.Millisecond, 1<<20); err != nil {
		t.Fatalf("正常耗时不得判定为塌陷: %v", err)
	}
	err := benchStallErr("BenchmarkUpload(1 MiB)", 7280*time.Millisecond, 1<<20)
	if err == nil {
		t.Fatal("7.28s 的 1 MiB op（实测 CI 塌陷值）必须判定为环境 I/O 塌陷")
	}
	for _, want := range []string{"BenchmarkUpload(1 MiB)", "7.28s", "1048576", "重跑"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("失败信息应可操作（应含 %q），实际: %v", want, err)
		}
	}
}

func BenchmarkUpload(b *testing.B) {
	ts, _ := newMockServerBench(b)

	// 创建 1MB 临时文件
	srcDir := b.TempDir()
	src := filepath.Join(srcDir, "upload.dat")
	data := make([]byte, 1*size.MiB)
	if _, err := rand.Read(data); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0644); err != nil {
		b.Fatal(err)
	}

	c := NewFileClient(ts.URL)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		remoteName := fmt.Sprintf("bench_upload_%d.dat", i)
		opStart := time.Now()
		res, err := c.Upload(b.Context(), src, remoteName)
		checkBenchStall(b, "BenchmarkUpload(1 MiB)", opStart, len(data))
		if err != nil {
			b.Fatalf("Upload: %v", err)
		}
		if !res.Success {
			b.Fatalf("upload 失败: %+v", res)
		}
	}
}

// BenchmarkDownload 测试已上传文件的下载性能。
// 先预置 1MB 文件到服务端目录，再反复下载到临时目录。
// 单次操作处理 1 MiB 数据，通过 b.SetBytes 记录吞吐量。
func BenchmarkDownload(b *testing.B) {
	ts, dir := newMockServerBench(b)

	// 预置 1MB 文件到服务端
	data := make([]byte, 1*size.MiB)
	if _, err := rand.Read(data); err != nil {
		b.Fatal(err)
	}
	expectedSum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, "download.dat"), data, 0644); err != nil {
		b.Fatal(err)
	}

	outDir := b.TempDir()
	c := NewFileClient(ts.URL)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := filepath.Join(outDir, fmt.Sprintf("got_%d.dat", i))
		opStart := time.Now()
		if err := c.Download(b.Context(), "download.dat", out); err != nil {
			b.Fatalf("Download: %v", err)
		}
		checkBenchStall(b, "BenchmarkDownload(1 MiB)", opStart, len(data))
		// 验证下载内容正确性
		got, err := os.ReadFile(out)
		if err != nil {
			b.Fatalf("读取下载文件失败: %v", err)
		}
		gotSum := sha256.Sum256(got)
		if gotSum != expectedSum {
			b.Fatalf("下载内容 checksum 不匹配")
		}
	}
}

// BenchmarkUpload_4MB_Regular 测试 4MB 文件上传性能。
//
// 假设：4MB < AutoChunkThreshold（100 MiB），因此走普通上传路径，不应触发自动分块。
// 手动设置 ChunkSize = 1MB 验证客户端配置正确传递。
// 单次操作处理 4 MiB 数据，通过 b.SetBytes 记录吞吐量。
func BenchmarkUpload_4MB_Regular(b *testing.B) {
	ts, _ := newMockServerBench(b)

	// 创建 4MB 临时文件
	srcDir := b.TempDir()
	src := filepath.Join(srcDir, "chunked_upload.dat")
	data := make([]byte, 4*size.MiB)
	if _, err := rand.Read(data); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(src, data, 0644); err != nil {
		b.Fatal(err)
	}

	c := NewFileClient(ts.URL)

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		remoteName := fmt.Sprintf("bench_chunked_%d.dat", i)
		opStart := time.Now()
		res, err := c.Upload(b.Context(), src, remoteName)
		checkBenchStall(b, "BenchmarkUpload_4MB_Regular(4 MiB)", opStart, len(data))
		if err != nil {
			b.Fatalf("Upload: %v", err)
		}
		if !res.Success {
			b.Fatalf("upload 失败: %+v", res)
		}
	}
}

// BenchmarkListFiles 测试列出包含 100+ 文件的目录性能。
// 先创建 100 个小型文本文件到服务端目录，再反复调用 List。
func BenchmarkListFiles(b *testing.B) {
	ts, dir := newMockServerBench(b)

	// 创建 100 个文件
	for i := range 100 {
		name := fmt.Sprintf("file_%03d.txt", i)
		content := fmt.Sprintf("content_%d", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			b.Fatal(err)
		}
	}

	c := NewFileClient(ts.URL)
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		opStart := time.Now()
		files, err := c.List(b.Context())
		checkBenchStall(b, "BenchmarkListFiles", opStart, 0)
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(files) != 100 {
			b.Fatalf("期望 100 个文件，得到 %d", len(files))
		}
	}
}
