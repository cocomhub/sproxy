// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_status_test.go 补齐分块上传状态查询（`GET /upload/status`）此前无包内覆盖的
// **按文件名**分支：有同名未完成会话 → 返回会话进度；无会话但目标文件已完成 → 返回完成态
// （含 checksum 台账命中与实时计算两条子路径）；租户/路径非法与目标不存在 → 拒绝/回落。
// 另一条按 upload_id 查询的分支已在 chunked_handlers_test.go 覆盖。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// statusByFilename 以 filename 查询上传状态（不带 upload_id），返回响应与解析结果。
func statusByFilename(t *testing.T, svc *Service, filename string) (*httptest.ResponseRecorder, ChunkStatusResponse) {
	t.Helper()
	req := httptest.NewRequest("GET", "/upload/status?filename="+filename, nil)
	rr := httptest.NewRecorder()
	svc.UploadStatus(rr, req)
	var resp ChunkStatusResponse
	if rr.Body.Len() > 0 {
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("响应体不是合法 JSON（%v）: %s", err, rr.Body.String())
		}
	}
	return rr, resp
}

// TestService_UploadStatus_ByFilename_FindsSession 覆盖按文件名命中未完成会话：
// 返回 upload_id、总分片数与缺失分片（而非回落文件存在性检查）。
func TestService_UploadStatus_ByFilename_FindsSession(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	if _, err := env.us.CreateSession("sid-file", "f.txt", 8, 4, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rr, resp := statusByFilename(t, svc, "f.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !resp.Success || resp.UploadID != "sid-file" || resp.TotalChunks != 2 {
		t.Fatalf("响应=%+v want {Success:true UploadID:sid-file TotalChunks:2}", resp)
	}
	if len(resp.MissingChunks) != 2 {
		t.Fatalf("未接收任何分片应报 2 个缺失, got %v", resp.MissingChunks)
	}
}

// TestService_UploadStatus_ByFilename_FileAlreadyCompleted 覆盖无会话但目标文件已完成：
// checksum 台账命中时直接回台账值；台账缺失时实时计算。
func TestService_UploadStatus_ByFilename_FileAlreadyCompleted(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	body := []byte("completed-body")

	cases := []struct {
		name        string
		seedLedger  bool
		wantChecksm bool
	}{
		{"台账命中", true, true},
		{"台账缺失时实时计算", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkFileAlreadyCompletedCase(t, tc.seedLedger, tc.wantChecksm, body)
		})
	}
}

// checkFileAlreadyCompletedCase 抽出的单用例断言 helper（降低
// TestService_UploadStatus_ByFilename_FileAlreadyCompleted 复杂度）：种子台账可选，断言完成态与
// checksum 台账/实时计算口径。
func checkFileAlreadyCompletedCase(t *testing.T, seedLedger, wantChecksm bool, body []byte) {
	t.Helper()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	abs := filepath.Join(env.dir, "user", "done.bin")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if seedLedger {
		env.cs.Set("user/done.bin", sha256Hex(body))
	}

	rr, resp := statusByFilename(t, svc, "done.bin")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !resp.Success || !resp.Completed || resp.Filename != "done.bin" {
		t.Fatalf("响应=%+v want 完成态", resp)
	}
	if !wantChecksm || resp.FileChecksum != sha256Hex(body) {
		t.Fatalf("FileChecksum=%q want %q", resp.FileChecksum, sha256Hex(body))
	}
}

// TestService_UploadStatus_ByFilename_MissingFileFallsThrough 覆盖无会话且目标文件不存在：
// 两条查询分支都不处理 → 统一回落 404「未找到文件或上传会话」。
func TestService_UploadStatus_ByFilename_MissingFileFallsThrough(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)

	rr, resp := statusByFilename(t, svc, "nope.bin")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("应 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp.Success || resp.Message != "未找到文件或上传会话" {
		t.Fatalf("响应=%+v want 未找到文件或上传会话", resp)
	}
}

// TestService_UploadStatus_ByFilename_RejectsInvalidFilename 覆盖按文件名查询的入口校验：
// 路径穿越在触盘前被拒（400），不泄露任何文件/会话存在性。
func TestService_UploadStatus_ByFilename_RejectsInvalidFilename(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)

	rr, resp := statusByFilename(t, svc, "..%2Fevil.txt")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("路径穿越应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp.Success || resp.Message != errMsgInvalidFilename {
		t.Fatalf("响应=%+v want %q", resp, errMsgInvalidFilename)
	}
}

// TestService_UploadStatus_ByFilename_TenantAndPathGuards 覆盖 checkFileExistsStatus 的
// 两条拒绝分支：owner 非法导致租户不可用；文件名通过入口校验但被 user 桶段名规则拒绝
// （`.__` 内部前缀）。两者都必须 400，且不得误报文件存在。
func TestService_UploadStatus_ByFilename_TenantAndPathGuards(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	cases := []struct {
		name     string
		actor    string
		filename string
	}{
		{"owner 非法（租户不可用）", "..", "f.txt"},
		{"段名非法（.__ 内部前缀）", "alice", ".__internal.txt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newDirsEnv(t) // UploadStoreFor 恒 nil → 直接走 checkFileExistsStatus
			req := httptest.NewRequest("GET", "/upload/status?filename="+tc.filename, nil)
			req.Header.Set("X-Test-Actor", tc.actor)
			rr := httptest.NewRecorder()
			env.svc.UploadStatus(rr, req)

			if rr.Code != http.StatusBadRequest {
				t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
			}
			var resp ChunkStatusResponse
			if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
				t.Fatalf("响应体不是合法 JSON: %s", rr.Body.String())
			}
			if resp.Success || resp.Message != errMsgInvalidPath {
				t.Fatalf("响应=%+v want %q", resp, errMsgInvalidPath)
			}
		})
	}
}

// TestService_UploadStatus_ByFilename_InFlightProgress 覆盖「无 chunked 会话但注册表有进行中
// 阶段进度」（进度可视化二期 T4）：单次上传在途时，按 filename 轮询即可拿到服务端加密 /
// 远端卷上传阶段；文件尚不可见 ⇒ 命中在途分支（而非 404），且不误报完成。
func TestService_UploadStatus_ByFilename_InFlightProgress(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	// 注册表键是逻辑路径（写入层 tnt.UserRel 口径：user/<rel>）。
	svc.progressRegistry().fnFor("user/inflight.bin")(syncpkg.StageRemote, 7, 20)

	rr, resp := statusByFilename(t, svc, "inflight.bin")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !resp.Success || resp.Stage != string(syncpkg.StageRemote) || resp.RemoteDone != 7 || resp.RemoteTotal != 20 {
		t.Fatalf("响应=%+v want 进行中远端阶段 7/20", resp)
	}
	if resp.Completed {
		t.Fatalf("进行中不得报完成: %+v", resp)
	}
	if resp.Filename != "inflight.bin" {
		t.Fatalf("Filename=%q want inflight.bin", resp.Filename)
	}
}

// TestService_UploadStatus_ByFilename_SessionIncludesStageProgress 覆盖按文件名命中会话时
// 同时带回服务端写入阶段进度（与按 upload_id 查询同一键口径）。
func TestService_UploadStatus_ByFilename_SessionIncludesStageProgress(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	if _, err := env.us.CreateSession("sid-stage", "f.txt", 8, 4, 2, "", 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.progressRegistry().fnFor("user/f.txt")(syncpkg.StageEncrypt, 3, 8)

	rr, resp := statusByFilename(t, svc, "f.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !resp.Success || resp.UploadID != "sid-stage" {
		t.Fatalf("响应=%+v want 会话态", resp)
	}
	if resp.Stage != string(syncpkg.StageEncrypt) || resp.EncryptDone != 3 || resp.EncryptTotal != 8 {
		t.Fatalf("会话响应未带回阶段进度: %+v", resp)
	}
}

// TestService_UploadStatus_ByUploadID_StageProgress 锁定「同源键口径缺陷一并修复」：写入层注册表
// 以 tnt.UserRel（user/<rel>）为键，而原 upload_id 路径传**用户可见路径**（无 user/ 前缀）
// ⇒ 阶段进度从未命中；统一为 progressRel() 后能返回。去掉键口径统一即红。
func TestService_UploadStatus_ByUploadID_StageProgress(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	sess, err := env.us.CreateSession("sid-up", "dir/u.bin", 8, 4, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	svc.progressRegistry().fnFor("user/dir/u.bin")(syncpkg.StageEncrypt, 4, 8)

	req := httptest.NewRequest("GET", "/upload/status?upload_id="+sess.UploadID, nil)
	rr := httptest.NewRecorder()
	svc.UploadStatus(rr, req)
	var resp ChunkStatusResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON: %s", rr.Body.String())
	}
	if resp.UploadID != "sid-up" {
		t.Fatalf("响应=%+v want 会话态", resp)
	}
	if resp.Stage != string(syncpkg.StageEncrypt) || resp.EncryptDone != 4 || resp.EncryptTotal != 8 {
		t.Fatalf("upload_id 路径未带回阶段进度: %+v", resp)
	}
}

// TestService_UploadStatus_ByFilename_CompletedBeatsStaleProgress 锁定「完成态优先于在途进度」：
// 上传已完成后进度条目在 TTL 内仍存活，按 filename 轮询仍必须回 Completed（否则 pkg/client
// 续传自愈会把已存在文件误判成进行中）。
func TestService_UploadStatus_ByFilename_CompletedBeatsStaleProgress(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newChunkedTestEnv(t)
	svc := env.handlers(4)
	body := []byte("done-body")
	abs := filepath.Join(env.dir, "user", "done.bin")
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(abs, body, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	env.cs.Set("user/done.bin", sha256Hex(body))
	// 目标文件已存在，但残留一条在途进度（模拟上传完成后 TTL 内的陈旧条目）。
	svc.progressRegistry().fnFor("user/done.bin")(syncpkg.StageRemote, 9, 9)

	rr, resp := statusByFilename(t, svc, "done.bin")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if !resp.Success || !resp.Completed || resp.FileChecksum != sha256Hex(body) {
		t.Fatalf("应回完成态而非在途进度: %+v", resp)
	}
}
