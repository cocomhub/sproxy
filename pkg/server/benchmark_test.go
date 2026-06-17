// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---- 基准测试辅助函数（接受 testing.TB 以支持 testing.B） ----

// benchServer 创建基准测试用测试服务器（不含分块路由）。
func benchServer(tb testing.TB, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	tb.Helper()

	tmpDir := tb.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(cfg.UploadsDir, nil)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "bench",
		buildAt:       "bench",
		checksumStore: cs,
		logger:        slog.Default(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	mux.HandleFunc("GET /healthz", h.healthz)

	ts := httptest.NewServer(mux)
	tb.Cleanup(ts.Close)
	return ts.URL, &cfgPtr
}

// benchServerWithChunked 创建含分块路由的基准测试用测试服务器。
func benchServerWithChunked(tb testing.TB, modifyCfg func(*Config)) (string, *atomic.Pointer[Config]) {
	tb.Helper()

	tmpDir := tb.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	cfg.ChunkSize = 1 << 20 // 1 MiB for benchmarks
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(cfg.UploadsDir, nil)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "bench",
		buildAt:       "bench",
		checksumStore: cs,
		uploadStore:   NewUploadStore(cfg.UploadsDir, 24*time.Hour, nil),
		logger:        slog.Default(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	mux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	mux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	mux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))

	ts := httptest.NewServer(mux)
	tb.Cleanup(func() {
		ts.Close()
		h.uploadStore.Stop()
	})
	return ts.URL, &cfgPtr
}

// uploadFileBench 上传文件（testing.TB 版本，复用 uploadFile 的逻辑）。
func uploadFileBench(tb testing.TB, baseURL, filename string, body []byte, headers map[string]string) (int, []byte) {
	tb.Helper()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		tb.Fatalf("create form file: %v", err)
	}
	if _, err = part.Write(body); err != nil {
		tb.Fatalf("write part: %v", err)
	}
	if err = mw.Close(); err != nil {
		tb.Fatalf("close multipart: %v", err)
	}

	req, err := http.NewRequest("POST", baseURL+"/upload", &buf)
	if err != nil {
		tb.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	var resp *http.Response
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		tb.Fatalf("do upload: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody
}

// ---- 基准测试 ----

// BenchmarkUpload 上传 1 MiB 文件，记录吞吐量（bytes/sec）。
func BenchmarkUpload(b *testing.B) {
	url, _ := benchServer(b, nil)

	data := bytes.Repeat([]byte("A"), 1<<20) // 1 MiB
	cs := sha256hex(data)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		filename := fmt.Sprintf("bench-upload-%d.bin", i)
		status, _ := uploadFileBench(b, url, filename, data, map[string]string{
			"X-File-Checksum": cs,
		})
		if status != 200 {
			b.Fatalf("upload #%d failed: status=%d", i, status)
		}
	}
}

// BenchmarkDownload 下载已上传的 1 MiB 文件。
func BenchmarkDownload(b *testing.B) {
	url, _ := benchServer(b, nil)

	// Setup：上传一个 1 MiB 文件（不计入计时）
	data := bytes.Repeat([]byte("B"), 1<<20)
	cs := sha256hex(data)
	status, _ := uploadFileBench(b, url, "bench-download.bin", data, map[string]string{
		"X-File-Checksum": cs,
	})
	if status != 200 {
		b.Fatalf("setup upload failed: %d", status)
	}

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		resp, err := http.Get(url + "/download?filename=bench-download.bin")
		if err != nil {
			b.Fatalf("download: %v", err)
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if n != int64(len(data)) {
			b.Fatalf("read %d bytes, expected %d", n, len(data))
		}
	}
}

// BenchmarkConcurrentUploads 10 并发 goroutine 同时上传 ~10 KiB 小文件。
func BenchmarkConcurrentUploads(b *testing.B) {
	url, _ := benchServer(b, nil)

	data := bytes.Repeat([]byte("small"), 2500) // ≈ 10 KiB
	cs := sha256hex(data)

	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()

	const concurrency = 10
	var (
		wg      sync.WaitGroup
		counter atomic.Int64
		errCh   = make(chan error, concurrency)
	)

	for range concurrency {
		wg.Go(func() {
			for {
				n := int(counter.Add(1) - 1)
				if n >= b.N {
					return
				}
				filename := fmt.Sprintf("concurrent-%d-%d.bin", n, time.Now().UnixNano())
				status, _ := uploadFileBench(b, url, filename, data, map[string]string{
					"X-File-Checksum": cs,
				})
				if status != 200 {
					errCh <- fmt.Errorf("concurrent upload #%d failed: status=%d", n, status)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChunkedUpload 4 MiB 文件分块上传（4 chunks @ 1 MiB each）。
func BenchmarkChunkedUpload(b *testing.B) {
	url, _ := benchServerWithChunked(b, nil)

	chunkSize := int64(1 << 20) // 1 MiB per chunk
	totalChunks := 4
	fileSize := int(chunkSize) * totalChunks // 4 MiB

	// 生成 4 MiB 文件数据
	fileData := make([]byte, fileSize)
	for i := range fileData {
		fileData[i] = byte(i % 251) // 可预测但非全重复
	}
	fileChecksum := sha256hex(fileData)

	b.SetBytes(int64(fileSize))
	b.ReportAllocs()
	b.ResetTimer()

	for i := range b.N {
		uploadID := fmt.Sprintf("bench-chunked-%d", i)
		filename := fmt.Sprintf("bench-chunked-%d.bin", i)

		// ---- Init ----
		initReq := map[string]any{
			"upload_id":     uploadID,
			"filename":      filename,
			"total_size":    fileSize,
			"chunk_size":    chunkSize,
			"total_chunks":  totalChunks,
			"file_checksum": fileChecksum,
		}
		initBody, _ := json.Marshal(initReq)
		resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initBody))
		if err != nil {
			b.Fatalf("init #%d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		// ---- Upload chunks ----
		for ci := range totalChunks {
			start := ci * int(chunkSize)
			end := min(start+int(chunkSize), fileSize)
			chunkData := fileData[start:end]
			chunkCS := sha256hex(chunkData)

			var buf bytes.Buffer
			mw := multipart.NewWriter(&buf)
			_ = mw.WriteField("upload_id", uploadID)
			_ = mw.WriteField("chunk_index", fmt.Sprintf("%d", ci))
			_ = mw.WriteField("chunk_checksum", chunkCS)
			part, _ := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", ci))
			_, _ = part.Write(chunkData)
			_ = mw.Close()

			resp, err = http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
			if err != nil {
				b.Fatalf("chunk #%d/%d: %v", i, ci, err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}

		// ---- Complete ----
		completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
		resp, err = http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
		if err != nil {
			b.Fatalf("complete #%d: %v", i, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
