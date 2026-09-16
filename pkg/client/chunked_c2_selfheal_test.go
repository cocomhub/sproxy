// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestChunkedUpload_ReinitOnMissingTemp 钉住 CHUNK C-2 客户端自愈：upload 遇「会话缺少
// 在途临时文件」（旧磁盘遗留/篡改 ⇒ TempPath 为空）时，客户端**自动重新 init**（新 upload_id）
// 并全量重传，最终上传成功，而不是重试耗尽后失败/死循环。
//
// 服务端行为模拟：
//   - GET /upload/status ⇒ 404（无续传会话，走新 init）；
//   - POST /upload/init 收到**旧 id** ⇒ 200（建会话）；
//   - POST /upload/init 收到 **id-reinit**（自动 re-init 的新 id）⇒ 200（建新会话）；
//   - POST /upload/chunk 带**旧 id** ⇒ 500 + should_retry + 「缺少在途临时文件」；
//   - POST /upload/chunk 带**新 id** ⇒ 200 success；
//   - POST /upload/complete ⇒ 200 success（对任意 id）。
func TestChunkedUpload_ReinitOnMissingTemp(t *testing.T) {
	t.Parallel()

	const totalChunks = 3
	fileData := bytes.Repeat([]byte("C2"), testChunkSize*totalChunks)

	var (
		initOld   atomic.Int32 // init 旧 id 次数
		initNew   atomic.Int32 // init 新 id（-reinit）次数
		chunkOld  atomic.Int32 // chunk 旧 id 次数
		chunkNew  atomic.Int32 // chunk 新 id 次数
		completeN atomic.Int32
	)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"message":"not found"}`))
	})
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			UploadID string `json:"upload_id"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if idHasReinitSuffix(req.UploadID) {
			initNew.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"upload_id":"` + req.UploadID + `","chunk_size":1024}`))
			return
		}
		initOld.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"` + req.UploadID + `","chunk_size":1024}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseMultipartForm(1 << 20)
		uploadID := r.FormValue("upload_id")
		w.Header().Set("Content-Type", "application/json")
		if idHasReinitSuffix(uploadID) {
			chunkNew.Add(1)
			_, _ = w.Write([]byte(`{"success":true}`))
			return
		}
		chunkOld.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"success":false,"should_retry":true,"message":"上传会话缺少在途临时文件，请重新初始化"}`))
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, _ *http.Request) {
		completeN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"x","file_checksum":"abc"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	tmpDir := t.TempDir()
	filePath := filepath.Join(tmpDir, "c2.dat")
	if err := os.WriteFile(filePath, fileData, 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}

	c := NewFileClient(ts.URL)
	result, err := c.ChunkedUpload(t.Context(), filePath, "c2.dat", WithChunkedChunkSize(testChunkSize))
	if err != nil {
		t.Fatalf("ChunkedUpload failed: %v", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("expected success, got %+v", result)
	}
	if initOld.Load() != 1 {
		t.Fatalf("init old id calls = %d, want 1", initOld.Load())
	}
	if initNew.Load() != 1 {
		t.Fatalf("init reinit id calls = %d, want 1（自动 re-init）", initNew.Load())
	}
	if chunkNew.Load() < int32(totalChunks) {
		t.Fatalf("chunk with new id calls = %d, want >= %d（全量重传）", chunkNew.Load(), totalChunks)
	}
	if chunkOld.Load() == 0 {
		t.Fatal("expected at least one chunk call with old id (triggering the temp-missing detection)")
	}
	if completeN.Load() == 0 {
		t.Fatal("expected complete call")
	}
}

// idHasReinitSuffix 判断 upload_id 是否带自动 re-init 后缀（-reinit）。
func idHasReinitSuffix(id string) bool {
	return len(id) >= 7 && id[len(id)-7:] == "-reinit"
}
