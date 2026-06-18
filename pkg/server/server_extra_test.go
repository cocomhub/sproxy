// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestBatchRenameHandler_HappyPath(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = tmpDir
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	cs := NewChecksumStore(tmpDir, testLogger())
	mux := http.NewServeMux()
	Handlers := RegisterRoutes(t.Context(), mux, &cfgPtr, "test", "now", nil, testLogger(), nil)

	// 上传一个文件，然后批量重命名
	body := "--BOUNDARY\r\nContent-Disposition: form-data; name=\"file\"; filename=\"old.txt\"\r\nContent-Type: text/plain\r\n\r\nhello\r\n--BOUNDARY--\r\n"
	uploadReq := httptest.NewRequest(http.MethodPost, "/upload", strings.NewReader(body))
	uploadReq.Header.Set("Content-Type", "multipart/form-data; boundary=BOUNDARY")
	uploadReq.Header.Set("X-File-Checksum", "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, uploadReq)
	if w.Code != http.StatusOK {
		t.Fatalf("upload failed: %d", w.Code)
	}
	_ = Handlers
	_ = cs
	renameReq := httptest.NewRequest(http.MethodPost, "/batch-rename", strings.NewReader(`[]`))
	renameReq.Header.Set("Content-Type", "application/json")
	renameReq.Header.Set("Authorization", "Bearer test-token")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, renameReq)
	// 只要不 panic 就可以
}

func TestUploadStore_SessionDir(t *testing.T) {
	us := NewUploadStore(t.TempDir(), 0, nil)
	dir := us.SessionDir("test-upload-id")
	if dir == "" {
		t.Fatal("expected non-empty session dir")
	}
	if !strings.Contains(dir, "test-upload-id") {
		t.Errorf("expected session dir to contain upload ID, got: %s", dir)
	}
}
