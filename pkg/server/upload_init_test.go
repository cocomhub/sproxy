// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// newTestServerWithUploadInit 返回一个包含 chunked upload handler 的 Handlers 实例。
func newTestServerWithUploadInit(t *testing.T, modifyCfg func(*Config)) (*Handlers, *atomic.Pointer[Config], func()) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = tmpDir
	if modifyCfg != nil {
		modifyCfg(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	cs := NewChecksumStore(cfg.UploadsDir, nil)
	store := NewUploadStore(cfg.UploadsDir, cfg.UploadSessionTTL, nil)
	h := &Handlers{
		cfgPtr:        &cfgPtr,
		uploadStore:   store,
		checksumStore: cs,
		logger:        slog.Default(),
	}
	cleanup := func() {
		store.Stop()
	}
	return h, &cfgPtr, cleanup
}

// postInit 发送 upload/init POST 请求，返回 ResponseRecorder。
func postInit(t *testing.T, h *Handlers, bodyJSON string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", "/upload/init", strings.NewReader(bodyJSON))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.authMiddleware(http.HandlerFunc(h.uploadInit)).ServeHTTP(w, req)
	return w
}

// assertStatusEq 检查响应状态码。
func assertStatusEq(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Errorf("status = %d, want %d; body=%s", w.Code, want, w.Body.String())
	}
}

// assertMsgContains 检查 JSON 响应体 message 字段包含子串。
func assertMsgContains(t *testing.T, w *httptest.ResponseRecorder, substr string) {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("json decode: %v; body=%s", err, w.Body.String())
	}
	msg, _ := m["message"].(string)
	if !strings.Contains(msg, substr) {
		t.Errorf("message = %q, want substring %q; body=%s", msg, substr, w.Body.String())
	}
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestUploadInit_MissingUploadID(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "缺少 upload_id")
}

func TestUploadInit_InvalidTotalSize(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{"upload_id":"id1","filename":"f.txt","total_size":0,"chunk_size":4096,"total_chunks":1,"file_checksum":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "total_size 必须大于 0")
}

func TestUploadInit_InvalidChunkSize(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{"upload_id":"id2","filename":"f.txt","total_size":100,"chunk_size":0,"total_chunks":1,"file_checksum":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "chunk_size 必须大于 0")
}

func TestUploadInit_InvalidTotalChunks(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{"upload_id":"id3","filename":"f.txt","total_size":100,"chunk_size":4096,"total_chunks":0,"file_checksum":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "total_chunks 必须大于 0")
}

func TestUploadInit_ChunkSizeTimesTotalChunksLtTotalSize(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{"upload_id":"id4","filename":"f.txt","total_size":100,"chunk_size":30,"total_chunks":3,"file_checksum":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "chunk_size * total_chunks")
}

func TestUploadInit_NonHexChecksum(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()
	w := postInit(t, h, `{"upload_id":"id5","filename":"f.txt","total_size":100,"chunk_size":4096,"total_chunks":1,"file_checksum":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`)
	assertStatusEq(t, w, 400)
	assertMsgContains(t, w, "不是有效的 hex")
}

func TestUploadInit_ChecksumMismatchConflict(t *testing.T) {
	t.Parallel()
	uploadsDir := t.TempDir()
	h, _, cleanup := newTestServerWithUploadInit(t, func(c *Config) {
		c.UploadsDir = uploadsDir
	})
	defer cleanup()
	writeFile(t, filepath.Join(uploadsDir, "existing.txt"), []byte("some content"))
	w := postInit(t, h, `{"upload_id":"id6","filename":"existing.txt","total_size":12,"chunk_size":4096,"total_chunks":1,"file_checksum":"0000000000000000000000000000000000000000000000000000000000000000"}`)
	assertStatusEq(t, w, 409)
	assertMsgContains(t, w, "checksum 不匹配")
}

func TestUploadInit_FileAlreadyExistsChecksumMatch(t *testing.T) {
	t.Parallel()
	uploadsDir := t.TempDir()
	h, _, cleanup := newTestServerWithUploadInit(t, func(c *Config) {
		c.UploadsDir = uploadsDir
	})
	defer cleanup()

	filePath := filepath.Join(uploadsDir, "exists.txt")
	content := []byte("hello world")
	writeFile(t, filePath, content)
	if err := os.WriteFile(filepath.Join(uploadsDir, ".checksums.json"), []byte(`{"exists.txt":"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}`), 0644); err != nil {
		t.Fatal(err)
	}

	w := postInit(t, h, `{"upload_id":"id7","filename":"exists.txt","total_size":11,"chunk_size":4096,"total_chunks":1,"file_checksum":"b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9"}`)
	assertStatusEq(t, w, 200)
	assertMsgContains(t, w, "文件已存在")
}
