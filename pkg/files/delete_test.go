// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// delete_test.go 是文件服务**写面**删除族（`POST /delete` 与 `POST /api/batch/delete`）的
// 域级测试：用 `newDirsEnv` 注入的最小装配件直接驱动 `*Service`。覆盖单条成功（含 checksum
// 台账清理与 Metrics）、缺 filename/非法 filename/缺 checksum、文件不存在、checksum 不匹配、
// 文件级互斥占用，以及批量族的继续处理语义、空列表与非法 JSON。

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// deleteReq 构造 `POST /delete?filename=` 请求（checksum 空则不设头）。
func deleteReq(actor, filename, checksum string) *http.Request {
	q := url.Values{}
	if filename != "" {
		q.Set("filename", filename)
	}
	req := httptest.NewRequest("POST", "/delete?"+q.Encode(), nil)
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	return req
}

// TestService_Delete_Success 覆盖成功删除：200 + 消息、文件消失、checksum 台账条目清理、
// Metrics.RecordDelete 入账。
func TestService_Delete_Success(t *testing.T) {
	env := newDirsEnv(t)
	env.metrics = &fakeMetrics{}
	env.enableWriteDefaults()

	const body = "delete-me"
	writeUserFile(t, env, "alice", "user/f.txt", body)
	cs := env.checksumStoreFor("alice")
	cs.Set("user/f.txt", sha256Hex([]byte(body)))

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("删除应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Message != "文件删除成功: f.txt" {
		t.Fatalf("响应=%+v want 成功外壳", resp)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); !os.IsNotExist(err) {
		t.Fatalf("文件应已删除, stat err=%v", err)
	}
	if _, ok := cs.Get("user/f.txt"); ok {
		t.Fatal("checksum 台账条目应已清理")
	}
	if env.metrics.deleteCalls != 1 {
		t.Fatalf("RecordDelete 应调用 1 次, got %d", env.metrics.deleteCalls)
	}
}

// TestService_Delete_RejectsMissingFilename 覆盖 filename 查询参数缺失 → 400。
func TestService_Delete_RejectsMissingFilename(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "", "deadbeef"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 filename 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgEmptyFilename {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgEmptyFilename)
	}
}

// TestService_Delete_RejectsInvalidFilename 覆盖 filename 路径穿越 → 400。
func TestService_Delete_RejectsInvalidFilename(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "../evil", "deadbeef"))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("路径穿越应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgInvalidFilename {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgInvalidFilename)
	}
}

// TestService_Delete_RejectsMissingChecksum 覆盖缺少 X-File-Checksum → 400（在触盘前拒绝）。
func TestService_Delete_RejectsMissingChecksum(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/f.txt", "X")
	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 checksum 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgMissingChecksum {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgMissingChecksum)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); err != nil {
		t.Fatalf("拒绝分支不得删除文件: %v", err)
	}
}

// TestService_Delete_MissingFile 覆盖文件不存在 → 404。
func TestService_Delete_MissingFile(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "nope.txt", "deadbeef"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("文件不存在应 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "文件不存在" {
		t.Fatalf("Message=%q want 文件不存在", resp.Message)
	}
}

// TestService_Delete_ChecksumMismatch 覆盖 checksum 不匹配 → 400，文件保留。
func TestService_Delete_ChecksumMismatch(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/f.txt", "keep-me")
	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte("other"))))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("checksum 不匹配应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "文件校验失败" {
		t.Fatalf("Message=%q want 文件校验失败", resp.Message)
	}
	if got := mustReadUserFile(t, env, "alice", "user/f.txt"); got != "keep-me" {
		t.Fatalf("拒绝分支不得删除文件: %q", got)
	}
}

// TestService_Delete_RejectsWhenLocked 覆盖文件级互斥被占用 → 409（move/上传窗口闭合）。
func TestService_Delete_RejectsWhenLocked(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	env.acquireFileLock = func(string, string) (func(), bool) { return nil, false }
	env.rebuild()

	writeUserFile(t, env, "alice", "user/f.txt", "locked")
	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "f.txt", sha256Hex([]byte("locked"))))
	if rr.Code != http.StatusConflict {
		t.Fatalf("锁占用应 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "文件正在移动/上传中，请稍后重试" {
		t.Fatalf("Message=%q want 锁占用文案", resp.Message)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "f.txt")); err != nil {
		t.Fatalf("锁占用分支不得删除文件: %v", err)
	}
}

// TestService_BatchDelete_MixedResults 覆盖批量删除的五种逐条结果：成功、幂等缺失、
// 缺 checksum、checksum 不匹配、无效路径；HTTP 恒 200（继续处理）。
func TestService_BatchDelete_MixedResults(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/gone.txt", "G")
	writeUserFile(t, env, "alice", "user/nocs.txt", "N")
	writeUserFile(t, env, "alice", "user/bad.txt", "B")

	req := postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{Files: []BatchDeleteFile{
		{Filename: "gone.txt", Checksum: sha256Hex([]byte("G"))},
		{Filename: "missing.txt", Checksum: "deadbeef"},
		{Filename: "nocs.txt"},
		{Filename: "bad.txt", Checksum: sha256Hex([]byte("WRONG"))},
		{Filename: "../evil", Checksum: "deadbeef"},
	}})
	rr := httptest.NewRecorder()
	env.svc.BatchDelete(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("批量应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatch(t, rr)
	if len(resp.Results) != 5 {
		t.Fatalf("应 5 条结果, got %d: %+v", len(resp.Results), resp.Results)
	}
	wants := []struct {
		success bool
		message string
	}{
		{true, "删除成功"},
		{true, "文件不存在（幂等删除）"},
		{false, "缺少 checksum"},
		{false, "文件校验失败"},
		{false, "无效的文件路径"},
	}
	for i, w := range wants {
		if resp.Results[i].Success != w.success || resp.Results[i].Message != w.message {
			t.Fatalf("结果[%d]=%+v want {%v %q}", i, resp.Results[i], w.success, w.message)
		}
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("第一条应已删除, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "bad.txt")); err != nil {
		t.Fatalf("checksum 不匹配分支不得删除: %v", err)
	}
}

// TestService_BatchDelete_RejectsBadRequests 覆盖空 files 与非法 JSON 两条 400。
func TestService_BatchDelete_RejectsBadRequests(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.BatchDelete(rr, postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{}))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 files 应 400, got %d", rr.Code)
	}
	if resp := decodeResp(t, rr); resp.Message != "files 不能为空" {
		t.Fatalf("Message=%q want files 不能为空", resp.Message)
	}

	badReq := httptest.NewRequest("POST", "/api/batch/delete", bytes.NewReader([]byte("{not json")))
	badReq.Header.Set("X-Test-Actor", "alice")
	rr = httptest.NewRecorder()
	env.svc.BatchDelete(rr, badReq)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400, got %d", rr.Code)
	}
	if resp := decodeResp(t, rr); resp.Message != "无法解析请求体" {
		t.Fatalf("Message=%q want 无法解析请求体", resp.Message)
	}
}
