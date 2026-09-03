// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// chunked_mismatch_test.go 覆盖任务 5 客户端侧对 mismatch_chunks 的自动重传：
// /upload/complete 返回 400 + mismatch_chunks 后，ChunkedUploader.run 只重传服务端
// 报告的分片（mismatch 索引），非坏分片零重传，随后再次 complete 成功。
// 用 mock 服务端精确断言提交轨迹：首次全量 [0,1,2]，mismatch 后只补 [1]。完整端到端
// 流程（真实服务端 init→chunk→complete→mismatch→重传→complete）由 pkg/server 的
// TestCompleteFullVerifyAndMismatchChunks 覆盖。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// chunkAt 返回 content 中第 idx 分片（按 chunkSize 切分）。
func chunkAt(content []byte, chunkSize int64, idx int) []byte {
	start := idx * int(chunkSize)
	end := min(start+int(chunkSize), len(content))
	return content[start:end]
}

// mockMismatchChunkServer 是有界完整性的分块上传 mock 服务端（客户端 auto-retransmit 测试）。
//   - /upload/chunk：分片内容须与本地文件该片一致（否则 400），记录提交过的分片索引。
//     幂等：同分片重复提交直接成功（与服务端 bitmap 幂等一致）。
//   - /upload/complete：第 mismatchAt 次调用返回 400 + mismatch_chunks=[mismatchIdx]，
//     其余返回 success（模拟完整校验通过）。
type mockMismatchChunkServer struct {
	content       []byte
	chunkSize     int64
	mismatchAt    int   // 第几次 complete 返回 mismatch（>=1）；0 = 永不
	mismatchIdx   int   // 每次 mismatch 报告的分片
	submitted     []int // 每次 /upload/chunk 的分片索引（追加序）
	completeCalls int
	mu            sync.Mutex
}

func newMockMismatchChunkServer(content []byte, chunkSize int64, mismatchAt, mismatchIdx int) *mockMismatchChunkServer {
	return &mockMismatchChunkServer{content: content, chunkSize: chunkSize, mismatchAt: mismatchAt, mismatchIdx: mismatchIdx}
}

func (m *mockMismatchChunkServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			UploadID    string `json:"upload_id"`
			TotalSize   int64  `json:"total_size"`
			ChunkSize   int64  `json:"chunk_size"`
			TotalChunks int    `json:"total_chunks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "upload_id": req.UploadID,
			"chunk_size": m.chunkSize, "total_chunks": req.TotalChunks,
		})
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var idx int
		fmt.Sscanf(r.FormValue("chunk_index"), "%d", &idx)
		chkCS := r.FormValue("chunk_checksum")
		file, _, err := r.FormFile("chunk")
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(file)
		file.Close()
		if idx < 0 || idx >= (len(m.content)+int(m.chunkSize)-1)/int(m.chunkSize) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		want := chunkAt(m.content, m.chunkSize, idx)
		if chkCS != sha256hex(want) || !bytes.Equal(data, want) {
			_, _ = w.Write([]byte(`{"success":false,"message":"chunk checksum mismatch"}`))
			return
		}
		m.mu.Lock()
		m.submitted = append(m.submitted, idx)
		m.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "message": "ok"})
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.completeCalls++
		call := m.completeCalls
		m.mu.Unlock()
		if m.mismatchAt > 0 && call == m.mismatchAt {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": false, "message": fmt.Sprintf("%d 个分片校验失败", 1),
				"mismatch_chunks": []int{m.mismatchIdx},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true, "file_checksum": sha256hex(m.content), "message": "complete",
		})
	})
	return mux
}

func (m *mockMismatchChunkServer) submittedSnapshot() []int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]int, len(m.submitted))
	copy(out, m.submitted)
	return out
}

func (m *mockMismatchChunkServer) completeCallsSnapshot() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.completeCalls
}

// TestClientChunkedUploader_RetransmitMismatchChunksOnly 是任务 5 客户端契约核心测试：
// 服务端第 1 次 complete 返回 mismatch_chunks=[1]，run 随即只重传分片 1（0/2 零重传），
// 第 2 次 complete 成功。断言：提交轨迹 == [0,1,2,1]；complete 调用次数 == 2。
func TestClientChunkedUploader_RetransmitMismatchChunksOnly(t *testing.T) {
	content := make([]byte, 0, 9000)
	for i := range 9000 {
		content = append(content, byte(i%251))
	}
	chunkSize := int64(4096)
	mock := newMockMismatchChunkServer(content, chunkSize, 1 /*mismatchAt*/, 1 /*mismatchIdx*/)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewFileClient(srv.URL)
	c.chunkSize = chunkSize
	c.maxChunkSize = chunkSize
	c.logger = testLogger()

	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	totalChunks := (len(content) + int(chunkSize) - 1) / int(chunkSize)

	u := newChunkedUploader(chunkedUploaderOpts{
		client:      c,
		filePath:    srcPath,
		uploadID:    "client-mismatch-1",
		chunkSize:   chunkSize,
		fileSize:    int64(len(content)),
		totalChunks: totalChunks,
		checksum:    sha256hex(content),
		filename:    "f.bin",
		concurrency: 2,
	})
	all := make([]int, totalChunks)
	for i := range all {
		all[i] = i
	}
	result, err := u.run(t.Context(), all)
	if err != nil {
		t.Fatalf("ChunkedUploader.run: %v", err)
	}
	if !result.Success {
		t.Fatalf("run 应成功: %+v", result)
	}

	submitted := mock.submittedSnapshot()
	// 并发上传使阶段内到达序不确定；断言多重集：全量分片各提交一次 + 坏分片 1 多传一次。
	counts := make([]int, totalChunks)
	for _, idx := range submitted {
		if idx < 0 || idx >= totalChunks {
			t.Fatalf("提交了非法分片索引 %d: %v", idx, submitted)
		}
		counts[idx]++
	}
	for i, n := range counts {
		want := 1
		if i == mock.mismatchIdx {
			want = 2 // 初始 1 次 + mismatch 后重传 1 次
		}
		if n != want {
			t.Fatalf("分片 %d 提交 %d 次 want %d（分片 0/2 应只提交一次，0 重传）; 轨迹=%v",
				i, n, want, submitted)
		}
	}
	if len(submitted) != totalChunks+1 {
		t.Fatalf("提交总次数=%d want %d（全量 + 坏片重传 1 次）: %v", len(submitted), totalChunks+1, submitted)
	}
	if got := mock.completeCallsSnapshot(); got != 2 {
		t.Fatalf("complete 调用次数=%d want 2（首次 mismatch + 重传后成功）", got)
	}
}

// TestClientChunkedUploader_NoMismatch_SingleComplete 验证无 mismatch 时 complete 只调一次、
// 提交轨迹为全量分片（无多余重传）。
func TestClientChunkedUploader_NoMismatch_SingleComplete(t *testing.T) {
	content := bytes.Repeat([]byte("abc"), 3000)
	chunkSize := int64(4096)
	mock := newMockMismatchChunkServer(content, chunkSize, 0, 0)
	srv := httptest.NewServer(mock.handler())
	defer srv.Close()

	c := NewFileClient(srv.URL)
	c.chunkSize = chunkSize
	c.maxChunkSize = chunkSize
	c.logger = testLogger()

	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, content, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	totalChunks := (len(content) + int(chunkSize) - 1) / int(chunkSize)
	u := newChunkedUploader(chunkedUploaderOpts{
		client:      c,
		filePath:    srcPath,
		uploadID:    "client-nomismatch-1",
		chunkSize:   chunkSize,
		fileSize:    int64(len(content)),
		totalChunks: totalChunks,
		checksum:    sha256hex(content),
		filename:    "f.bin",
		concurrency: 2,
	})
	all := make([]int, totalChunks)
	for i := range all {
		all[i] = i
	}
	if _, err := u.run(t.Context(), all); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := mock.completeCallsSnapshot(); got != 1 {
		t.Fatalf("无 mismatch 时 complete 应只调 1 次, got %d", got)
	}
	submitted := mock.submittedSnapshot()
	if len(submitted) != totalChunks {
		t.Fatalf("提交轨迹应恰为全量 %d 个分片, got %v", totalChunks, submitted)
	}
}

var _ = multipart.NewWriter
