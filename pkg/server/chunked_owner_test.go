// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// chunked_owner_test.go 验证分块上传在多租户（owner 认证）下的 session key 隔离。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// actorChunkedMux 构造把固定 actor 注入请求 ctx 后转发分块上传 handler 的 mux。
// 模拟 authMiddleware 验签后 withActor 的行为（复用 cloud_owner_test.go 的模式）。
func actorChunkedMux(h *Handlers, actor string) *http.ServeMux {
	wrap := func(hf http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(withActor(r.Context(), actor))
			hf(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload/init", wrap(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", wrap(h.uploadChunk))
	mux.HandleFunc("POST /upload/complete", wrap(h.uploadComplete))
	mux.HandleFunc("GET /upload/status", wrap(h.uploadStatus))
	return mux
}

// ownerChunkedEnv 提供共享 Handlers 的多 actor 分块上传测试环境。
type ownerChunkedEnv struct {
	h   *Handlers
	dir string
	mux map[string]*http.ServeMux // actor → mux
}

// newOwnerChunkedEnv 创建多 actor 分块上传环境。
func newOwnerChunkedEnv(t *testing.T) *ownerChunkedEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = dir
	cfg.ChunkSize = 4 << 10 // 4 KiB 小分块便于单文件多 chunk 测试
	cfgPtr := newTestCfgPtr(dir)
	cfgPtr.Store(cfg)

	h := &Handlers{
		cfgPtr:        cfgPtr,
		checksumStore: NewChecksumStore(dir, testLogger()),
		uploadStore:   MustNewUploadStore(dir, 24*3600*1e9, testLogger()), // 24h
		logger:        testLogger(),
		auditLogger:   testLogger(),
		uploadingStop: make(chan struct{}),
	}
	t.Cleanup(func() {
		close(h.uploadingStop) // 先停 cleanup goroutine，再等其退出
		h.uploadingWg.Wait()
		h.uploadStore.Stop()
		os.RemoveAll(filepath.Join(dir, ".__chunked__"))
	})
	return &ownerChunkedEnv{
		h:   h,
		dir: dir,
		mux: map[string]*http.ServeMux{
			"ak-A": actorChunkedMux(h, "ak-A"),
			"":     actorChunkedMux(h, ""),
		},
	}
}

// initAs 以指定 actor 发起 /upload/init，返回完整响应体。
func (e *ownerChunkedEnv) initAs(t *testing.T, actor, uploadID, filename string, totalSize, chunkSize int64, totalChunks int, checksum string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"upload_id":     uploadID,
		"filename":      filename,
		"total_size":    totalSize,
		"chunk_size":    chunkSize,
		"total_chunks":  totalChunks,
		"file_checksum": checksum,
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/upload/init", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp
}

// chunkAs 以指定 actor 上传单个分块。
func (e *ownerChunkedEnv) chunkAs(t *testing.T, actor, uploadID string, idx int, data []byte) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("upload_id", uploadID)
	_ = mw.WriteField("chunk_index", strconv.Itoa(idx))
	_ = mw.WriteField("chunk_checksum", sha256Hex(data))
	part, _ := mw.CreateFormFile("chunk", "chunk.bin")
	_, _ = part.Write(data)
	_ = mw.Close()

	req := httptest.NewRequest("POST", "/upload/chunk", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	return rr.Code
}

// completeAs 以指定 actor 完成上传。
func (e *ownerChunkedEnv) completeAs(t *testing.T, actor, uploadID string) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	req := httptest.NewRequest("POST", "/upload/complete", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp
}

// TestChunkedUploadOwner_SessionKeyScopedByOwner 验证带 owner 认证的分块上传全流程：
// init 返回带 owner 前缀的 upload_id，chunk/complete/status 沿用该 id 必须成功；
// 伪造 id 或他人 owner 前缀必须被拒绝（防 IDOR）。
func TestChunkedUploadOwner_SessionKeyScopedByOwner(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	filename := "dir/owner-data.bin"
	totalSize := 6000
	chunkSize := int64(4096)
	totalChunks := 2
	totalData := make([]byte, 0, totalSize)
	for i := range totalSize {
		totalData = append(totalData, byte(i%251))
	}
	fileChecksum := sha256Hex(totalData)

	bareID := "aaaa1111bbbb2222cccc3333dddd4444"
	code, resp := env.initAs(t, "ak-A", bareID, filename, int64(totalSize), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("owner init 失败: %d %v", code, resp)
	}
	uploadID, _ := resp["upload_id"].(string)
	if uploadID == bareID {
		t.Fatalf("owner init 应返回带 owner 前缀的 upload_id（防跨租户碰撞），got %q", uploadID)
	}
	if !strings.HasPrefix(uploadID, "ak-A/") {
		t.Fatalf("owner 前缀缺失: got %q, want 前缀 ak-A/", uploadID)
	}

	// 用带前缀 id 上传分块
	chunk0 := totalData[:4096]
	chunk1 := totalData[4096:]
	if c := env.chunkAs(t, "ak-A", uploadID, 0, chunk0); c != http.StatusOK {
		t.Fatalf("owner chunk 0 失败: %d", c)
	}
	if c := env.chunkAs(t, "ak-A", uploadID, 1, chunk1); c != http.StatusOK {
		t.Fatalf("owner chunk 1 失败: %d", c)
	}

	// complete 必须成功
	cc, cresp := env.completeAs(t, "ak-A", uploadID)
	if cc != http.StatusOK {
		t.Fatalf("owner complete 失败: %d %v", cc, cresp)
	}
	finalChecksum, _ := cresp["file_checksum"].(string)
	if finalChecksum != fileChecksum {
		t.Fatalf("complete checksum 不匹配: got %s want %s", finalChecksum, fileChecksum)
	}

	// 文件落盘到 owner 子目录
	fullPath := filepath.Join(env.dir, "ak-A", "dir", "owner-data.bin")
	dataOnDisk, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("owner 文件未按 owner 目录落盘: %v", err)
	}
	if string(dataOnDisk) != string(totalData) {
		t.Fatalf("落盘内容不一致: len=%d want %d", len(dataOnDisk), len(totalData))
	}
	// checksum store 用 owner 作用域 key
	csKey := checksumStoreKey("ak-A", "dir/owner-data.bin")
	if got, ok := env.h.checksumStore.Get(csKey); !ok || got != fileChecksum {
		t.Fatalf("owner checksum 记录缺失: key=%q ok=%v got=%q", csKey, ok, got)
	}
}

// TestChunkedUploadOwner_BareUploadIDRejected 验证伪造/陈旧 bare upload_id 在 owner 认证下
// 必须被拒绝——服务端只认带 owner 前缀的会话 id（防跨租户接管）。
func TestChunkedUploadOwner_BareUploadIDRejected(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	filename := "dir/bare-reject.bin"
	bareID := "bbbb1111aaaa2222cccc3333dddd4444"
	fileChecksum := sha256Hex([]byte("bare-content"))

	code, resp := env.initAs(t, "ak-A", bareID, filename, 12, 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	uploadID, _ := resp["upload_id"].(string)
	if uploadID != "ak-A/"+bareID {
		t.Fatalf("init 返回 id %q, want %q", uploadID, "ak-A/"+bareID)
	}

	// 用 bare id（无 owner 前缀）上传 → 必须 404
	if c := env.chunkAs(t, "ak-A", bareID, 0, []byte("bare-content")); c != http.StatusNotFound {
		t.Fatalf("bare id chunk 应 404（防伪造前缀接管）: %d", c)
	}
	// 未认证（空 owner）用同样 bare id → 也成功建独立会话？不，空 owner 会话 key 就是 bare id
	// ——这里验证的是"服务端只认带前缀 id"→ bare id 在 owner 视角不可用，已覆盖。

	// 伪造他人 owner 前缀（ak-B/）→ 必须 404（IDOR）
	if c := env.chunkAs(t, "ak-A", "ak-B/"+bareID, 0, []byte("bare-content")); c == http.StatusOK {
		t.Fatalf("伪造他人 owner 前缀的 chunk 必须 404（IDOR）")
	}
}

// TestChunkedUploadOwner_GetSessionByFilenameOwner 验证（审查 #6）按文件名查未完成会话时
// owner 精确匹配：未认证只命中全局（裸 id）会话；owner 前缀包含关系的两个租户（ak/akx）
// 不会互相看到对方会话。
func TestChunkedUploadOwner_GetSessionByFilenameOwner(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = dir
	cfgPtr := newTestCfgPtr(dir)
	cfgPtr.Store(cfg)
	store := MustNewUploadStore(dir, 24*3600*1e9, testLogger())
	t.Cleanup(store.Stop)

	fs := []string{
		"ak-A/d1", "ak-A/d2", // 租户 A 的两个会话
		"ak/d3", // 租户 "ak"（ak 是 ak-A 的前缀关系）
		"d4",    // 未认证（裸 id）会话
	}
	for _, id := range fs {
		if _, err := store.CreateSession(id, "same.bin", 1, 4096, 1, "00", 0); err != nil {
			t.Fatalf("创建会话 %s 失败: %v", id, err)
		}
	}

	// 未认证：按文件名查 → 只命中裸 id 会话（d4）
	if s := store.GetSessionByFilenameOwner("same.bin", ""); s == nil || s.UploadID != "d4" {
		t.Fatalf("未认证按文件名查应命中裸 id 会话 d4, got %+v", s)
	}

	// ak-A 只命中 ak-A/ 会话（首个）
	if s := store.GetSessionByFilenameOwner("same.bin", "ak-A"); s == nil || !strings.HasPrefix(s.UploadID, "ak-A/") {
		t.Fatalf("ak-A 应命中 ak-A/ 会话, got %+v", s)
	}
	// ak（前缀关系命名）不吞并 ak-A 的会话
	if s := store.GetSessionByFilenameOwner("same.bin", "ak"); s == nil || s.UploadID != "ak/d3" {
		t.Fatalf("ak 只应命中 ak/ 会话（前缀包含不误配 ak-A/）, got %+v", s)
	}
}
func TestChunkedUploadOwner_SameBareIDDifferentOwnerIsolated(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	filename := "shared.bin"
	sharedID := "cccc1111bbbb2222aaaa3333dddd4444"
	sizeA := 12
	sizeB := 9
	csA := sha256Hex([]byte("content-A!!!"))
	csB := sha256Hex([]byte("content-B!"))

	_, respA := env.initAs(t, "ak-A", sharedID, filename, int64(sizeA), 4096, 1, csA)
	uploadIDA, _ := respA["upload_id"].(string)

	// 另一个 actor（这里仅 ak-A 与空，空 owner 会话 key 即 bare id）
	codeEmpty, respEmpty := env.initAs(t, "", sharedID, filename, int64(sizeB), 4096, 1, csB)
	if codeEmpty != http.StatusOK {
		t.Fatalf("空 owner init 失败: %d %v", codeEmpty, respEmpty)
	}
	uploadIDEmpty, _ := respEmpty["upload_id"].(string)
	if uploadIDEmpty != sharedID {
		t.Fatalf("空 owner 会话 key 应为 bare id, got %q", uploadIDEmpty)
	}
	if uploadIDA == uploadIDEmpty {
		t.Fatalf("owner 与空 owner 同 bare id 会话应隔离: %q == %q", uploadIDA, uploadIDEmpty)
	}

	// 各自用各自 id 上传互不影响
	if c := env.chunkAs(t, "ak-A", uploadIDA, 0, []byte("content-A!!!")); c != http.StatusOK {
		t.Fatalf("ak-A chunk 失败: %d", c)
	}
	if c := env.chunkAs(t, "", uploadIDEmpty, 0, []byte("content-B!")); c != http.StatusOK {
		t.Fatalf("empty chunk 失败: %d", c)
	}
	if cc, cresp := env.completeAs(t, "ak-A", uploadIDA); cc != http.StatusOK {
		t.Fatalf("ak-A complete 失败: %d %v", cc, cresp)
	}
	if cc, _ := env.completeAs(t, "", uploadIDEmpty); cc != http.StatusOK {
		t.Fatalf("empty complete 失败: %d", cc)
	}
}

// TestChunkedUploadOwner_RestartRecoversOwnerSession 验证 owner 会话在服务重启后恢复：
// session.json 用完整 id（<owner>/<bare>）作 map key，恢复后 GetSession 能命中，
// status/complete 不再 404（修复审查 #2：recoverOwnerSessions 曾用 bare 目录名作 key）。
func TestChunkedUploadOwner_RestartRecoversOwnerSession(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.UploadsDir = dir
	cfgPtr := newTestCfgPtr(dir)
	cfgPtr.Store(cfg)

	// 第一代 store：创建 owner 会话并上传一个分块
	h1 := &Handlers{
		cfgPtr:        cfgPtr,
		checksumStore: NewChecksumStore(dir, testLogger()),
		uploadStore:   MustNewUploadStore(dir, 24*3600*1e9, testLogger()),
		logger:        testLogger(),
		auditLogger:   testLogger(),
	}
	uploadID := "ak-A/dddd1111bbbb2222cccc3333dddd4444"
	filename := "dir/restart.bin"
	content := []byte("restart-content")
	if _, err := h1.uploadStore.CreateSession(uploadID, filename, int64(len(content)), 4096, 1,
		sha256Hex(content), 0); err != nil {
		t.Fatalf("创建 owner 会话失败: %v", err)
	}
	if err := h1.uploadStore.MarkChunkReceived(uploadID, 0, sha256Hex(content)); err != nil {
		t.Fatalf("标记分块失败: %v", err)
	}
	h1.uploadStore.Stop() // 模拟重启（落盘）

	// 第二代 store：从磁盘恢复，key 必须命中完整 id
	h2 := &Handlers{
		cfgPtr:        cfgPtr,
		checksumStore: NewChecksumStore(dir, testLogger()),
		uploadStore:   MustNewUploadStore(dir, 24*3600*1e9, testLogger()),
		logger:        testLogger(),
		auditLogger:   testLogger(),
	}
	t.Cleanup(h2.uploadStore.Stop)

	recovered := h2.uploadStore.GetSession(uploadID)
	if recovered == nil {
		t.Fatalf("重启后 GetSession(%q) 未命中——恢复 key 错位（bare 目录名当完整 id 用）", uploadID)
	}
	if recovered.Filename != filename {
		t.Fatalf("恢复会话 Filename = %q, want %q", recovered.Filename, filename)
	}
	if !recovered.ReceivedChunks[0] {
		t.Fatalf("恢复会话应保留已接收分块标记（bitmap 对齐）")
	}

	// complete 走 handler 也须可命中（validateCompleteSession → GetSession）
	raw, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	req := httptest.NewRequest("POST", "/upload/complete", bytes.NewReader(raw))
	req = req.WithContext(withActor(req.Context(), "ak-A"))
	rr := httptest.NewRecorder()
	h2.uploadComplete(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("重启后带 owner complete 失败: %d %s", rr.Code, rr.Body.String())
	}

	// 空 owner 会话恢复不受影响：创建 bare-id 会话并落盘，第二代恢复后 bare key 命中
	emptyID := "eeee1111bbbb2222cccc3333dddd4444"
	emptyContent := []byte("x")
	if _, err := h1.uploadStore.CreateSession(emptyID, "empty.bin", int64(len(emptyContent)), 4096, 1,
		sha256Hex(emptyContent), 0); err != nil {
		t.Fatalf("创建空 owner 会话失败: %v", err)
	}
	if err := h1.uploadStore.MarkChunkReceived(emptyID, 0, sha256Hex(emptyContent)); err != nil {
		t.Fatalf("标记空 owner 分块失败: %v", err)
	}
	h1.uploadStore.Stop()
	h3 := &Handlers{
		cfgPtr:        cfgPtr,
		checksumStore: NewChecksumStore(dir, testLogger()),
		uploadStore:   MustNewUploadStore(dir, 24*3600*1e9, testLogger()),
		logger:        testLogger(),
		auditLogger:   testLogger(),
	}
	t.Cleanup(h3.uploadStore.Stop)
	if got := h3.uploadStore.GetSession(emptyID); got == nil {
		t.Fatalf("空 owner 会话重启后也应恢复，GetSession(%q) 未命中", emptyID)
	}
}
