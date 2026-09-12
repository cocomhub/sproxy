// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// handlers_test.go 是分块子包的**域级处理器测试**：以真实下层能力（pkg/storage 租户根、
// pkg/checksum 台账）+ 本文件内的最小桩装配 Deps，直接驱动本包的 HTTP 处理器（不经 pkg/server）。
//
// 存在理由：`pkg/server` 的集成测试虽经完整装配覆盖同样路径，但 Go 的覆盖率归属按包切分
// （无 -coverpkg），迁出的处理器若只由 pkg/server 测试驱动，本包覆盖率会虚低。故此处覆盖
// 「会话生命周期 + 分块直写 + 合并且校验」主路径与关键拒绝路径（与 pkg/server 侧用例**互补**，
// 不追求逐字重复）。
//
// 桩的诚实声明（等价范围）：
//   - RouteUpload 恒返回默认租户且**不预留**（无 scope/pool 预留）——覆盖单卷零回归路径；
//     多卷路由/双账本预留由 pkg/server 集成测试覆盖（本文件不复制）。
//   - ResolveDownloadPath 直接指向构造好的租户与相对路径（不覆盖 kind=cloud_task 的归属校验，
//     属读面/云域，由 pkg/server 覆盖）。
//   - VersioningEnabled 恒 false ⇒ 覆盖写不触发版本保存（本包 SaveVersion 及其清理路径不达）；
//     AcquireFileLock 恒成功（不测 409 互斥）。
//   - LocateOwnerFile 恒未命中（单卷语义）。

type chunkedTestEnv struct {
	dir  string
	root *storage.Root
	tnt  *storage.Tenant
	cs   *checksum.ChecksumStore
	us   *UploadStore
}

func newChunkedTestEnv(t *testing.T) *chunkedTestEnv {
	t.Helper()
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	t.Cleanup(func() { _ = root.Close() })
	tnt, err := storage.NewTenant("alice", root)
	if err != nil {
		t.Fatalf("建租户失败: %v", err)
	}
	if mkErr := tnt.Root().MkdirAll("meta", 0o755); mkErr != nil {
		t.Fatalf("建 meta 桶失败: %v", mkErr)
	}
	chunkAbs, ok := tnt.Root().Abs("chunk")
	if !ok {
		t.Fatal("派生 chunk 桶失败")
	}
	us, err := NewUploadStore(chunkAbs, time.Hour, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("建 UploadStore 失败: %v", err)
	}
	t.Cleanup(us.Stop)
	metaAbs, _ := tnt.Root().Abs("meta")
	return &chunkedTestEnv{
		dir: dir, root: root, tnt: tnt,
		cs: checksum.NewChecksumStore(filepath.Join(metaAbs, "checksums.json"), nil),
		us: us,
	}
}

// handlers 装配本包处理器（真实下层能力 + 桩），chunkSize 为配置分块大小。
func (e *chunkedTestEnv) handlers(chunkSize int64) *Service {
	return NewService(Deps{
		Logger:           func() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) },
		ActorFromRequest: func(*http.Request) string { return "alice" },
		ChunkSize:        func() int64 { return chunkSize },
		VersioningEnabled: func() bool {
			return false
		},
		VersioningMaxVersions: func() int { return 0 },
		UploadStoreFor:        func(string) *UploadStore { return e.us },
		TenantFor:             func(string) *storage.Tenant { return e.tnt },
		VolumeTenant:          func(string, string) *storage.Tenant { return e.tnt },
		QuotaScopeFor:         func(string, string) *quota.Scope { return nil },
		ChecksumStoreFor:      func(string) *checksum.ChecksumStore { return e.cs },
		Uploading:             &sync.Map{},
		ResolveDownloadPath: func(r *http.Request) (DownloadPath, error) {
			name := r.URL.Query().Get("filename")
			rel, ok := e.tnt.UserRel(name)
			if !ok {
				return DownloadPath{}, &HTTPError{Status: http.StatusBadRequest, Message: "无效的文件名"}
			}
			return DownloadPath{Filename: name, Tenant: e.tnt, Rel: rel}, nil
		},
		LocateOwnerFile: func(string, string) (FileLocation, bool) { return FileLocation{}, false },
		RouteUpload: func(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
			if explicitVol != "" && explicitVol != "default" {
				return UploadRoute{}, &HTTPError{Status: http.StatusConflict, Message: "卷不存在"}
			}
			return UploadRoute{VolumeName: "", Tenant: e.tnt, Release: func() {}}, nil
		},
		AcquireFileLock:      func(string, string) (func(), bool) { return func() {}, true },
		RecordOverwriteAudit: func(ctx context.Context, filename string) {},
		RecordFileAudit:      func(context.Context, string, string, string, string) {},
	})
}

func (e *chunkedTestEnv) doJSON(t *testing.T, h *Service, method, target string, fn http.HandlerFunc, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal 请求体: %v", err)
		}
		rd = bytes.NewReader(buf)
	}
	req := httptest.NewRequest(method, target, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	fn(rec, req)
	return rec
}

// TestService_ChunkedUploadLifecycle_FullFlow 覆盖 init → chunk → status → complete 主链路：
// 分块直写整临时文件、逐块校验、complete 全文件校验后 rename 为正式名并写 checksum 台账。
// 断言落盘副作用（磁盘文件内容与 checksum），不只看状态码。
func TestService_ChunkedUploadLifecycle_FullFlow(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	content := []byte("0123456789")
	fileCS := sha256Hex(content)
	uploadID := "lifecycle-1"
	filename := "dir/flow.bin"

	// init
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": fileCS, "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var initResp ChunkedInitResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &initResp); err != nil {
		t.Fatalf("解析 init 响应: %v", err)
	}
	if !initResp.Success || initResp.UploadID != uploadID || initResp.ChunkSize != 4 {
		t.Fatalf("init 响应异常: %+v", initResp)
	}
	// 在途临时文件已按 TotalSize 预占
	sess := env.us.GetSession(uploadID)
	if sess == nil || sess.TempPath == "" {
		t.Fatalf("会话未建立或 TempPath 为空: %+v", sess)
	}
	tempAbs, ok := env.tnt.Root().Abs(sess.TempPath)
	if !ok {
		t.Fatal("TempPath 越界")
	}
	if fi, err := os.Stat(tempAbs); err != nil || fi.Size() != int64(len(content)) {
		t.Fatalf("在途临时文件应为 %d 字节: err=%v", len(content), err)
	}

	// chunk 0/1/2（末片短于 chunk_size）
	for i := range 3 {
		start := i * 4
		end := min(start+4, len(content))
		data := content[start:end]
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		if err := w.WriteField("upload_id", uploadID); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteField("chunk_index", string(rune('0'+i))); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteField("chunk_checksum", sha256Hex(data)); err != nil {
			t.Fatal(err)
		}
		fw, err := w.CreateFormFile("chunk", "chunk")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/upload/chunk", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		chunkRec := httptest.NewRecorder()
		h.UploadChunk(chunkRec, req)
		if chunkRec.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, chunkRec.Code, chunkRec.Body.String())
		}
		var cr ChunkUploadResponse
		if err := json.Unmarshal(chunkRec.Body.Bytes(), &cr); err != nil {
			t.Fatalf("解析 chunk 响应: %v", err)
		}
		if !cr.Success || cr.ChunkIndex != i {
			t.Fatalf("chunk %d 响应异常: %+v", i, cr)
		}
	}

	// status：全部分片已接收
	rec = env.doJSON(t, h, http.MethodGet, "/upload/status?upload_id="+uploadID, h.UploadStatus, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var st ChunkStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析 status: %v", err)
	}
	if !st.Success || st.ReceivedCount != 3 || len(st.MissingChunks) != 0 {
		t.Fatalf("status 异常: %+v", st)
	}

	// sessions：未完成会话在列表中
	rec = env.doJSON(t, h, http.MethodGet, "/upload/sessions", h.UploadSessions, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sessions 状态=%d", rec.Code)
	}
	var list ChunkSessionsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("解析 sessions: %v", err)
	}
	if len(list.Sessions) != 1 || list.Sessions[0].UploadID != uploadID || list.Sessions[0].Status != "uploading" {
		t.Fatalf("sessions 异常: %+v", list.Sessions)
	}

	// complete：rename 落盘 + checksum 台账
	rec = env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": uploadID})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var cr ChunkCompleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("解析 complete: %v", err)
	}
	if !cr.Success || cr.FileChecksum != fileCS {
		t.Fatalf("complete 响应异常: %+v", cr)
	}
	rel, _ := env.tnt.UserRel(filename)
	abs, ok := env.tnt.Root().Abs(rel)
	if !ok {
		t.Fatal("落盘路径派生失败")
	}
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("落盘内容不一致: got=%q want=%q", got, content)
	}
	if csVal, ok := env.cs.Get(rel); !ok || csVal != fileCS {
		t.Fatalf("checksum 台账未记录: ok=%v val=%q", ok, csVal)
	}
	// 在途临时文件已被 rename 移走
	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("在途临时文件应已随 rename 消失: %v", err)
	}
}

// TestService_ChunkedInit_RejectsBadInput 覆盖 init 的字段校验拒绝路径。
func TestService_ChunkedInit_RejectsBadInput(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)
	base := func() map[string]any {
		return map[string]any{
			"upload_id": "b", "filename": "a.bin", "total_size": 8,
			"chunk_size": 4, "total_chunks": 2,
			"file_checksum": sha256Hex([]byte("01234567")),
		}
	}
	cases := []struct {
		name string
		mut  func(m map[string]any)
	}{
		{"缺少 upload_id", func(m map[string]any) { m["upload_id"] = "" }},
		{"upload_id 非法", func(m map[string]any) { m["upload_id"] = "../x" }},
		{"文件名穿越", func(m map[string]any) { m["filename"] = "../x.bin" }},
		{"total_size 非正", func(m map[string]any) { m["total_size"] = 0 }},
		{"chunk_size 非正", func(m map[string]any) { m["chunk_size"] = 0 }},
		{"chunk_size*total_chunks 不足", func(m map[string]any) { m["total_chunks"] = 1 }},
		{"file_checksum 非 hex", func(m map[string]any) { m["file_checksum"] = "zz" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := base()
			tc.mut(m)
			rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, m)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("应 400，得到 %d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestService_ChunkedStatus_NotFoundOrByUploadID 覆盖 status 的 404 与命中路径。
func TestService_ChunkedStatus_NotFoundOrByUploadID(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodGet, "/upload/status?upload_id=nope", h.UploadStatus, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知 upload_id 应 404，得到 %d", rec.Code)
	}

	sess, err := env.us.CreateSession("sid-1", "s.bin", 8, 4, 2, strings.Repeat("a", 64), 0)
	if err != nil {
		t.Fatalf("建会话: %v", err)
	}
	rec = env.doJSON(t, h, http.MethodGet, "/upload/status?upload_id="+sess.UploadID, h.UploadStatus, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("命中会话应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	var st ChunkStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if st.UploadID != "sid-1" || st.TotalChunks != 2 || len(st.MissingChunks) != 2 {
		t.Fatalf("status 异常: %+v", st)
	}
}

// TestService_DownloadChunk_RangeAndErrors 覆盖分块下载的 200 与 400/404/416 路径。
func TestService_DownloadChunk_RangeAndErrors(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	content := []byte("abcdefghij")
	rel, _ := env.tnt.UserRel("dl.bin")
	if err := env.tnt.Root().MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, ok := env.tnt.Root().Abs(rel)
	if !ok {
		t.Fatal("目标路径派生失败")
	}
	if err := os.WriteFile(abs, content, 0o600); err != nil {
		t.Fatalf("写目标文件: %v", err)
	}
	env.cs.Set(rel, sha256Hex(content))

	rec := env.doJSON(t, h, http.MethodGet, "/download/chunk?filename=dl.bin&offset=2&length=4", h.DownloadChunk, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("分块下载应 200，得到 %d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != "cdef" {
		t.Fatalf("分块内容=%q want cdef", got)
	}
	if cr := rec.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Fatalf("Content-Range=%q", cr)
	}
	if cs := rec.Header().Get("X-Chunk-Checksum"); cs != sha256Hex([]byte("cdef")) {
		t.Fatalf("X-Chunk-Checksum=%q", cs)
	}
	if cs := rec.Header().Get(headerFileChecksum); cs != sha256Hex(content) {
		t.Fatalf("%s=%q", headerFileChecksum, cs)
	}

	// 非法 offset / length → 400
	for _, q := range []string{"offset=-1", "length=0"} {
		badRec := env.doJSON(t, h, http.MethodGet, "/download/chunk?filename=dl.bin&"+q, h.DownloadChunk, nil)
		if badRec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，得到 %d", q, badRec.Code)
		}
	}
	// 路径穿越 → 400（由下载路径解析桩拒绝）
	rec = env.doJSON(t, h, http.MethodGet, "/download/chunk?filename=../x", h.DownloadChunk, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("穿越应 400，得到 %d", rec.Code)
	}
	// 文件不存在 → 404
	rec = env.doJSON(t, h, http.MethodGet, "/download/chunk?filename=missing.bin", h.DownloadChunk, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("缺文件应 404，得到 %d", rec.Code)
	}
	// offset 超出文件大小 → 416
	rec = env.doJSON(t, h, http.MethodGet, "/download/chunk?filename=dl.bin&offset=99", h.DownloadChunk, nil)
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("越界 offset 应 416，得到 %d", rec.Code)
	}
}

// TestService_UploadChunk_RejectsBadRequests 覆盖分块上传的拒绝路径。
func TestService_UploadChunk_RejectsBadRequests(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	// 非 multipart 请求体 → 413
	req := httptest.NewRequest(http.MethodPost, "/upload/chunk", strings.NewReader("not-multipart"))
	rec := httptest.NewRecorder()
	h.UploadChunk(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("非 multipart 应 413，得到 %d", rec.Code)
	}

	// 缺少 upload_id / chunk_index / 非法 checksum → 400
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("chunk_index", "0")
	_ = w.WriteField("chunk_checksum", strings.Repeat("a", 64))
	fw, _ := w.CreateFormFile("chunk", "chunk")
	_, _ = fw.Write([]byte("abcd"))
	_ = w.Close()
	req = httptest.NewRequest(http.MethodPost, "/upload/chunk", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec = httptest.NewRecorder()
	h.UploadChunk(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 upload_id 应 400，得到 %d", rec.Code)
	}

	// 会话不存在 → 404
	buf.Reset()
	w = multipart.NewWriter(&buf)
	_ = w.WriteField("upload_id", "missing-session")
	_ = w.WriteField("chunk_index", "0")
	_ = w.WriteField("chunk_checksum", strings.Repeat("a", 64))
	fw, _ = w.CreateFormFile("chunk", "chunk")
	_, _ = fw.Write([]byte("abcd"))
	_ = w.Close()
	req = httptest.NewRequest(http.MethodPost, "/upload/chunk", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rec = httptest.NewRecorder()
	h.UploadChunk(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知会话应 404，得到 %d", rec.Code)
	}
}

// TestService_ChunkedComplete_Errors 覆盖 complete 的拒绝路径（缺 upload_id / 未知会话）。
func TestService_ChunkedComplete_Errors(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	rec := env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 upload_id 应 400，得到 %d", rec.Code)
	}
	rec = env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": "nope"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("未知会话应 404，得到 %d", rec.Code)
	}
	// 分片未收齐 → 400
	if _, err := env.us.CreateSession("partial", "p.bin", 8, 4, 2, strings.Repeat("b", 64), 0); err != nil {
		t.Fatal(err)
	}
	rec = env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": "partial"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("分片未齐应 400，得到 %d body=%s", rec.Code, rec.Body.String())
	}
}
