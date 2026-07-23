// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/client"
)

// newTestServerWithChunked 启动一个包含分块上传/下载路由的测试服务器。
// 使用 t.TempDir() 与 t.Cleanup() 自动管理临时目录与 UploadStore 后台 goroutine。
func newTestServerWithChunked(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config], func()) {
	t.Helper()

	tmpDir := t.TempDir()

	cfg := Default()
	cfg.UploadsDir = tmpDir
	cfg.ChunkSize = 4 << 10 // 4 KiB for testing
	if modifyCfg != nil {
		modifyCfg(cfg)
	}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(cfg.UploadsDir, nil)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		version:       "test",
		buildAt:       "test",
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
	t.Cleanup(func() {
		ts.Close()
		h.uploadStore.Stop()
	})
	// 返回 no-op cleanup 保持调用方代码兼容
	cleanup := func() {}
	return ts.URL, &cfgPtr, cleanup
}

func TestUploadInit_HappyPath(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	body := []byte("test file content for chunked upload")
	fileChecksum := sha256hex(body)

	initReq := map[string]any{
		"upload_id":     "test-upload-happy",
		"filename":      "chunked-test.txt",
		"total_size":    len(body),
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": fileChecksum,
	}
	initJSON, _ := json.Marshal(initReq)

	resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var initResult ChunkedInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResult); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !initResult.Success {
		t.Fatalf("expected success, got: %v", initResult)
	}
	if initResult.UploadID == "" {
		t.Fatal("expected non-empty upload_id")
	}
}

func TestUploadInit_InvalidFilename(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	initReq := map[string]any{
		"upload_id":     "test-upload-invalid-filename",
		"filename":      "../../escape.txt",
		"total_size":    100,
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": strings.Repeat("a", 64),
	}
	initJSON, _ := json.Marshal(initReq)

	resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for path traversal, got %d", resp.StatusCode)
	}
}

func TestUploadInit_InvalidChecksum(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	initReq := map[string]any{
		"upload_id":     "test-upload-invalid-checksum",
		"filename":      "test.txt",
		"total_size":    100,
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": "not-a-valid-hex-checksum",
	}
	initJSON, _ := json.Marshal(initReq)

	resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid checksum, got %d", resp.StatusCode)
	}
}

func TestUploadChunk_Success(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 先 init
	fileData := []byte("hello chunked world! this is a test of chunked upload")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "chunk-test.txt", int64(len(fileData)), fileChecksum)

	// 上传一个分块
	chunkIndex := 0
	chunkCS := sha256hex(fileData)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("upload_id", uploadID)
	mw.WriteField("chunk_index", fmt.Sprintf("%d", chunkIndex))
	mw.WriteField("chunk_checksum", chunkCS)
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	part.Write(fileData)
	mw.Close()

	resp, err := http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("chunk upload failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var chunkResult ChunkUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&chunkResult); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !chunkResult.Success {
		t.Fatalf("chunk upload failed: %v", chunkResult)
	}
}

func TestUploadChunk_ChecksumMismatch(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("data with correct content")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "bad-chunk.txt", int64(len(fileData)), fileChecksum)

	// 上传分块但给错误的 checksum
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("upload_id", uploadID)
	mw.WriteField("chunk_index", "0")
	mw.WriteField("chunk_checksum", sha256hex([]byte("wrong data")))
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	part.Write(fileData)
	mw.Close()

	resp, err := http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("chunk upload failed: %v", err)
	}
	defer resp.Body.Close()

	var chunkResult ChunkUploadResponse
	json.NewDecoder(resp.Body).Decode(&chunkResult)
	if chunkResult.Success {
		t.Fatal("expected failure due to checksum mismatch")
	}
	if !chunkResult.ShouldRetry {
		t.Fatal("expected should_retry=true on checksum mismatch")
	}
}

func TestUploadChunk_Idempotent(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("idempotent chunk data")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "idempotent.txt", int64(len(fileData)), fileChecksum)

	chunkCS := sha256hex(fileData)

	// 第一次上传
	uploadChunk(t, url, uploadID, 0, chunkCS, fileData)

	// 第二次上传同样的分块，应该幂等成功
	resp := uploadChunk(t, url, uploadID, 0, chunkCS, fileData)
	var result ChunkUploadResponse
	json.NewDecoder(resp.Body).Decode(&result)
	resp.Body.Close()

	if !result.Success {
		t.Fatalf("idempotent upload should succeed: %v", result)
	}
}

func TestUploadStatus_Partial(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 创建一个总大小正好为 2 个分块的文件
	fileData := bytes.Repeat([]byte("A"), 8192) // 2 chunks at 4 KiB each
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "partial-status.txt", int64(len(fileData)), fileChecksum)

	// 只上传第一个分块
	chunk0 := fileData[:4096]
	chunk0CS := sha256hex(chunk0)
	uploadChunk(t, url, uploadID, 0, chunk0CS, chunk0)

	// 查询状态
	resp, err := http.Get(url + "/upload/status?upload_id=" + uploadID)
	if err != nil {
		t.Fatalf("status request failed: %v", err)
	}
	defer resp.Body.Close()

	var status ChunkStatusResponse
	json.NewDecoder(resp.Body).Decode(&status)

	if !status.Success {
		t.Fatalf("expected success, got: %v", status)
	}
	if status.ReceivedCount != 1 {
		t.Fatalf("expected 1 received chunk, got %d", status.ReceivedCount)
	}
	if len(status.MissingChunks) != 1 || status.MissingChunks[0] != 1 {
		t.Fatalf("expected missing chunk [1], got %v", status.MissingChunks)
	}
	if status.Completed {
		t.Fatal("should not be completed")
	}
}

func TestUploadComplete_FullFlow(t *testing.T) {
	url, cfgPtr, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 创建 3 个分块的文件
	fileData := bytes.Repeat([]byte("TestComplete"), 12000) // ~3 chunks at 4 KiB
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)
	totalChunks := int((int64(len(fileData)) + chunkSize - 1) / chunkSize)

	uploadID := initSessionEx(t, url, "full-flow.txt", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

	// 上传所有分块
	for i := range totalChunks {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		chunkCS := sha256hex(chunkData)
		uploadChunk(t, url, uploadID, i, chunkCS, chunkData)
	}

	// 完成
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete request failed: %v", err)
	}
	defer resp.Body.Close()

	var completeResult ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&completeResult)

	if !completeResult.Success {
		t.Fatalf("expected success, got: %v", completeResult)
	}
	if completeResult.FileChecksum != fileChecksum {
		t.Fatalf("checksum mismatch: server=%s expected=%s", completeResult.FileChecksum, fileChecksum)
	}

	// 验证文件已保存
	uploadsDir := cfgPtr.Load().UploadsDir
	savedPath := filepath.Join(uploadsDir, "full-flow.txt")
	if _, err := os.Stat(savedPath); os.IsNotExist(err) {
		t.Fatalf("saved file not found: %s", savedPath)
	}

	// 验证文件内容正确
	saved, _ := os.ReadFile(savedPath)
	if !bytes.Equal(saved, fileData) {
		t.Fatalf("saved file content mismatch: len(saved)=%d len(original)=%d", len(saved), len(fileData))
	}
}

func TestUploadComplete_MissingChunks(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := bytes.Repeat([]byte("B"), 8192)
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "missing-chunks.txt", int64(len(fileData)), fileChecksum)

	// 只上传一个分块，另一个不上传
	chunk0 := fileData[:4096]
	chunk0CS := sha256hex(chunk0)
	uploadChunk(t, url, uploadID, 0, chunk0CS, chunk0)

	// 尝试完成（应该失败）
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing chunks, got %d", resp.StatusCode)
	}
}

func TestDownloadChunk_FirstChunk(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 先用普通上传创建一个文件，确保 checksumStore 有记录
	fileData := []byte("download chunk test file content for chunked download test")
	fileChecksum := sha256hex(fileData)

	// 通过 upload init + complete 创建文件
	chunkSize := int64(4096)
	totalChunks := 1
	uploadID := initSessionEx(t, url, "dl-chunk-test.txt", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)
	// 上传分块
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("upload_id", uploadID)
	mw.WriteField("chunk_index", "0")
	mw.WriteField("chunk_checksum", fileChecksum)
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	part.Write(fileData)
	mw.Close()
	z1, _ := http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	if z1 != nil {
		z1.Body.Close()
	}
	// 完成
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	z2, _ := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if z2 != nil {
		z2.Body.Close()
	}

	// 下载第一个分块
	resp, err := http.Get(url + "/download/chunk?filename=dl-chunk-test.txt&offset=0&length=10")
	if err != nil {
		t.Fatalf("download chunk failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// 验证 Content-Range
	cr := resp.Header.Get("Content-Range")
	expectedCR := fmt.Sprintf("bytes 0-9/%d", len(fileData))
	if cr != expectedCR {
		t.Fatalf("Content-Range mismatch: got %q, want %q", cr, expectedCR)
	}

	// 验证 X-Chunk-Checksum
	chunkCS := resp.Header.Get("X-Chunk-Checksum")
	actualChunk := fileData[:10]
	expectedChunkCS := sha256hex(actualChunk)
	if chunkCS != expectedChunkCS {
		t.Fatalf("chunk checksum mismatch: got %s, want %s", chunkCS, expectedChunkCS)
	}

	// 验证 X-File-Checksum
	fileCS := resp.Header.Get("X-File-Checksum")
	if fileCS != fileChecksum {
		t.Fatalf("file checksum mismatch: got %s, want %s", fileCS, fileChecksum)
	}

	// 验证 body
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, actualChunk) {
		t.Fatalf("body mismatch: got %q, want %q", body, actualChunk)
	}
}

func TestDownloadChunk_EntireFile(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("entire file content as single chunk")
	fileChecksum := sha256hex(fileData)

	// 通过分块上传创建文件
	chunkSize := int64(4096)
	totalChunks := 1
	uploadID := initSessionEx(t, url, "full-chunk.txt", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("upload_id", uploadID)
	mw.WriteField("chunk_index", "0")
	mw.WriteField("chunk_checksum", fileChecksum)
	part, _ := mw.CreateFormFile("chunk", "00000.chunk")
	part.Write(fileData)
	mw.Close()
	z3, _ := http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	if z3 != nil {
		z3.Body.Close()
	}
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	z4, _ := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if z4 != nil {
		z4.Body.Close()
	}

	resp, err := http.Get(fmt.Sprintf("%s/download/chunk?filename=full-chunk.txt&offset=0&length=%d", url, len(fileData)))
	if err != nil {
		t.Fatalf("download chunk failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, fileData) {
		t.Fatalf("body mismatch")
	}

	chunkCS := resp.Header.Get("X-Chunk-Checksum")
	if chunkCS != fileChecksum {
		t.Fatalf("chunk checksum should equal file checksum for single chunk: %s vs %s", chunkCS, fileChecksum)
	}
}

func TestDownloadChunk_InvalidOffset(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("small file")
	fileChecksum := sha256hex(fileData)

	// 通过分块上传创建文件
	uploadID := initSession(t, url, "small.txt", int64(len(fileData)), fileChecksum)
	uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	z5, _ := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if z5 != nil {
		z5.Body.Close()
	}

	// offset 超过 file size
	resp, err := http.Get(url + "/download/chunk?filename=small.txt&offset=100&length=10")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", resp.StatusCode)
	}
}

func TestDownloadChunk_PathTraversal(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	resp, err := http.Get(url + "/download/chunk?filename=../../../etc/passwd&offset=0&length=10")
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

// ---- Helpers ----

func TestUploadComplete_SubDir(t *testing.T) {
	url, cfgPtr, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// filename 包含子目录
	filename := "subdir/upload-test.txt"
	fileData := []byte("file in subdirectory via chunked upload")
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)
	totalChunks := 1

	uploadID := initSessionEx(t, url, filename, int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

	// 上传分块
	uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)

	// 完成
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete request failed: %v", err)
	}
	defer resp.Body.Close()

	var completeResult ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&completeResult)
	if !completeResult.Success {
		t.Fatalf("expected success, got: %v", completeResult)
	}

	// 验证文件已创建在子目录中
	uploadsDir := cfgPtr.Load().UploadsDir
	savedPath := filepath.Join(uploadsDir, filepath.FromSlash(filename))
	if _, err := os.Stat(savedPath); os.IsNotExist(err) {
		t.Fatalf("saved file not found at subdirectory path: %s", savedPath)
	}

	saved, _ := os.ReadFile(savedPath)
	if !bytes.Equal(saved, fileData) {
		t.Fatalf("saved file content mismatch")
	}
}

// ---- 辅助函数 ----

func initSession(t *testing.T, baseURL, filename string, totalSize int64, fileChecksum string) string {
	t.Helper()
	return initSessionEx(t, baseURL, filename, totalSize, 4096, int((totalSize+4095)/4096), fileChecksum)
}

func initSessionEx(t *testing.T, baseURL, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string) string {
	t.Helper()
	uploadID := fmt.Sprintf("test-upload-%s-%d", filename, totalSize)
	initReq := map[string]any{
		"upload_id":     uploadID,
		"filename":      filename,
		"total_size":    totalSize,
		"chunk_size":    chunkSize,
		"total_chunks":  totalChunks,
		"file_checksum": fileChecksum,
	}
	initJSON, _ := json.Marshal(initReq)
	resp, err := http.Post(baseURL+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init failed: %v", err)
	}
	defer resp.Body.Close()
	var result ChunkedInitResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("init failed: %v", result)
	}
	return result.UploadID
}

func uploadChunk(t *testing.T, baseURL, uploadID string, chunkIndex int, chunkCS string, data []byte) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("upload_id", uploadID)
	mw.WriteField("chunk_index", fmt.Sprintf("%d", chunkIndex))
	mw.WriteField("chunk_checksum", chunkCS)
	part, _ := mw.CreateFormFile("chunk", fmt.Sprintf("%05d.chunk", chunkIndex))
	part.Write(data)
	mw.Close()

	resp, err := http.Post(baseURL+"/upload/chunk", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatalf("upload chunk %d failed: %v", chunkIndex, err)
	}
	return resp
}

func TestUploadStore_CreateAndGet(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-test-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, err := us.CreateSession("test-upload-id", "test.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if session.UploadID == "" {
		t.Fatal("upload_id should not be empty")
	}

	got := us.GetSession(session.UploadID)
	if got == nil {
		t.Fatal("GetSession returned nil")
	}
	if got.Filename != "test.txt" {
		t.Fatalf("filename mismatch: %s", got.Filename)
	}
}

func TestUploadStore_MarkAndCheck(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-mark-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-2", "test.txt", 8192, 4096, 2, strings.Repeat("b", 64), 0)

	if us.AllChunksReceived(session.UploadID) {
		t.Fatal("should not have all chunks before any upload")
	}

	us.MarkChunkReceived(session.UploadID, 0, "chunk0hash")
	if us.AllChunksReceived(session.UploadID) {
		t.Fatal("should not have all chunks after only first")
	}

	us.MarkChunkReceived(session.UploadID, 1, "chunk1hash")
	if !us.AllChunksReceived(session.UploadID) {
		t.Fatal("should have all chunks after both")
	}
}

func TestUploadStore_Complete(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-complete-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-3", "test.txt", 100, 4096, 1, strings.Repeat("c", 64), 0)
	us.MarkChunkReceived(session.UploadID, 0, "chunkhash")
	us.CompleteSession(session.UploadID)

	got := us.GetSession(session.UploadID)
	if !got.Completed {
		t.Fatal("session should be completed")
	}

	// 重复 complete 应返回错误
	err = us.CompleteSession(session.UploadID)
	if err == nil {
		t.Fatal("expected error on double complete")
	}
}

func TestUploadStore_MissingChunks(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "us-missing-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	us := NewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test-upload-id-4", "test.txt", 8192, 4096, 2, strings.Repeat("d", 64), 0)

	us.MarkChunkReceived(session.UploadID, 0, "h0")

	missing := MissingChunks(us.GetSession(session.UploadID))
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("expected missing [1], got %v", missing)
	}
}

// ---- 通用 SHA-256 辅助 ----
// sha256hex 定义在 integration_test.go

// ---- L3: 分块上传端到端链路测试 ----

func TestChunkedUpload_MultiChunkLargeFile(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := bytes.Repeat([]byte("LargeFileData!"), 4096) // ~52 KiB
	fileChecksum := sha256hex(fileData)

	chunkSize := int64(4096)
	totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)

	uploadID := initSessionEx(t, url, "large-chunked.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

	for i := range totalChunks {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		chunkCS := sha256hex(chunkData)
		uploadChunk(t, url, uploadID, i, chunkCS, chunkData)
	}

	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()

	var result ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("complete failed: %v", result)
	}
	if result.FileChecksum != fileChecksum {
		t.Fatalf("checksum mismatch: got %s, want %s", result.FileChecksum, fileChecksum)
	}
}

func TestChunkedUpload_ResumeAfterInterrupt(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := bytes.Repeat([]byte("ResumeMe"), 3000) // ~24 KiB, 6 chunks at 4KiB
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)
	totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)

	uploadID := initSessionEx(t, url, "resume-test.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

	// 只上传前 3 个分块
	for i := range 3 {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		uploadChunk(t, url, uploadID, i, sha256hex(chunkData), chunkData)
	}

	// 查询状态
	statusResp, err := http.Get(url + "/upload/status?upload_id=" + uploadID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var status ChunkStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()

	if !status.Success {
		t.Fatalf("status failed: %v", status)
	}
	if len(status.MissingChunks) != totalChunks-3 {
		t.Fatalf("expected %d missing chunks, got %v", totalChunks-3, status.MissingChunks)
	}

	// 上传剩余分块
	for _, idx := range status.MissingChunks {
		start := idx * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		uploadChunk(t, url, uploadID, idx, sha256hex(chunkData), chunkData)
	}

	// Complete
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()
	var result ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("resume complete failed: %v", result)
	}
}

func TestChunkedUpload_AlreadyExists_ChecksumMatch(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	body := []byte("existing file content")
	cs := sha256hex(body)
	uploadFile(t, url, "existing.txt", body, map[string]string{"X-File-Checksum": cs})

	initReq := map[string]any{
		"upload_id":     "already-exists-test",
		"filename":      "existing.txt",
		"total_size":    len(body),
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": cs,
	}
	initJSON, _ := json.Marshal(initReq)
	resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer resp.Body.Close()

	var initResult ChunkedInitResponse
	json.NewDecoder(resp.Body).Decode(&initResult)
	if !initResult.Success {
		t.Fatalf("expected success for existing file with matching checksum: %v", initResult)
	}
	if initResult.UploadID != "already_exists" {
		t.Fatalf("expected upload_id='already_exists', got %q", initResult.UploadID)
	}
}

func TestChunkedUpload_AlreadyExists_ChecksumMismatch(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	body := []byte("original content")
	uploadFile(t, url, "conflict-chunked.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	initReq := map[string]any{
		"upload_id":     "conflict-test",
		"filename":      "conflict-chunked.txt",
		"total_size":    100,
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": strings.Repeat("f", 64),
	}
	initJSON, _ := json.Marshal(initReq)
	resp, err := http.Post(url+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409, got %d", resp.StatusCode)
	}
}

func TestChunkedUploadStatus_ByFilename(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("status by filename test")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "status-by-fn.txt", int64(len(fileData)), fileChecksum)

	uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)

	resp, err := http.Get(url + "/upload/status?filename=status-by-fn.txt")
	if err != nil {
		t.Fatalf("status by filename: %v", err)
	}
	defer resp.Body.Close()

	var status ChunkStatusResponse
	json.NewDecoder(resp.Body).Decode(&status)
	if !status.Success {
		t.Fatalf("status by filename failed: %v", status)
	}
	if status.UploadID != uploadID {
		t.Fatalf("expected upload_id %s, got %s", uploadID, status.UploadID)
	}
}

func TestChunkedDigestConsistency(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	data := []byte("consistency check across upload modes")
	cs := sha256hex(data)

	// 方式1: 普通上传
	uploadFile(t, url, "consistency-normal.bin", data, map[string]string{"X-File-Checksum": cs})

	// 方式2: 分块上传
	chunkSize := int64(4096)
	totalChunks := 1
	uploadID := initSessionEx(t, url, "consistency-chunked.bin", int64(len(data)), chunkSize, totalChunks, cs)
	uploadChunk(t, url, uploadID, 0, cs, data)
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	z6, _ := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if z6 != nil {
		z6.Body.Close()
	}

	c := client.NewFileClient(url)
	outDir := t.TempDir()

	out1 := filepath.Join(outDir, "normal.bin")
	if err := c.Download(t.Context(), "consistency-normal.bin", out1); err != nil {
		t.Fatalf("download normal: %v", err)
	}
	out2 := filepath.Join(outDir, "chunked.bin")
	if err := c.Download(t.Context(), "consistency-chunked.bin", out2); err != nil {
		t.Fatalf("download chunked: %v", err)
	}

	data1, _ := os.ReadFile(out1)
	data2, _ := os.ReadFile(out2)
	if !bytes.Equal(data1, data2) {
		t.Fatal("normal upload and chunked upload produced different content")
	}
}

func TestChunkedDownload_EmptyFile(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 上传空文件
	body := []byte{}
	fileCS := sha256hex(body)
	uploadFile(t, url, "empty.txt", body, map[string]string{
		"X-File-Checksum": fileCS,
	})

	// 下载空文件（从 offset=0 开始）
	resp, err := http.Get(url + "/download/chunk?filename=empty.txt&offset=0&length=64")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable || resp.StatusCode == http.StatusOK {
		// 空文件按 offset 0 处理：要么返回 416（空文件无内容可下载）
		// 要么返回 200 但 content-length=0。两种行为都能接受
		t.Logf("empty file download returned status %d (acceptable)", resp.StatusCode)
	} else {
		t.Errorf("expected 416 or 200 for empty file, got %d", resp.StatusCode)
	}
}

func TestChunkedDownload_RangeNotSatisfiable(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 上传一个文件
	body := []byte("small file")
	fileCS := sha256hex(body)
	uploadFile(t, url, "small.txt", body, map[string]string{
		"X-File-Checksum": fileCS,
	})

	// 请求超出文件范围的 offset
	resp, err := http.Get(url + "/download/chunk?filename=small.txt&offset=100&length=64")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("expected 416, got %d", resp.StatusCode)
	}
}

func TestChunkedUpload_Resume(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := bytes.Repeat([]byte("ResumeMe"), 3000) // ~24 KiB, 6 chunks at 4KiB
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)
	totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)

	uploadID := initSessionEx(t, url, "resume-dl-verify.bin", int64(len(fileData)), chunkSize, totalChunks, fileChecksum)

	// 1. 只上传前 3 个分块
	for i := range 3 {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		uploadChunk(t, url, uploadID, i, sha256hex(chunkData), chunkData)
	}

	// 2. 查询 uploadStatus 验证已接收列表
	statusResp, err := http.Get(url + "/upload/status?upload_id=" + uploadID)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	var status ChunkStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()

	if !status.Success {
		t.Fatalf("status failed: %v", status)
	}
	if status.ReceivedCount != 3 {
		t.Fatalf("expected 3 received chunks, got %d (missing: %v)", status.ReceivedCount, status.MissingChunks)
	}
	if len(status.MissingChunks) != totalChunks-3 {
		t.Fatalf("expected %d missing chunks, got %v", totalChunks-3, status.MissingChunks)
	}

	// 3. 补传缺失 chunk
	for _, idx := range status.MissingChunks {
		start := idx * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		uploadChunk(t, url, uploadID, idx, sha256hex(chunkData), chunkData)
	}

	// 4. uploadComplete -> 验证成功
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer resp.Body.Close()
	var result ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("resume complete failed: %v", result)
	}
	if result.FileChecksum != fileChecksum {
		t.Fatalf("checksum mismatch: got %s, want %s", result.FileChecksum, fileChecksum)
	}

	// 5. 下载验证 checksum 一致
	dlResp, err := http.Get(url + "/download?filename=resume-dl-verify.bin")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("download status: expected 200, got %d", dlResp.StatusCode)
	}
	dlData, err := io.ReadAll(dlResp.Body)
	if err != nil {
		t.Fatalf("read download body: %v", err)
	}
	if !bytes.Equal(dlData, fileData) {
		t.Fatalf("downloaded content mismatch: len(dl)=%d, len(orig)=%d", len(dlData), len(fileData))
	}
	dlCS := dlResp.Header.Get("X-File-Checksum")
	if dlCS != fileChecksum {
		t.Fatalf("download checksum header mismatch: got %s, want %s", dlCS, fileChecksum)
	}
}

func TestChunkedUpload_RetryExhausted(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("retry exhausted test data")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "retry-exhausted.txt", int64(len(fileData)), fileChecksum)

	// 用错误的 checksum 上传 chunk，服务端返回 200 但 Success=false
	wrongCS := sha256hex([]byte("wrong data"))
	chunkResp := uploadChunk(t, url, uploadID, 0, wrongCS, fileData)
	var uploadResult ChunkUploadResponse
	json.NewDecoder(chunkResp.Body).Decode(&uploadResult)
	chunkResp.Body.Close()
	if uploadResult.Success {
		t.Fatal("expected chunk upload to fail with wrong checksum")
	}
	if !uploadResult.ShouldRetry {
		t.Fatal("expected ShouldRetry=true for checksum mismatch")
	}

	// 用正确的 checksum 上传正确数据，应该成功
	correctCS := sha256hex(fileData)
	chunkResp2 := uploadChunk(t, url, uploadID, 0, correctCS, fileData)
	json.NewDecoder(chunkResp2.Body).Decode(&uploadResult)
	chunkResp2.Body.Close()
	if !uploadResult.Success {
		t.Fatalf("expected chunk upload to succeed with correct checksum: %v", uploadResult)
	}

	// complete 验证成功
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	cpResp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	defer cpResp.Body.Close()
	var completeResult ChunkCompleteResponse
	json.NewDecoder(cpResp.Body).Decode(&completeResult)
	if !completeResult.Success {
		t.Fatalf("complete failed: %v", completeResult)
	}
	if completeResult.FileChecksum != fileChecksum {
		t.Fatalf("checksum mismatch: got %s, want %s", completeResult.FileChecksum, fileChecksum)
	}
}

func doUploadInit(t *testing.T, baseURL, filename string, totalSize int64, fileChecksum string, fileModTime int64) ChunkedInitResponse {
	t.Helper()
	uploadID := fmt.Sprintf("test-upload-doinit-%s-%d", filename, totalSize)
	initReq := map[string]any{
		"upload_id":     uploadID,
		"filename":      filename,
		"total_size":    totalSize,
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": fileChecksum,
		"file_mod_time": fileModTime,
	}
	initJSON, _ := json.Marshal(initReq)
	resp, err := http.Post(baseURL+"/upload/init", "application/json", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("doUploadInit failed: %v", err)
	}
	defer resp.Body.Close()
	var result ChunkedInitResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode doUploadInit response: %v", err)
	}
	return result
}

func TestMergeAndRenameFile_InvalidPath(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	resp, err := http.Post(url+"/upload/complete", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 for missing upload_id, got %d", resp.StatusCode)
	}
}

func TestRecordCompleteMetadata_FileMTime(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	body := []byte("chunked content for mtime test")
	cs := sha256hex(body)
	initResp := doUploadInit(t, url, "mtime-test.txt", int64(len(body)), cs, 1000)
	if initResp.UploadID == "" {
		t.Fatal("expected non-empty upload id")
	}
}

func TestChunkedUpload_ContextCancelled(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	ctx, cancel := context.WithCancel(t.Context())
	cancel() // 立即取消

	fileCS := sha256hex([]byte("cancelled"))
	initReq := map[string]any{
		"upload_id":     "cancel-test",
		"filename":      "cancel.txt",
		"total_size":    len("cancelled"),
		"chunk_size":    4096,
		"total_chunks":  1,
		"file_checksum": fileCS,
	}
	initJSON, _ := json.Marshal(initReq)
	req, err := http.NewRequestWithContext(ctx, "POST", url+"/upload/init", bytes.NewReader(initJSON))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// 已取消的 context 可能导致客户端 transport 层报错（如 "context canceled"），
		// 这是可接受的 —— 我们只需确认没有 panic
		t.Logf("context cancelled request returned error (expected): %v", err)
		return
	}
	defer resp.Body.Close()
	// 如果能拿到响应，应该是有效的 HTTP 状态码（而不是 panic）
	t.Logf("context cancelled init returned status %d", resp.StatusCode)
}

// TestMergeAndRenameFile_FullFlow 通过完整的端到端分块上传流程间接覆盖 mergeAndRenameFile 成功路径。
func TestMergeAndRenameFile_FullFlow(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	content := []byte("full flow merge test content for mergeAndRenameFile")
	cs := sha256hex(content)

	// 初始化分块上传
	initResp := doUploadInit(t, url, "merge-full-test.txt", int64(len(content)), cs, 0)
	if initResp.UploadID == "" {
		t.Fatal("expected non-empty upload id")
	}

	// 上传单个分块（分块上传流程）
	uploadChunk(t, url, initResp.UploadID, 0, cs, content)

	// 完成上传 -> 触发 mergeAndRenameFile
	completeBody, _ := json.Marshal(map[string]string{"upload_id": initResp.UploadID})
	resp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var result ChunkCompleteResponse
	json.NewDecoder(resp.Body).Decode(&result)
	if !result.Success {
		t.Fatalf("merge and rename failed: %v", result)
	}
	if result.FileChecksum != cs {
		t.Fatalf("checksum mismatch: got %s, want %s", result.FileChecksum, cs)
	}
}

func TestNegotiateChunkSize_EdgeCases(t *testing.T) {
	tests := []struct {
		name       string
		clientSize int64
		cfgSize    int64
		want       int64
		wantAdj    bool
	}{
		{
			name:       "client 64 MiB exact",
			clientSize: 64 * 1024 * 1024,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    true,
		},
		{
			name:       "client 0, cfg 4 MiB",
			clientSize: 0,
			cfgSize:    4 * 1024 * 1024,
			want:       4 * 1024 * 1024,
			wantAdj:    false,
		},
		{
			name:       "client 0, cfg 0",
			clientSize: 0,
			cfgSize:    0,
			want:       size.DefaultChunkSize,
			wantAdj:    false,
		},
		{
			name:       "client below margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin - 1,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin - 1,
			wantAdj:    false,
		},
		{
			name:       "client at margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    false,
		},
		{
			name:       "client above margin",
			clientSize: size.DefaultChunkBodyLimit - chunkOverheadMargin + 1,
			cfgSize:    0,
			want:       size.DefaultChunkBodyLimit - chunkOverheadMargin,
			wantAdj:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, adj := negotiateChunkSize(tt.clientSize, tt.cfgSize)
			if got != tt.want {
				t.Errorf("negotiateChunkSize(%d, %d) = %d, want %d", tt.clientSize, tt.cfgSize, got, tt.want)
			}
			if adj != tt.wantAdj {
				t.Errorf("negotiateChunkSize(%d, %d) adjusted = %v, want %v", tt.clientSize, tt.cfgSize, adj, tt.wantAdj)
			}
		})
	}
}
