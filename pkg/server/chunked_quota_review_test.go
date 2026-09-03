// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// chunked_quota_review_test.go 锁定配额磁盘封顶审查 B §8 的 6 项缺口（担保位）：
//  1. §8-B 非末片超长 chunk 防越界：BoundWriter(limit=该片实际长度) 截断直写，不污染邻区；
//  2. §8-C TempPath=="" 防御：chunk 请求 500 + ShouldRetry（chunked_upload.go:489-494 现行为）；
//  3. §8-C cleanupExpired 配额对账：删除临时名 + user 桶 Reserved 归零（DeleteSession 镜像强度）；
//  4. §8-F version 数据级备份：分块覆盖写 complete 后 version 桶 <id> 文件内容 == 覆盖前 old；
//  5. §8-E 并发分片 seek 真并发：多 goroutine 不同 offset 直写，无 -race 失败 + 内容正确；
//  6. §8-A/D init 边界：chunk_size*total_chunks < total_size → 400；total_size<=0 → 400。
//
// 纯标准库测试；127.0.0.1 回环（httptest）；并发测试留足 -race 余量。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestUploadChunk_OversizedMiddleChunk_BoundTruncates 覆盖 §8-B 非末片超长 chunk 防越界：
// 合法 init（3 片）后，把中间分片（chunk1，合法 4096 字节）发 4097 字节 → 服务端先校验实际
// 4097 字节 checksum、再经 BoundWriter(limit=4096) 截断直写 → 临时名 chunk1 区 == 4096 字节
// B、相邻 chunk0/chunk2 区保持原占位未被污染；客户端收到 200+success（非错误），协议符合
// 服务端"限长截断"语义。
func TestUploadChunk_OversizedMiddleChunk_BoundTruncates(t *testing.T) {
	url, cfgPtr, cleanup := newTestServerWithChunked(t, func(c *Config) { c.ChunkSize = 4 << 10 })
	defer cleanup()

	content := bytes.Repeat([]byte("A"), 9000) // 3 chunks（chunk2 为 808 字节末片）
	fileChecksum := sha256hex(content)
	uploadID := initSession(t, url, "oversize.bin", int64(len(content)), fileChecksum)

	// 合法 chunk0（4096 字节）。
	chunk0 := content[:4096]
	cRes := uploadChunk(t, url, uploadID, 0, sha256hex(chunk0), chunk0)
	var cr ChunkUploadResponse
	if err := json.NewDecoder(cRes.Body).Decode(&cr); err != nil {
		t.Fatalf("decode chunk0: %v", err)
	}
	if cRes.StatusCode != http.StatusOK || !cr.Success {
		t.Fatalf("chunk0 应 200+success, got %d: %+v", cRes.StatusCode, cr)
	}

	// 中间分片 chunk1 发 4097 字节（合法 4096 + 越界 1 字节）。客户端对**实际发送字节**做
	// checksum（服务端先校验再截断）——协议上越界字节通过校验后被 BoundWriter 截断。
	oversized := bytes.Repeat([]byte("B"), 4097)
	cRes1 := uploadChunk(t, url, uploadID, 1, sha256hex(oversized), oversized)
	var cr1 ChunkUploadResponse
	if err := json.NewDecoder(cRes1.Body).Decode(&cr1); err != nil {
		t.Fatalf("decode chunk1: %v", err)
	}
	if cRes1.StatusCode != http.StatusOK || !cr1.Success {
		t.Fatalf("chunk1(4097B) 应 200+success（限长截断而非拒绝）, got %d: %+v", cRes1.StatusCode, cr1)
	}
	if cr1.ChunkIndex != 1 {
		t.Fatalf("chunk1 ChunkIndex=%d want 1", cr1.ChunkIndex)
	}
	if cr1.ShouldRetry {
		t.Fatal("截断直写成功不应标 ShouldRetry")
	}

	// 读取临时名，断言逐片区域：chunk0 区 == chunk0、chunk1 区 == 4096B B、chunk2 区保持
	// 占位 0（越界字节未越过 limit 写坏相邻分片）。
	root := cfgPtr.Load().StorageRoot
	tempAbs := filepath.Join(root, "anonymous", "user", inflightTempNameFor("user/oversize.bin", uploadID))
	tempData, err := os.ReadFile(tempAbs)
	if err != nil {
		t.Fatalf("读取临时名: %v", err)
	}
	if len(tempData) != 9000 {
		t.Fatalf("临时名长度=%d want 9000（Truncate 预占）", len(tempData))
	}
	if !bytes.Equal(tempData[:4096], chunk0) {
		t.Fatal("chunk0 区内容被越界写污染")
	}
	if !bytes.Equal(tempData[4096:8192], bytes.Repeat([]byte("B"), 4096)) {
		t.Fatal("chunk1 区（限长 4096）内容不正确——越界字节未截断")
	}
	if !bytes.Equal(tempData[8192:9000], make([]byte, 808)) {
		t.Fatal("越界写污染了相邻 chunk2 占位区（BoundWriter 未截断）")
	}
}

// TestUploadChunk_TempPathEmpty_ReturnsRetry500 覆盖 §8-C TempPath=="" 防御（旧磁盘遗留/
// 篡改会话）：手工构造 TempPath=="" 的 session（等价 init 后临时名缺失/被篡改）→ chunk 请求
// 返回 500 + ShouldRetry=true（chunked_upload.go:489-494 现行为），提示客户端重试 init 重建
// 临时名，不静默吞掉分片。
func TestUploadChunk_TempPathEmpty_ReturnsRetry500(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	// 直接以无 TempPath 的会话模拟"临时名缺失"状态（t 命名经 ValidSegmentName 合法）。
	uploadID := "temp-empty-sess"
	content := bytes.Repeat([]byte("R"), 1234)
	session, err := env.h.uploadStoreFor("alice").CreateSession(uploadID, "tempempty.bin",
		int64(len(content)), 4096, 1, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if session.TempPath != "" {
		t.Fatalf("预置会话 TempPath 应为空, got %q", session.TempPath)
	}

	// chunk 请求 → 500 + ShouldRetry（TempPath=="" 防御分支）。
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("upload_id", uploadID)
	_ = mw.WriteField("chunk_index", "0")
	_ = mw.WriteField("chunk_checksum", sha256Hex(content))
	part, _ := mw.CreateFormFile("chunk", "chunk.bin")
	_, _ = part.Write(content)
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/upload/chunk", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	env.mux["alice"].ServeHTTP(rr, req)
	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("TempPath==\"\" chunk 应 500, got %d body=%s", rr.Code, rr.Body.String())
	}
	var cr ChunkUploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rr.Body.String())
	}
	if !cr.ShouldRetry {
		t.Fatalf("TempPath==\"\" chunk 应 ShouldRetry=true（提示重新 init）, got %+v", cr)
	}
	if !strings.Contains(cr.Message, "重新初始化") {
		t.Fatalf("错误信息应提示重试 init, got %q", cr.Message)
	}
}

// TestUploadStore_CleanupExpired_RemovesTempFile 覆盖 §8-C cleanupExpired 配额对账：
// 负 TTL 会话（ExpiresAt 过去）+ session.TempPath 指向真实临时名 → 触发过期清理后
// 临时文件被删除 + user 桶 Reserved 归零（镜像 TestUploadStore_DeleteSession_RemovesTempFile
// :375 的断言强度，配额账本不因过期路径泄漏）。
func TestUploadStore_CleanupExpired_RemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	h := newChunkedTestHandlers(t, dir, 4096)
	t.Cleanup(func() { _ = h.Close() })

	// 负 TTL：ExpiresAt = now.Add(负) → 已过期，cleanupExpired 立即清理（memory:
	// negative-ttl-for-testing）。必须在首次 uploadStoreFor 前设置（store 懒创建时读取）。
	cfg := h.cfgPtr.Load()
	cfg.UploadSessionTTL = -1 * time.Hour
	h.cfgPtr.Store(cfg)

	us := h.uploadStoreFor("alice")
	uploadID := "expired-tempfile"
	content := bytes.Repeat([]byte("x"), 100)
	scope := h.quotaBucketFor("alice", "user")
	rr, err := scope.TryReserve(int64(len(content)))
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	session, err := us.CreateSession(uploadID, "expired.bin", int64(len(content)), 4096, 1,
		sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session.TempPath = "user/" + inflightTempNameFor(mustUserRel(t, h, "alice", "expired.bin"), uploadID)
	session.Reservation = rr
	us.MarkChunkReceived(uploadID, 0, sha256Hex(content))

	// 在 user 桶写一个临时名文件（模拟在途数据，位于目标同目录）。
	tnt := h.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	tempAbs := filepath.Join(userAbs, inflightTempNameFor(mustUserRel(t, h, "alice", "expired.bin"), uploadID))
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("mkdir user: %v", err)
	}
	if err := os.WriteFile(tempAbs, content, 0o600); err != nil {
		t.Fatalf("写临时名: %v", err)
	}

	// 触发过期清理（直接调用，不等 5 分钟 ticker）。
	us.cleanupExpired()

	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("cleanupExpired 后临时名应被删除（stat err=%v）", err)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("cleanupExpired 后 user 桶 Reserved()=%d want 0（预留已归还）", got)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("cleanupExpired 后 user 桶 Usage()=%d want 0（无泄漏）", got)
	}
	if us.GetSession(uploadID) != nil {
		t.Fatal("cleanupExpired 后 session 应从 map 移除")
	}
}

// TestCompleteOverwrite_VersionBackup 覆盖 §8-F version 数据级备份：分块覆盖写（versioning
// 开启 + 同名旧文件 checksum 不同）complete 后 → 断言 version 桶 `f.txt/<id>` 文件**内容** ==
// 覆盖前 old 内容（现有测试只断 version 桶 Usage==60，此处补数据级备份：不只记账，旧文件
// 字节确实落盘可恢复）。
func TestCompleteOverwrite_VersionBackup(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	env.h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 1000}
	env.h.cfgPtr.Load().Versioning.Enabled = true
	env.h.cfgPtr.Load().Versioning.MaxVersions = 10

	old := []byte(strings.Repeat("a", 60))
	umux := actorUploadDeleteMux(env.h, "alice")
	if code, resp := uploadAs(t, umux, "ov.bin", old); code != http.StatusOK {
		t.Fatalf("预置文件上传应 200, got %d: %s", code, resp)
	}

	// 分块覆盖写（versioning 开启：同名旧文件 checksum 不同 → 覆盖语义）。
	newContent := []byte("b")
	fileChecksum := sha256Hex(newContent)
	uploadID := "complete-ov-version"
	code, resp := env.initAs(t, "alice", uploadID, "ov.bin", int64(len(newContent)), 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	if c := env.chunkAs(t, "alice", uploadID, 0, newContent); c != http.StatusOK {
		t.Fatalf("chunk 应 200, got %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}

	// 最终 user 文件 == 新内容。
	finalPath := filepath.Join(env.dir, "alice", "user", "ov.bin")
	finalData, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("覆盖后文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, newContent) {
		t.Fatal("覆盖后 user 文件内容不正确")
	}

	// 数据级备份断言：version 桶 <id> 文件内容 == 覆盖前 old（而不仅是 Usage 记账 60）。
	verDir := filepath.Join(env.dir, "alice", "version", "ov.bin")
	entries, err := os.ReadDir(verDir)
	if err != nil {
		t.Fatalf("读取版本目录失败: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("应恰好 1 个版本（旧文件备份）, got %d", len(entries))
	}
	verData, err := os.ReadFile(filepath.Join(verDir, entries[0].Name()))
	if err != nil {
		t.Fatalf("读取版本文件失败: %v", err)
	}
	if !bytes.Equal(verData, old) {
		t.Fatalf("version 桶版本文件内容 != 覆盖前 old（len=%d want %d）——数据级备份缺失",
			len(verData), len(old))
	}
	// 配额账本断言（现有测试强度保留）：version 桶 Usage == old 大小、user 桶收敛到新大小。
	if got := env.h.quotaBucketFor("alice", "version").Usage(); got != int64(len(old)) {
		t.Fatalf("覆盖后 version 桶 Usage()=%d want %d（旧版本已备份）", got, len(old))
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != int64(len(newContent)) {
		t.Fatalf("覆盖后 user 桶 Usage()=%d want %d（ReleaseUsage(old)+commit(new)）", got, len(newContent))
	}
}

// TestChunkedUpload_ConcurrentChunks_Race 覆盖 §8-E 并发分片 seek 真并发：多 goroutine 并发
// 上传不同 offset 分片（uploadChunk → writeChunkDirect → BoundWriter 直写临时名，非仅内存
// MarkChunkReceived）→ 在 -race 下无数据竞争 + 内容逐片正确（临时名/最终文件 == 原文件）
// + complete 成功。每个请求独立通道 + 收集式错误上报（子 goroutine 内不 t.Fatal）。
func TestChunkedUpload_ConcurrentChunks_Race(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := make([]byte, 9000)
	for i := range content {
		content[i] = byte(i % 251)
	}
	fileChecksum := sha256Hex(content)
	chunkSize := int64(4096)
	totalChunks := 3
	uploadID := "concurrent-seek"
	code, resp := env.initAs(t, "alice", uploadID, "conc.bin", int64(len(content)), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}

	// 并发真 seek 直写：3 个 goroutine 同时走 uploadChunk（各自独立请求/句柄，offset 不同）。
	var wg sync.WaitGroup
	errCh := make(chan error, totalChunks)
	for i := range totalChunks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			start := idx * int(chunkSize)
			end := min(start+int(chunkSize), len(content))
			if c := env.chunkAs(t, "alice", uploadID, idx, content[start:end]); c != http.StatusOK {
				errCh <- fmt.Errorf("chunk %d 应 200, got %d", idx, c)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发分片上传失败: %v", err)
	}

	// 临时名内容逐片正确（== 原文件）。
	rel, _ := env.h.tenantFor("alice").UserRel("conc.bin")
	tempAbs := filepath.Join(env.dir, "alice", "user", inflightTempNameFor(rel, uploadID))
	tempData, err := os.ReadFile(tempAbs)
	if err != nil {
		t.Fatalf("读取临时名: %v", err)
	}
	if !bytes.Equal(tempData, content) {
		t.Fatalf("并发 seek 直写后临时名内容 != 原文件（len=%d want %d）", len(tempData), len(content))
	}

	// complete 成功 → 最终文件 == 原文件。
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}
	finalData, err := os.ReadFile(filepath.Join(env.dir, "alice", "user", "conc.bin"))
	if err != nil {
		t.Fatalf("最终文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, content) {
		t.Fatalf("并发上传 complete 后最终文件内容 != 原文件（len=%d want %d）", len(finalData), len(content))
	}
}

// TestUploadInit_ChunkSpanningBounds_Reject 覆盖 §8-A init 边界：chunk_size*total_chunks
// 无法容纳 total_size（declared 容量不足）→ 400 拒绝（不创建会话/预留）。幂等校验先行，
// 无需四舍五入整定。
func TestUploadInit_ChunkSpanningBounds_Reject(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()

	// 2 chunks * 4096 = 8192 < total_size 9000 → 无法容纳 → 400。
	const okChecksum = "0000000000000000000000000000000000000000000000000000000000000000"
	w := postInit(t, h, `{"upload_id":"span1","filename":"span.bin","total_size":9000,"chunk_size":4096,"total_chunks":2,"file_checksum":"`+okChecksum+`"}`)
	assertStatusEq(t, w, http.StatusBadRequest)
	assertMsgContains(t, w, "chunk_size * total_chunks")

	// 边界恰好容纳（chunk_size*total_chunks == total_size）不应 400（合法性对照，非越界）。
	wOK := postInit(t, h, `{"upload_id":"span-ok","filename":"span-ok.bin","total_size":8192,"chunk_size":4096,"total_chunks":2,"file_checksum":"`+okChecksum+`"}`)
	assertStatusEq(t, wOK, http.StatusOK)
}

// TestUploadInit_EmptyFile_Rejected400 覆盖 §8-D init 边界：total_size<=0（空文件/负大小）
// → 400 拒绝（不创建会话）。
func TestUploadInit_EmptyFile_Rejected400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestServerWithUploadInit(t, nil)
	defer cleanup()

	const okChecksum = "0000000000000000000000000000000000000000000000000000000000000000"
	tests := []struct {
		name string
		json string
	}{
		{name: "zero size", json: `{"upload_id":"empty0","filename":"empty0.bin","total_size":0,"chunk_size":4096,"total_chunks":1,"file_checksum":"` + okChecksum + `"}`},
		{name: "negative size", json: `{"upload_id":"emptyn","filename":"emptyn.bin","total_size":-5,"chunk_size":4096,"total_chunks":1,"file_checksum":"` + okChecksum + `"}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := postInit(t, h, tc.json)
			assertStatusEq(t, w, http.StatusBadRequest)
			assertMsgContains(t, w, "total_size")
		})
	}
}
