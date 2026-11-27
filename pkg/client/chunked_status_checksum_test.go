// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// chunked_status_checksum_test.go 锁「续传状态返回 Completed 时必须核对 checksum」。
//
// 背景（2026-11-27 对抗审查发现，P1）：
// 服务端 `checkFileExistsStatus` 只要**目标路径已有文件**就回 `Completed: true` 并带上
// **该文件**的 checksum（chunked_upload.go:1208 起），它回答的是「这个路径有完整文件」，
// 而不是「你要传的这份内容已经在上面」。
// 客户端 `tryResumeSession` 拿到 Completed 就**直接成功返回**、不比对 checksum ⇒
//   「把 B 覆盖写到一个已有 A 的路径」会被静默当成成功，实际内容仍是 A。
// 对上层（sdserver 重传/纠错、sclient 覆盖上传、任何 sync 工具）这是**静默数据不生效**：
// 返回 nil error、日志写「文件已存在」，用户以为已更新。
//
// 断言：状态回报的 checksum 与本次待传内容不同 ⇒ **不得**短路成功，必须继续新建会话
// （由服务端按自身策略覆盖或明确 409 —— 绝不静默成功）。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// TestChunkedUpload_ExistingDifferentChecksumNotShortCircuited 同名不同内容必须继续上传。
func TestChunkedUpload_ExistingDifferentChecksumNotShortCircuited(t *testing.T) {
	t.Parallel()
	var initN, chunkN, completeN atomic.Int32
	mux := http.NewServeMux()
	// 服务端说：该路径已有完整文件，但 checksum 是「旧内容」的。
	mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"completed":true,"file_checksum":"` +
			strings.Repeat("a", 64) + `","filename":"f.bin","message":"文件已存在"}`))
	})
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var req struct {
			UploadID string `json:"upload_id"`
		}
		_ = json.Unmarshal(body, &req)
		initN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"` + req.UploadID + `","chunk_size":1024}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
		chunkN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	mux.HandleFunc("POST /upload/complete", func(w http.ResponseWriter, _ *http.Request) {
		completeN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"upload_id":"x"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	local := filepath.Join(dir, "f.bin")
	// 内容与状态回报的 checksum（'a'*64）必然不同。
	if err := os.WriteFile(local, []byte("new-content-B"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient(ts.URL, WithAccessKey("ak", "sk"), WithAccessKeyID("skey-test01"))
	res, err := c.ChunkedUpload(t.Context(), local, "f.bin")
	if err != nil {
		// 服务端若选择 409（versioning 关闭时不允许覆盖），客户端如实报错也是正确行为；
		// 但**绝不能**是「成功」。此处 mock 的 init 是成功的，故不应走这里。
		t.Fatalf("同名不同内容时不应报错（mock 服务端允许覆盖）: %v", err)
	}
	if res != nil && res.UploadID == UploadIDAlreadyExists {
		t.Fatal("同名不同内容被当成「已存在」短路成功（内容未更新却返回成功）")
	}
	if initN.Load() != 1 {
		t.Fatalf("未新建上传会话（init=%d）：说明仍在用旧的「路径有文件即完成」短路", initN.Load())
	}
	if chunkN.Load() == 0 || completeN.Load() != 1 {
		t.Fatalf("未真正上传分块（chunk=%d complete=%d）", chunkN.Load(), completeN.Load())
	}
}

// TestChunkedUpload_ExistingSameChecksumStillShortCircuits 同名同内容仍走幂等短路
// （避免把「已存在且内容一致」也变成一次完整上传）。
func TestChunkedUpload_ExistingSameChecksumStillShortCircuits(t *testing.T) {
	t.Parallel()
	var chunkN atomic.Int32
	mux := http.NewServeMux()
	dir := t.TempDir()
	local := filepath.Join(dir, "same.bin")
	if err := os.WriteFile(local, []byte("same-content"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	raw := sha256.Sum256(b)
	sum := hex.EncodeToString(raw[:])
	mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"completed":true,"file_checksum":"` + sum + `","filename":"same.bin"}`))
	})
	mux.HandleFunc("POST /upload/chunk", func(w http.ResponseWriter, _ *http.Request) {
		chunkN.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	c := NewFileClient(ts.URL, WithAccessKey("ak", "sk"), WithAccessKeyID("skey-test01"))
	res, err := c.ChunkedUpload(t.Context(), local, "same.bin")
	if err != nil {
		t.Fatalf("同内容应幂等成功: %v", err)
	}
	// 短路的具体形态（服务端哨兵 `already_exists` 还是客户端派生的 session id）不是契约，
	// **「零分块传输 + 成功」**才是：内容已在云端，不该重传。
	if res == nil || !res.Success {
		t.Fatalf("同内容应幂等成功，实际 %+v", res)
	}
	if chunkN.Load() != 0 {
		t.Fatalf("同内容不应重传分块（chunk=%d）", chunkN.Load())
	}
}

// TestChunkedUpload_ConflictErrorIsActionable 锁「同名不同内容被拒时错误必须可操作」。
//
// 我们**刻意不**自动删除旧文件后重传（删除不可逆，SDK 不替调用方猜意图），因此错误文案
// 必须直接给出下一步：先删除/改名，或由服务端开启 versioning。否则用户只看到
// 「已存在同名文件」，不知道该怎么继续。
func TestChunkedUpload_ConflictErrorIsActionable(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /upload/status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false}`))
	})
	mux.HandleFunc("POST /upload/init", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"success":false,"message":"目标卷 \"main\" 已存在同名文件"}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	dir := t.TempDir()
	local := filepath.Join(dir, "c.bin")
	if err := os.WriteFile(local, []byte("conflict"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient(ts.URL, WithAccessKey("ak", "sk"), WithAccessKeyID("skey-test01"))
	_, err := c.ChunkedUpload(t.Context(), local, "c.bin")
	if err == nil {
		t.Fatal("同名不同内容应报错（服务端 409）")
	}
	msg := err.Error()
	for _, want := range []string{"409", "已存在", "删除/改名", "versioning"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("错误文案缺少可操作指引 %q: %s", want, msg)
		}
	}
}
