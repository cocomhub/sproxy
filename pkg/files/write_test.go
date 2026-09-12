// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// write_test.go 是文件服务**写面**单次上传族（`POST /upload`）的域级测试：用
// `newDirsEnv`（见 dirs_test.go 的等价范围声明）注入的最小装配件直接驱动 `*Service`，
// 不经装配层（pkg/server 的 Handlers 薄适配）。
//
// 覆盖范围（提升迁移前 pkg/files 写面 0% 的包内覆盖）：成功落盘/响应头/checksum 台账/
// 双账本 no-op 结算、幂等重传、冲突（versioning 关）、版本化覆盖写、并发锁、缺 checksum、
// 非法路径、客户端 checksum 不匹配（清理已写文件）、卷路由错误映射。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// fakeMetrics 是 Metrics 接缝的替身：记录写面调用次数/字节，供用例断言计量落点。
type fakeMetrics struct {
	uploadBytes int64
	uploadCalls int
	deleteCalls int
}

func (m *fakeMetrics) RecordUpload(bytes int64) { m.uploadBytes += bytes; m.uploadCalls++ }
func (m *fakeMetrics) RecordDownload(int64)     {}
func (m *fakeMetrics) RecordDelete()            { m.deleteCalls++ }

// locateOwnerFileDefault 复刻生产 locateOwnerFile 的单卷/默认卷快路径：stat 命中即定位
// （默认卷名 = 卷集合默认卷；VolSet 未装配时为空串）。多卷下的逐卷探测不在替身范围。
func (e *dirsEnv) locateOwnerFileDefault(owner, rel string) (FileLocation, bool) {
	owner = normalizeOwner(owner)
	tnt := e.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return FileLocation{}, false
	}
	if _, err := tnt.Root().Stat(rel); err != nil {
		return FileLocation{}, false
	}
	vol := ""
	if e.volSet != nil {
		vol = e.volSet.Default().Name
	}
	return FileLocation{VolumeName: vol, Tenant: tnt}, true
}

// routeUploadDefault 复刻生产 routeUpload 的单卷/默认卷形态：返回默认卷租户 + 空账本
// （Scope/Pool 均 nil → UploadRoute.Commit/Release 为 no-op）。显式卷 ACL/唯一性与容量
// 路由语义不在替身范围（那些分支由 pkg/server 的集成测试覆盖）。
func (e *dirsEnv) routeUploadDefault(owner, rel, explicitVol string, size int64, forceHomeVol string) (UploadRoute, error) {
	tnt := e.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return UploadRoute{}, &HTTPError{Status: http.StatusBadRequest, Message: errMsgInvalidPath}
	}
	vol := ""
	if e.volSet != nil {
		vol = e.volSet.Default().Name
	}
	return UploadRoute{VolumeName: vol, Tenant: tnt, Release: func() {}}, nil
}

// enableWriteDefaults 打开写面族的两条真实装配路径（写前视图定位 + 卷路由）并重建 Service。
func (e *dirsEnv) enableWriteDefaults() {
	if e.locateOwnerFile == nil {
		e.locateOwnerFile = e.locateOwnerFileDefault
	}
	if e.routeUpload == nil {
		e.routeUpload = e.routeUploadDefault
	}
	e.rebuild()
}

// uploadReq 构造 multipart 上传请求。remotePath 空则不设 X-File-Path（回落 multipart
// 文件名）；checksum 空则不设 X-File-Checksum；mtimeNano > 0 时设 X-File-MTime。
func uploadReq(t *testing.T, actor, remotePath string, body []byte, checksum string, mtimeNano int64) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	name := remotePath
	if name == "" {
		name = "f.txt"
	}
	part, err := mw.CreateFormFile("file", filepath.Base(name))
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatalf("写 multipart part: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("关闭 multipart writer: %v", err)
	}
	req := httptest.NewRequest("POST", "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	if remotePath != "" {
		req.Header.Set("X-File-Path", remotePath)
	}
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	if mtimeNano > 0 {
		req.Header.Set(headerFileMTime, strconv.FormatInt(mtimeNano, 10))
	}
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	return req
}

// upload 直接驱动 Upload 处理器并返回响应。
func (e *dirsEnv) upload(t *testing.T, actor, remotePath string, body []byte, checksum string, mtimeNano int64) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	e.svc.Upload(rr, uploadReq(t, actor, remotePath, body, checksum, mtimeNano))
	return rr
}

// mustReadUserFile 读取 owner 用户桶内文件内容（helper 失败即 fatal）。
func mustReadUserFile(t *testing.T, env *dirsEnv, owner, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(env.root, owner, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("读取 %s/%s 失败: %v", owner, rel, err)
	}
	return string(b)
}

// TestService_Upload_SuccessWritesFileAndHeaders 覆盖成功上传的完整落点：200 + JSON 外壳
// （含 checksum）、文件落 <root>/<owner>/user/<rel>、X-File-Checksum 响应头、checksum 台账
// 记录（key = 租户根内 rel）、X-File-MTime 生效、Metrics.RecordUpload 入账。
func TestService_Upload_SuccessWritesFileAndHeaders(t *testing.T) {
	env := newDirsEnv(t)
	env.metrics = &fakeMetrics{}
	env.enableWriteDefaults()

	const body = "hello upload"
	mtime := time.Date(2023, 11, 14, 22, 13, 20, 0, time.UTC).UnixNano()
	rr := env.upload(t, "alice", "dir/a.txt", []byte(body), sha256Hex([]byte(body)), mtime)

	if rr.Code != http.StatusOK {
		t.Fatalf("上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type=%q want application/json", ct)
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Checksum != sha256Hex([]byte(body)) {
		t.Fatalf("响应=%+v want {Success:true Checksum:%s}", resp, sha256Hex([]byte(body)))
	}
	if want := "文件上传成功, size: " + strconv.Itoa(len(body)); resp.Message != want {
		t.Fatalf("Message=%q want %q", resp.Message, want)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sha256Hex([]byte(body)) {
		t.Fatalf("X-File-Checksum=%q want %q", got, sha256Hex([]byte(body)))
	}
	if got := mustReadUserFile(t, env, "alice", "user/dir/a.txt"); got != body {
		t.Fatalf("落盘内容=%q want %q", got, body)
	}
	// checksum 台账记录（key = 租户根内 rel，无 owner 前缀）。
	cs := env.checksumStoreFor("alice")
	if cs == nil {
		t.Fatal("checksumStoreFor(alice) 应为非 nil")
	}
	if got, ok := cs.Get("user/dir/a.txt"); !ok || got != sha256Hex([]byte(body)) {
		t.Fatalf("checksum 台账=%q ok=%v want %q", got, ok, sha256Hex([]byte(body)))
	}
	// X-File-MTime 生效（秒级断言，避开文件系统纳秒精度差异）。
	info, err := os.Stat(filepath.Join(env.root, "alice", "user", "dir", "a.txt"))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.ModTime().Unix(); got != mtime/1e9 {
		t.Fatalf("mtime=%d want %d", got, mtime/1e9)
	}
	if env.metrics.uploadCalls != 1 || env.metrics.uploadBytes != int64(len(body)) {
		t.Fatalf("Metrics=%+v want {uploadCalls:1 uploadBytes:%d}", env.metrics, len(body))
	}
}

// TestService_Upload_IdempotentSameChecksum 覆盖幂等重传：同名 + 同 checksum 直接 200
// 「文件已上传成功」，不重复写盘、不保存版本。
func TestService_Upload_IdempotentSameChecksum(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const body = "same content"
	writeUserFile(t, env, "alice", "user/f.txt", body)

	rr := env.upload(t, "alice", "f.txt", []byte(body), sha256Hex([]byte(body)), 0)
	if rr.Code != http.StatusOK {
		t.Fatalf("幂等重传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Message != "文件已上传成功, size: "+strconv.Itoa(len(body)) {
		t.Fatalf("响应=%+v want 幂等成功外壳", resp)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sha256Hex([]byte(body)) {
		t.Fatalf("幂等分支应带 X-File-Checksum=%q, got %q", sha256Hex([]byte(body)), got)
	}
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != body {
		t.Fatalf("幂等重传不应改内容: %q", got)
	}
}

// TestService_Upload_ConflictWhenChecksumDiffersAndVersioningOff 覆盖 versioning 关闭时
// 同名不同 checksum → 409 + 附带服务端实际 checksum，且不覆盖原文件。
func TestService_Upload_ConflictWhenChecksumDiffersAndVersioningOff(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/f.txt", "old-content")
	newBody := []byte("new-content")

	rr := env.upload(t, "alice", "f.txt", newBody, sha256Hex(newBody), 0)
	if rr.Code != http.StatusConflict {
		t.Fatalf("checksum 冲突应 409, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResp(t, rr)
	if resp.Success || resp.Message != "文件已存在，但校验失败" {
		t.Fatalf("响应=%+v want 409 冲突文案", resp)
	}
	if resp.Checksum != sha256Hex([]byte("old-content")) {
		t.Fatalf("冲突响应应附服务端实际 checksum=%q, got %q", sha256Hex([]byte("old-content")), resp.Checksum)
	}
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != "old-content" {
		t.Fatalf("冲突分支不得覆盖原文件: %q", got)
	}
}

// TestService_Upload_VersioningOverwriteSavesVersion 覆盖 versioning 开启时同名不同
// checksum 走版本化覆盖写：旧内容保存为版本、新内容落盘、响应 200。
func TestService_Upload_VersioningOverwriteSavesVersion(t *testing.T) {
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/f.txt", "v1-content")
	newBody := []byte("v2-content")

	rr := env.upload(t, "alice", "f.txt", newBody, sha256Hex(newBody), 0)
	if rr.Code != http.StatusOK {
		t.Fatalf("版本化覆盖应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != string(newBody) {
		t.Fatalf("覆盖后内容=%q want %q", got, newBody)
	}
	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("应保存 1 个版本, got %d err=%v", len(entries), err)
	}
	tnt := env.tenantFor("alice")
	verRel, ok := tnt.FeatureRel("version", "f.txt")
	if !ok {
		t.Fatal("FeatureRel(version, f.txt) 失败")
	}
	b, err := os.ReadFile(filepath.Join(env.root, "alice", filepath.FromSlash(verRel+"/"+entries[0].Name)))
	if err != nil {
		t.Fatalf("读取版本文件: %v", err)
	}
	if string(b) != "v1-content" {
		t.Fatalf("版本内容=%q want v1-content", string(b))
	}
}

// TestService_Upload_RejectsWhenFileLocked 覆盖并发上传防护：锁池已有同 (owner, rel)
// 条目 → 409「文件正在上传中」，不写盘。
func TestService_Upload_RejectsWhenFileLocked(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("locked")
	if _, ok := env.svc.rt.fileLocks().TryMark("alice", "user/f.txt", uploadingLockUpload); !ok {
		t.Fatal("前置：锁应可获取（release 故意不调，保持占用）")
	}

	rr := env.upload(t, "alice", "f.txt", body, sha256Hex(body), 0)
	if rr.Code != http.StatusConflict {
		t.Fatalf("锁占用应 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "文件正在上传中" {
		t.Fatalf("Message=%q want 文件正在上传中", resp.Message)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("锁占用分支不得写盘, stat err=%v", err)
	}
}

// TestService_Upload_RejectsMissingChecksum 覆盖缺少 X-File-Checksum → 400。
func TestService_Upload_RejectsMissingChecksum(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := env.upload(t, "alice", "f.txt", []byte("x"), "", 0)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 checksum 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgMissingChecksum {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgMissingChecksum)
	}
}

// TestService_Upload_RejectsBadPath 覆盖路径穿越：X-File-Path=../evil → 400，不落盘。
func TestService_Upload_RejectsBadPath(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	body := []byte("evil")
	rr := env.upload(t, "alice", "../evil", body, sha256Hex(body), 0)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("路径穿越应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Success {
		t.Fatalf("路径穿越不得成功: %+v", resp)
	}
}

// TestService_Upload_RejectsClientChecksumMismatch 覆盖客户端上报 checksum 与服务端实际
// 不符：400 + 清理已写入文件（不留半成品）。
func TestService_Upload_RejectsClientChecksumMismatch(t *testing.T) {
	env := newDirsEnv(t)
	env.metrics = &fakeMetrics{}
	env.enableWriteDefaults()

	wrong := sha256Hex([]byte("other"))
	rr := env.upload(t, "alice", "f.txt", []byte("actual-data"), wrong, 0)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("校验失败应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "文件 SHA-256 校验失败" {
		t.Fatalf("Message=%q want 文件 SHA-256 校验失败", resp.Message)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("校验失败应清理已写文件, stat err=%v", err)
	}
	if env.metrics.uploadCalls != 0 {
		t.Fatalf("校验失败不应计入上传字节, got %+v", env.metrics)
	}
}

// TestService_Upload_RouteErrorMapsStatus 覆盖卷路由失败的两种映射：HTTPError 按其状态码
// 与文案回包；普通错误回落 500 + errMsgSaveFailed。
func TestService_Upload_RouteErrorMapsStatus(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantMsg    string
	}{
		{"带状态码的路由错误", &HTTPError{Status: http.StatusInsufficientStorage, Message: "存储配额不足"}, http.StatusInsufficientStorage, "存储配额不足"},
		{"普通错误回落 500", os.ErrPermission, http.StatusInternalServerError, errMsgSaveFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t)
			env.enableWriteDefaults()
			env.routeUpload = func(string, string, string, int64, string) (UploadRoute, error) {
				return UploadRoute{}, tc.err
			}
			env.rebuild()

			body := []byte("x")
			rr := env.upload(t, "alice", "f.txt", body, sha256Hex(body), 0)
			if rr.Code != tc.wantStatus {
				t.Fatalf("状态码=%d want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}
			if resp := decodeResp(t, rr); resp.Message != tc.wantMsg {
				t.Fatalf("Message=%q want %q", resp.Message, tc.wantMsg)
			}
		})
	}
}

// TestService_Upload_JSONEncodeFailureFallback 覆盖 sendJSON 的序列化失败兜底不可达性：
// UploadResponse 恒可序列化，故此处只钉住正常外壳可被 json.Unmarshal 解析（守卫 DTO 形状
// 与响应写出路径一致）。真正的 encode 失败分支由 chunked_response 冻结表覆盖。
func TestService_Upload_ResponseIsParseableJSON(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	body := []byte("json")
	rr := env.upload(t, "alice", "f.txt", body, sha256Hex(body), 0)
	var out UploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不可解析: %v / %s", err, rr.Body.String())
	}
}
