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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// newTestServerWithChunked 启动一个包含分块上传/下载路由的测试服务器。
func newTestServerWithChunked(t *testing.T, modifyCfg func(*Config)) (string, *atomic.Pointer[Config], func()) {
	t.Helper()

	tmpDir, err := os.MkdirTemp("", "sproxy-chunked-test-*")
	if err != nil {
		t.Fatalf("mktmp: %v", err)
	}

	cfg := Default()
	cfg.UploadsDir = tmpDir
	cfg.ChunkSize = 4 << 10           // 4 KiB for testing
	cfg.MaxChunkUploadBytes = 8 << 10 // 8 KiB
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
		uploadStore:   NewUploadStore(cfg.UploadsDir, nil),
		logger:        slog.Default(),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	mux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	mux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	mux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))

	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		h.uploadStore.Stop()
		_ = os.RemoveAll(tmpDir)
	}
	return ts.URL, &cfgPtr, cleanup
}

func TestUploadInit_HappyPath(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	body := []byte("test file content for chunked upload")
	fileChecksum := sha256hex(body)

	initReq := map[string]any{
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
	http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	// 完成
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

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
	http.Post(url+"/upload/chunk", mw.FormDataContentType(), &buf)
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

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
	http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

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

func initSession(t *testing.T, baseURL, filename string, totalSize int64, fileChecksum string) string {
	t.Helper()
	return initSessionEx(t, baseURL, filename, totalSize, 4096, int((totalSize+4095)/4096), fileChecksum)
}

func initSessionEx(t *testing.T, baseURL, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string) string {
	t.Helper()
	initReq := map[string]any{
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

	us := NewUploadStore(tmpDir, nil)
	defer us.Stop()

	session, err := us.CreateSession("test.txt", 100, 4096, 1, strings.Repeat("a", 64))
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

	us := NewUploadStore(tmpDir, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test.txt", 8192, 4096, 2, strings.Repeat("b", 64))

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

	us := NewUploadStore(tmpDir, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test.txt", 100, 4096, 1, strings.Repeat("c", 64))
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

	us := NewUploadStore(tmpDir, nil)
	defer us.Stop()

	session, _ := us.CreateSession("test.txt", 8192, 4096, 2, strings.Repeat("d", 64))
	us.MarkChunkReceived(session.UploadID, 0, "h0")

	missing := MissingChunks(us.GetSession(session.UploadID))
	if len(missing) != 1 || missing[0] != 1 {
		t.Fatalf("expected missing [1], got %v", missing)
	}
}

// ---- 通用 SHA-256 辅助 ----
// sha256hex 定义在 integration_test.go
