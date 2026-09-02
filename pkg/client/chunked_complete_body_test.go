// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// chunked_complete_body_test.go 覆盖任务 6 I-1：客户端 completeOnce 在非 2xx 时也要
// 先读响应体解析 MismatchChunks 再判错（此前 xfer/隧道模式非 mismatch 失败不解析响应体、
// MismatchChunks 丢失）。保证：纯文本非-JSON 响应体也返回包含 Message 的错误而非
// 只报"解析失败"，且 mismatch_chunks 能从 JSON body 中恢复（即使 status 非 2xx）。

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCompleteServer 是一个最小分块上传服务端：/upload/complete 行为可配置。
type fakeCompleteServer struct {
	completeStatus int    // 非 0 时 complete 返回该状态码
	completeBody   string // complete 响应体原样返回
	completeCalls  int
	mismatchAt     int // 第几次 complete 返回 mismatch；0 = 永不
	mismatchIdx    int
}

func newFakeCompleteServer() *fakeCompleteServer {
	return &fakeCompleteServer{completeStatus: http.StatusOK, completeBody: `{"success":true}`}
}

func (m *fakeCompleteServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"u1","chunk_size":4096,"total_chunks":2}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"ok"}`))
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, r *http.Request) {
		m.completeCalls++
		if m.mismatchAt > 0 && m.completeCalls == m.mismatchAt {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": "校验失败", "mismatch_chunks": []int{m.mismatchIdx},
			})
			return
		}
		if m.completeStatus != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(m.completeStatus)
			_, _ = w.Write([]byte(m.completeBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.completeBody))
	})
	return mux
}

// runCompleteOnce 用 fakeCompleteServer 构造一个 ChunkedUploader 并触发一次 complete。
func runCompleteOnce(t *testing.T, mock *fakeCompleteServer) (*ChunkedUploadResult, error) {
	t.Helper()
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()
	c := NewFileClient(srv.URL)
	c.chunkSize = 4096
	c.maxChunkSize = 4096
	c.logger = testLogger()
	u := newChunkedUploader(chunkedUploaderOpts{
		client: c, filePath: "x", uploadID: "u1",
		chunkSize: 4096, fileSize: 8192, totalChunks: 2,
		checksum: "abc", filename: "f.bin", concurrency: 1,
	})
	return u.completeOnce(context.Background())
}

// TestCompleteOnce_Non2xx_JSONBody_PreservesMismatchChunks 验证 complete 返回 400 + JSON
// body（含 mismatch_chunks）时 completeOnce 仍解析出 MismatchChunks，而非只报传输错误。
func TestCompleteOnce_Non2xx_JSONBody_PreservesMismatchChunks(t *testing.T) {
	mock := newFakeCompleteServer()
	mock.completeStatus = http.StatusBadRequest
	mock.completeBody = `{"success":false,"message":"boom","mismatch_chunks":[1]}`

	res, err := runCompleteOnce(t, mock)
	if err != nil {
		t.Fatalf("completeOnce 应返回解析后的结果（不含传输错误）: %v", err)
	}
	if res.Success {
		t.Fatal("success 应为 false（400）")
	}
	if len(res.MismatchChunks) != 1 || res.MismatchChunks[0] != 1 {
		t.Fatalf("MismatchChunks=%v want [1]（400 body 中应解析出 mismatch）", res.MismatchChunks)
	}
	if res.Message != "boom" {
		t.Fatalf("Message=%q want boom", res.Message)
	}
}

// TestCompleteOnce_Non2xx_NonJSONBody_NotTransmitError 验证 complete 返回非 JSON 纯文本
// body（旧服务端/异常）时 completeOnce 给出确定性错误（错误文本携带 body 而非只报
// "解析 failed"），调用方可从错误文本拿到服务端信息。
func TestCompleteOnce_Non2xx_NonJSONBody_NotTransmitError(t *testing.T) {
	mock := newFakeCompleteServer()
	mock.completeStatus = http.StatusInternalServerError
	mock.completeBody = "internal error: no json"

	res, err := runCompleteOnce(t, mock)
	if err == nil {
		t.Fatal("非 JSON 500 body 应返回错误")
	}
	if !strings.Contains(err.Error(), "internal error: no json") {
		t.Fatalf("错误应携带服务端 body 文本: %v", err)
	}
	_ = res // completeResult 零值返回（调用方只读 err；非 JSON body 无有效结果）
}

// TestChunkedUploader_Run_MismatchNon2xx_RetransmitsAndCompletes 验证 run() 处理
// mismatch（400 + mismatch_chunks）：先读 body 解析出 mismatch → 只重传坏分片 → 再 complete
// 成功（协议层"先读响应体再判错"在 run 循环中生效）。
func TestChunkedUploader_Run_MismatchNon2xx_RetransmitsAndCompletes(t *testing.T) {
	mock := newFakeCompleteServer()
	mock.mismatchAt = 1
	mock.mismatchIdx = 1
	content := bytes.Repeat([]byte("ab"), 5000) // 2 chunks @ 4096
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewFileClient(srv.URL)
	c.chunkSize = 4096
	c.maxChunkSize = 4096
	c.logger = testLogger()
	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
	u := newChunkedUploader(chunkedUploaderOpts{
		client: c, filePath: srcPath, uploadID: "u1",
		chunkSize: 4096, fileSize: int64(len(content)), totalChunks: 2,
		checksum: sha256hex(content), filename: "f.bin", concurrency: 1,
	})
	result, err := u.run(context.Background(), []int{0, 1})
	if err != nil {
		t.Fatalf("run 应通过 mismatch 重传成功: %v", err)
	}
	if !result.Success {
		t.Fatalf("run 应成功: %+v", result)
	}
	if mock.completeCalls != 2 {
		t.Fatalf("complete 调用次数=%d want 2（首次 mismatch + 重传后成功）", mock.completeCalls)
	}
}
