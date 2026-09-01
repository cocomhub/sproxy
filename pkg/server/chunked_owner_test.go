// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// chunked_owner_test.go 验证分块上传迁移到 per-tenant chunk 桶后的多租户隔离：
// upload_id 用裸 id（无 owner 前缀），会话目录落 <root>/<owner>/chunk/<id>/，
// 租户隔离由 per-tenant UploadStore 实例物理保证。

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

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
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

// newChunkedTestHandlers 构建装配好多租户存储布局（globalRoot + 懒创建缓存）的 Handlers。
// dir 为存储根；uploadStores 懒创建（首次 uploadStoreFor 时建）。
func newChunkedTestHandlers(t *testing.T, dir string, chunkSize int64) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.UploadsDir = dir
	if chunkSize > 0 {
		cfg.ChunkSize = chunkSize
	}
	cfgPtr := newTestCfgPtr(dir)
	cfgPtr.Store(cfg)

	h := &Handlers{
		cfgPtr:        cfgPtr,
		logger:        testLogger(),
		auditLogger:   testLogger(),
		uploadingStop: make(chan struct{}),
	}
	globalRoot, err := storage.OpenRoot(cfg.StorageRoot())
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	h.globalRoot = globalRoot
	h.globalPool = quota.NewPool(cfg.MaxStorageBytes)
	h.tenantRoots = make(map[string]*storage.Tenant)
	h.checksumStores = make(map[string]*ChecksumStore)
	h.uploadStores = make(map[string]*UploadStore)
	h.quotaScopes = make(map[string]*quota.Scope)
	if h.tenantFor(anonymousOwner) == nil {
		t.Fatal("创建 anonymous 租户失败")
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// ownerChunkedEnv 提供共享 Handlers 的多 actor 分块上传测试环境。
type ownerChunkedEnv struct {
	h   *Handlers
	dir string
	mux map[string]*http.ServeMux // actor → mux
}

// newOwnerChunkedEnv 创建多 actor 分块上传环境（租户布局装配）。
func newOwnerChunkedEnv(t *testing.T) *ownerChunkedEnv {
	t.Helper()
	dir := t.TempDir()
	h := newChunkedTestHandlers(t, dir, 4<<10) // 4 KiB 小分块便于单文件多 chunk 测试
	env := &ownerChunkedEnv{h: h, dir: dir, mux: map[string]*http.ServeMux{}}
	for _, actor := range []string{"alice", "bob", "ak-A", ""} {
		env.mux[actor] = actorChunkedMux(h, actor)
	}
	return env
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

// statusAs 以指定 actor 查询 upload status，返回状态码与响应体。
func (e *ownerChunkedEnv) statusAs(t *testing.T, actor, query string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("GET", "/upload/status?"+query, nil)
	rr := httptest.NewRecorder()
	e.mux[actor].ServeHTTP(rr, req)
	var resp map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	return rr.Code, resp
}

// TestChunked_NewLayoutNoOwnerPrefix 验证新布局（任务 12）：
// upload_id 返回裸 id（无 alice/ 前缀）、会话目录落 <root>/alice/chunk/<id>/、
// chunk/complete/status 用裸 id 成功、跨租户同裸 id 会话互不可见（404）。
func TestChunked_NewLayoutNoOwnerPrefix(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	filename := "dir/f.bin"
	totalSize := 6000
	chunkSize := int64(4096)
	totalChunks := 2
	totalData := make([]byte, 0, totalSize)
	for i := range totalSize {
		totalData = append(totalData, byte(i%251))
	}
	fileChecksum := sha256Hex(totalData)

	bareID := "bareid123"
	code, resp := env.initAs(t, "alice", bareID, filename, int64(totalSize), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("alice init 失败: %d %v", code, resp)
	}
	uploadID, _ := resp["upload_id"].(string)
	if uploadID != bareID {
		t.Fatalf("init 应返回裸 id %q（无 owner 前缀）, got %q", bareID, uploadID)
	}

	// 会话目录在 <root>/alice/chunk/bareid123/（无 .__chunked__，无 owner 双层）
	sessionDir := filepath.Join(env.dir, "alice", "chunk", bareID)
	if _, err := os.Stat(filepath.Join(sessionDir, "session.json")); err != nil {
		t.Fatalf("会话目录未创建在 %s: %v", sessionDir, err)
	}

	// chunk/complete 用裸 id 成功
	chunk0 := totalData[:4096]
	chunk1 := totalData[4096:]
	if c := env.chunkAs(t, "alice", bareID, 0, chunk0); c != http.StatusOK {
		t.Fatalf("alice chunk 0 失败: %d", c)
	}
	if c := env.chunkAs(t, "alice", bareID, 1, chunk1); c != http.StatusOK {
		t.Fatalf("alice chunk 1 失败: %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", bareID); cc != http.StatusOK {
		t.Fatalf("alice complete 失败: %d %v", cc, cresp)
	}

	// status 用裸 id 成功（已完成会话可查）
	if sc, sresp := env.statusAs(t, "alice", "upload_id="+bareID); sc != http.StatusOK {
		t.Fatalf("alice status 失败: %d %v", sc, sresp)
	}

	// 最终文件落 alice/user/dir/f.bin（下载读到新布局）
	finalPath := filepath.Join(env.dir, "alice", "user", "dir", "f.bin")
	dataOnDisk, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("最终文件未按 user 桶落盘: %v", err)
	}
	if string(dataOnDisk) != string(totalData) {
		t.Fatalf("落盘内容不一致: len=%d want %d", len(dataOnDisk), len(totalData))
	}

	// 跨租户：bob 用 alice 的裸 id 会话 → 404（租户根隔离）
	if c := env.chunkAs(t, "bob", bareID, 0, chunk0); c != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id 上传应 404（per-tenant 隔离）: %d", c)
	}
	if cc, _ := env.completeAs(t, "bob", bareID); cc != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id complete 应 404: %d", cc)
	}
	if sc, _ := env.statusAs(t, "bob", "upload_id="+bareID); sc != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id status 应 404: %d", sc)
	}
}

// TestChunkedUploadOwner_BareIDWorkflow 验证带 owner 认证的分块上传全流程（新布局）：
// init 返回裸 id，chunk/complete/status 沿用裸 id 成功；最终文件落 owner user 桶。
func TestChunkedUploadOwner_BareIDWorkflow(t *testing.T) {
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
	if uploadID != bareID {
		t.Fatalf("init 应返回裸 id（不再带 owner 前缀）, got %q", uploadID)
	}

	// 用裸 id 上传分块
	chunk0 := totalData[:4096]
	chunk1 := totalData[4096:]
	if c := env.chunkAs(t, "ak-A", bareID, 0, chunk0); c != http.StatusOK {
		t.Fatalf("owner chunk 0 失败: %d", c)
	}
	if c := env.chunkAs(t, "ak-A", bareID, 1, chunk1); c != http.StatusOK {
		t.Fatalf("owner chunk 1 失败: %d", c)
	}

	// complete 必须成功
	cc, cresp := env.completeAs(t, "ak-A", bareID)
	if cc != http.StatusOK {
		t.Fatalf("owner complete 失败: %d %v", cc, cresp)
	}
	finalChecksum, _ := cresp["file_checksum"].(string)
	if finalChecksum != fileChecksum {
		t.Fatalf("complete checksum 不匹配: got %s want %s", finalChecksum, fileChecksum)
	}

	// 文件落盘到 owner 租户 user 桶
	fullPath := filepath.Join(env.dir, "ak-A", "user", "dir", "owner-data.bin")
	dataOnDisk, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("owner 文件未按 user 桶落盘: %v", err)
	}
	if string(dataOnDisk) != string(totalData) {
		t.Fatalf("落盘内容不一致: len=%d want %d", len(dataOnDisk), len(totalData))
	}
	// checksum store 用 per-tenant store，key = 租户根内相对路径
	if cs := env.h.checksumStoreFor("ak-A"); cs == nil {
		t.Fatal("per-tenant checksum store 不可用")
	} else if got, ok := cs.Get("user/dir/owner-data.bin"); !ok || got != fileChecksum {
		t.Fatalf("owner checksum 记录缺失: ok=%v got=%q", ok, got)
	}
}

// TestChunkedUploadOwner_CrossTenantBareIDSessionNotFound 验证跨租户裸 id 会话不可见：
// alice 创建会话后，bob 用同一裸 id chunk/complete/status → 404（租户根隔离，
// 取代旧"伪造 owner 前缀"防御——新机制下无需前缀）。
func TestChunkedUploadOwner_CrossTenantBareIDSessionNotFound(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	filename := "dir/bare-reject.bin"
	bareID := "bbbb1111aaaa2222cccc3333dddd4444"
	fileChecksum := sha256Hex([]byte("bare-content"))

	code, resp := env.initAs(t, "alice", bareID, filename, 12, 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 失败: %d %v", code, resp)
	}
	uploadID, _ := resp["upload_id"].(string)
	if uploadID != bareID {
		t.Fatalf("init 返回 id %q, want 裸 id %q", uploadID, bareID)
	}

	// bob 用同一裸 id 操作 → 必须 404（per-tenant 隔离，跨租户同裸 id 互不可见）
	if c := env.chunkAs(t, "bob", bareID, 0, []byte("bare-content")); c != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id chunk 应 404: %d", c)
	}
	if cc, _ := env.completeAs(t, "bob", bareID); cc != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id complete 应 404: %d", cc)
	}
	if sc, _ := env.statusAs(t, "bob", "upload_id="+bareID); sc != http.StatusNotFound {
		t.Fatalf("bob 用 alice 的裸 id status 应 404: %d", sc)
	}
}

// TestChunkedUploadOwner_GetSessionByFilenamePerTenant 验证按文件名查未完成会话时
// per-tenant store 实例天然隔离：同一文件名在不同租户 store 中各命中本租户会话，
// 互不串扰（取代旧 GetSessionByFilenameOwner 的 owner 精确匹配）。
func TestChunkedUploadOwner_GetSessionByFilenamePerTenant(t *testing.T) {
	env := newOwnerChunkedEnv(t)

	// 两个租户各自创建同名会话（裸 id）
	if code, resp := env.initAs(t, "alice", "alice-s1", "same.bin", 1, 4096, 1, strings.Repeat("0", 64)); code != http.StatusOK {
		t.Fatalf("alice init 失败: %d %v", code, resp)
	}
	if code, resp := env.initAs(t, "bob", "bob-s1", "same.bin", 1, 4096, 1, strings.Repeat("0", 64)); code != http.StatusOK {
		t.Fatalf("bob init 失败: %d %v", code, resp)
	}

	// alice 的 store 只命中 alice 的会话；bob 的 store 只命中 bob 的会话
	aliceStore := env.h.uploadStoreFor("alice")
	if s := aliceStore.GetSessionByFilename("same.bin"); s == nil || s.UploadID != "alice-s1" {
		t.Fatalf("alice store 按文件名应命中 alice-s1, got %+v", s)
	}
	bobStore := env.h.uploadStoreFor("bob")
	if s := bobStore.GetSessionByFilename("same.bin"); s == nil || s.UploadID != "bob-s1" {
		t.Fatalf("bob store 按文件名应命中 bob-s1, got %+v", s)
	}
	// 互不串扰：alice store 中不存在 bob 的会话
	if s := aliceStore.GetSession("bob-s1"); s != nil {
		t.Fatalf("alice store 不应包含 bob 的会话, got %+v", s)
	}
}

// TestChunkedUploadOwner_SameBareIDDifferentOwnerIsolated 验证同裸 id 在不同租户下隔离：
// alice 与 anonymous 各用同一裸 id 建独立会话，互不影响。
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
	if uploadIDA != sharedID {
		t.Fatalf("ak-A init 应返回裸 id, got %q", uploadIDA)
	}

	codeEmpty, respEmpty := env.initAs(t, "", sharedID, filename, int64(sizeB), 4096, 1, csB)
	if codeEmpty != http.StatusOK {
		t.Fatalf("空 owner init 失败: %d %v", codeEmpty, respEmpty)
	}
	uploadIDEmpty, _ := respEmpty["upload_id"].(string)
	if uploadIDEmpty != sharedID {
		t.Fatalf("空 owner 会话也应返回裸 id, got %q", uploadIDEmpty)
	}

	// 各自用各自 id（同为裸 id，但分属不同租户 store）上传互不影响
	if c := env.chunkAs(t, "ak-A", sharedID, 0, []byte("content-A!!!")); c != http.StatusOK {
		t.Fatalf("ak-A chunk 失败: %d", c)
	}
	if c := env.chunkAs(t, "", sharedID, 0, []byte("content-B!")); c != http.StatusOK {
		t.Fatalf("empty chunk 失败: %d", c)
	}
	if cc, cresp := env.completeAs(t, "ak-A", sharedID); cc != http.StatusOK {
		t.Fatalf("ak-A complete 失败: %d %v", cc, cresp)
	}
	if cc, _ := env.completeAs(t, "", sharedID); cc != http.StatusOK {
		t.Fatalf("empty complete 失败: %d", cc)
	}

	// 各自最终文件落各自租户 user 桶（隔离）
	if _, err := os.Stat(filepath.Join(env.dir, "ak-A", "user", "shared.bin")); err != nil {
		t.Fatalf("ak-A 文件未落盘: %v", err)
	}
	if _, err := os.Stat(filepath.Join(env.dir, "anonymous", "user", "shared.bin")); err != nil {
		t.Fatalf("anonymous 文件未落盘: %v", err)
	}
}

// TestChunkedUploadOwner_RestartRecoversPerTenantSession 验证租户会话在服务重启后恢复：
// session.json 位于 <root>/<owner>/chunk/<bare>/，恢复后 GetSession 命中、complete 不 404。
func TestChunkedUploadOwner_RestartRecoversPerTenantSession(t *testing.T) {
	dir := t.TempDir()

	// 第一代 handlers：创建 alice 会话并上传一个分块
	h1 := newChunkedTestHandlers(t, dir, 0)
	bareID := "dddd1111bbbb2222cccc3333dddd4444"
	filename := "dir/restart.bin"
	content := []byte("restart-content")
	if _, err := h1.uploadStoreFor("alice").CreateSession(bareID, filename, int64(len(content)), 4096, 1,
		sha256Hex(content), 0); err != nil {
		t.Fatalf("创建 alice 会话失败: %v", err)
	}
	if err := h1.uploadStoreFor("alice").MarkChunkReceived(bareID, 0, sha256Hex(content)); err != nil {
		t.Fatalf("标记分块失败: %v", err)
	}
	h1.Close() // 模拟重启（落盘 + 停止 store）

	// 第二代 handlers：从磁盘恢复，key 必须命中裸 id
	h2 := newChunkedTestHandlers(t, dir, 0)
	recovered := h2.uploadStoreFor("alice").GetSession(bareID)
	if recovered == nil {
		t.Fatalf("重启后 GetSession(%q) 未命中——恢复 key 错位", bareID)
	}
	if recovered.Filename != filename {
		t.Fatalf("恢复会话 Filename = %q, want %q", recovered.Filename, filename)
	}
	if !recovered.ReceivedChunks[0] {
		t.Fatalf("恢复会话应保留已接收分块标记（bitmap 对齐）")
	}

	// complete 走 handler 也须可命中（validateCompleteSession → GetSession）
	raw, _ := json.Marshal(map[string]string{"upload_id": bareID})
	req := httptest.NewRequest("POST", "/upload/complete", bytes.NewReader(raw))
	req = req.WithContext(withActor(req.Context(), "alice"))
	rr := httptest.NewRecorder()
	h2.uploadComplete(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("重启后带 owner complete 失败: %d %s", rr.Code, rr.Body.String())
	}
}
