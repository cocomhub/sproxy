// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
)

// mockBenchUploadHandler 处理 /upload 路由，由 newMockServerBench 注册。
// 校验 X-File-Checksum、解析 multipart 表单、写入文件到 dir 并比对 checksum。
func mockBenchUploadHandler(dir string) http.HandlerFunc {
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
		f, h, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()

		out, _ := os.Create(filepath.Join(dir, filepath.Base(h.Filename)))
		defer out.Close()
		hasher := sha256.New()
		buf := make([]byte, 4096)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				hasher.Write(buf[:n])
			}
			if rerr != nil {
				break
			}
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

	mux.HandleFunc("POST /upload", mockBenchUploadHandler(dir))
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

// BenchmarkUpload 测试 1MB 文件普通上传性能。
// 单次操作处理 1 MiB 数据，通过 b.SetBytes 记录吞吐量。
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
		res, err := c.Upload(b.Context(), src, remoteName)
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
	if err := os.WriteFile(filepath.Join(dir, "download.dat"), data, 0644); err != nil {
		b.Fatal(err)
	}

	outDir := b.TempDir()
	c := NewFileClient(ts.URL)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		out := filepath.Join(outDir, fmt.Sprintf("got_%d.dat", i))
		if err := c.Download(b.Context(), "download.dat", out); err != nil {
			b.Fatalf("Download: %v", err)
		}
	}
}

// BenchmarkChunkedUpload 测试 4MB 文件上传性能。
//
// ShouldAutoChunk(4MB) 返回 false（阈值 100 MiB），因此走普通上传路径。
// 手动设置 ChunkSize = 1MB 验证客户端配置正确传递。
// 单次操作处理 4 MiB 数据，通过 b.SetBytes 记录吞吐量。
func BenchmarkChunkedUpload(b *testing.B) {
	if ShouldAutoChunk(4 * size.MiB) {
		b.Fatal("4MB 不应触发自动分块（阈值 100 MiB）")
	}

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

	// 手动设 ChunkSize = 1MB，不自适应分块
	c := NewFileClient(ts.URL)
	c.ChunkSize = 1 * size.MiB

	b.SetBytes(int64(len(data)))
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		remoteName := fmt.Sprintf("bench_chunked_%d.dat", i)
		res, err := c.Upload(b.Context(), src, remoteName)
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
		files, err := c.List(b.Context())
		if err != nil {
			b.Fatalf("List: %v", err)
		}
		if len(files) != 100 {
			b.Fatalf("期望 100 个文件，得到 %d", len(files))
		}
	}
}
