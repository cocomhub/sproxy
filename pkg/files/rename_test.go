// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// rename_test.go 是文件服务**写面**重命名族（`POST /rename` 与 `POST /api/batch/rename`）
// 的域级测试：用 `newDirsEnv` 注入的最小装配件直接驱动 `*Service`。覆盖单条成功/同源同目标/
// 缺 checksum/源缺失/目标已存在/checksum 不匹配/参数非法/自动建父目录，以及批量族的
// 混合结果（单条失败不中断）、空列表与非法 JSON。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// renameReq 构造 `POST /rename?from=&to=` 请求（checksum 空则不设头）。
func renameReq(actor, from, to, checksum string) *http.Request {
	q := url.Values{}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	req := httptest.NewRequest("POST", "/rename?"+q.Encode(), nil)
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	return req
}

// postJSONReq 构造带 JSON body 的 POST 请求。
func postJSONReq(t *testing.T, actor, target string, payload any) *http.Request {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal payload: %v", err)
	}
	req := httptest.NewRequest("POST", target, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	return req
}

// decodeBatch 解析批量响应外壳。
func decodeBatch(t *testing.T, rr *httptest.ResponseRecorder) BatchResponse {
	t.Helper()
	var out BatchResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不是合法 JSON（%v）: %s", err, rr.Body.String())
	}
	return out
}

// TestService_Rename_Success 覆盖成功重命名：200 + 消息、源消失、目标出现、checksum 台账
// 随 key 一并迁移（从 old 移到 new）。
func TestService_Rename_Success(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const body = "rename-me"
	writeUserFile(t, env, "alice", "user/a.txt", body)
	cs := env.checksumStoreFor("alice")
	cs.Set("user/a.txt", sha256Hex([]byte(body)))

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "b.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("重命名应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeResp(t, rr)
	if !resp.Success || resp.Message != "文件已重命名: a.txt -> b.txt" {
		t.Fatalf("响应=%+v want 成功外壳", resp)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("源文件应已消失, stat err=%v", err)
	}
	if got := mustReadUserFile(t, env, "alice", "user/b.txt"); got != body {
		t.Fatalf("目标内容=%q want %q", got, body)
	}
	if _, ok := cs.Get("user/a.txt"); ok {
		t.Fatal("checksum 台账源 key 应已删除")
	}
	if got, ok := cs.Get("user/b.txt"); !ok || got != sha256Hex([]byte(body)) {
		t.Fatalf("checksum 台账目标 key=%q ok=%v", got, ok)
	}
}

// TestService_Rename_SameSourceAndTarget 覆盖 from==to 短路：200「源与目标相同，无需移动」。
func TestService_Rename_SameSourceAndTarget(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "a.txt", "deadbeef"))
	if rr.Code != http.StatusOK {
		t.Fatalf("同源同目标应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); !resp.Success || resp.Message != "源与目标相同，无需移动" {
		t.Fatalf("响应=%+v want 同源同目标外壳", resp)
	}
}

// TestService_Rename_RejectsMissingChecksum 覆盖缺少 X-File-Checksum → 400。
func TestService_Rename_RejectsMissingChecksum(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "b.txt", ""))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("缺 checksum 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgMissingChecksum {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgMissingChecksum)
	}
}

// TestService_Rename_MissingSource 覆盖源文件不存在 → 404。
func TestService_Rename_MissingSource(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "nope.txt", "b.txt", "deadbeef"))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("源缺失应 404, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "源文件不存在" {
		t.Fatalf("Message=%q want 源文件不存在", resp.Message)
	}
}

// TestService_Rename_TargetExists 覆盖目标已存在 → 409（不覆盖）。
func TestService_Rename_TargetExists(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/a.txt", "AAA")
	writeUserFile(t, env, "alice", "user/b.txt", "BBB")

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "b.txt", sha256Hex([]byte("AAA"))))
	if rr.Code != http.StatusConflict {
		t.Fatalf("目标已存在应 409, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "目标路径已存在" {
		t.Fatalf("Message=%q want 目标路径已存在", resp.Message)
	}
	if got := mustReadUserFile(t, env, "alice", "user/a.txt"); got != "AAA" {
		t.Fatalf("拒绝分支不得移动源文件: %q", got)
	}
}

// TestService_Rename_ChecksumMismatch 覆盖源 checksum 不匹配 → 400 + errMsgSrcChecksumFailed。
func TestService_Rename_ChecksumMismatch(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/a.txt", "real")

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "b.txt", sha256Hex([]byte("other"))))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("checksum 不匹配应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != errMsgSrcChecksumFailed {
		t.Fatalf("Message=%q want %q", resp.Message, errMsgSrcChecksumFailed)
	}
}

// TestService_Rename_CreatesParentDir 覆盖目标父目录自动创建（mkdir -p 中间目录）。
func TestService_Rename_CreatesParentDir(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	const body = "nested"
	writeUserFile(t, env, "alice", "user/a.txt", body)

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "sub/dir/b.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := mustReadUserFile(t, env, "alice", "user/sub/dir/b.txt"); got != body {
		t.Fatalf("目标内容=%q want %q", got, body)
	}
}

// TestService_Rename_RejectsBadParams 覆盖参数层的四条 400：缺 from、缺 to、源穿越、目标穿越。
func TestService_Rename_RejectsBadParams(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	cases := []struct {
		name    string
		from    string
		to      string
		wantMsg string
	}{
		{"缺 from", "", "b.txt", "from 和 to 都不能为空"},
		{"缺 to", "a.txt", "", "from 和 to 都不能为空"},
		{"源路径穿越", "../evil", "b.txt", "无效的源路径"},
		{"目标路径穿越", "a.txt", "../evil", "无效的目标路径"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			env.svc.Rename(rr, renameReq("alice", tc.from, tc.to, "deadbeef"))
			if rr.Code != http.StatusBadRequest {
				t.Fatalf("应 400, got %d: %s", rr.Code, rr.Body.String())
			}
			if resp := decodeResp(t, rr); resp.Message != tc.wantMsg {
				t.Fatalf("Message=%q want %q", resp.Message, tc.wantMsg)
			}
		})
	}
}

// TestService_BatchRename_MixedResults 覆盖批量重命名的「继续处理」语义：成功、源缺失、
// 无效路径、缺 checksum、目标已存在五种逐条结果互不影响，HTTP 仍 200。
func TestService_BatchRename_MixedResults(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/first.txt", "OK")
	writeUserFile(t, env, "alice", "user/src2.txt", "S2")
	writeUserFile(t, env, "alice", "user/ok2.txt", "OK2")
	writeUserFile(t, env, "alice", "user/dst.txt", "DST")

	req := postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{Operations: []BatchRenameOp{
		{From: "first.txt", To: "renamed.txt", Checksum: sha256Hex([]byte("OK"))},
		{From: "missing.txt", To: "x.txt", Checksum: "deadbeef"},
		{From: "../evil", To: "y.txt", Checksum: "deadbeef"},
		{From: "ok2.txt", To: "y.txt"},
		{From: "src2.txt", To: "dst.txt", Checksum: sha256Hex([]byte("S2"))},
	}})
	rr := httptest.NewRecorder()
	env.svc.BatchRename(rr, req)

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
		{true, "重命名成功"},
		{false, "源文件不存在"},
		{false, "无效的源路径"},
		{false, "缺少 checksum"},
		{false, "目标路径已存在"},
	}
	for i, w := range wants {
		if resp.Results[i].Success != w.success || resp.Results[i].Message != w.message {
			t.Fatalf("结果[%d]=%+v want {%v %q}", i, resp.Results[i], w.success, w.message)
		}
	}
	if got := mustReadUserFile(t, env, "alice", "user/renamed.txt"); got != "OK" {
		t.Fatalf("第一条应已重命名, 内容=%q", got)
	}
	if got := mustReadUserFile(t, env, "alice", "user/dst.txt"); got != "DST" {
		t.Fatalf("目标已存在分支不得覆盖: %q", got)
	}
}

// TestService_BatchRename_SameFromTo 覆盖批量的同源同目标短路（成功 + 专属文案）。
func TestService_BatchRename_SameFromTo(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	req := postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{Operations: []BatchRenameOp{
		{From: "same.txt", To: "same.txt"},
	}})
	rr := httptest.NewRecorder()
	env.svc.BatchRename(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatch(t, rr)
	if len(resp.Results) != 1 || !resp.Results[0].Success || resp.Results[0].Message != "源与目标相同，无需移动" {
		t.Fatalf("结果=%+v want 同源同目标", resp.Results)
	}
}

// TestService_BatchRename_RejectsBadRequests 覆盖三条 400：空 operations、非法 JSON、
// （附带）batch rename 的 continue 语义不因单条失败改变 HTTP 码。
func TestService_BatchRename_RejectsBadRequests(t *testing.T) {
	env := newDirsEnv(t)
	env.enableWriteDefaults()

	rr := httptest.NewRecorder()
	env.svc.BatchRename(rr, postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{}))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 operations 应 400, got %d", rr.Code)
	}
	if resp := decodeResp(t, rr); resp.Message != "operations 不能为空" {
		t.Fatalf("Message=%q want operations 不能为空", resp.Message)
	}

	badReq := httptest.NewRequest("POST", "/api/batch/rename", bytes.NewReader([]byte("{not json")))
	badReq.Header.Set("X-Test-Actor", "alice")
	rr = httptest.NewRecorder()
	env.svc.BatchRename(rr, badReq)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("非法 JSON 应 400, got %d", rr.Code)
	}
	if resp := decodeResp(t, rr); resp.Message != "无法解析请求体" {
		t.Fatalf("Message=%q want 无法解析请求体", resp.Message)
	}
}

// TestService_Rename_CrossScopeQuotaReserveFailure 覆盖跨 bucket_limits 子目录重命名的
// 配额拒绝分支：目标子 Scope 上限小于文件大小 → 507「目标目录配额不足」，源文件不动。
func TestService_Rename_CrossScopeQuotaReserveFailure(t *testing.T) {
	env := newDirsEnv(t)
	env.bucketLimits = map[string]int64{"user/sub": 1} // 上限 1 字节 < 文件大小
	env.enableWriteDefaults()

	const body = "quota-body" // 10 字节
	writeUserFile(t, env, "alice", "user/a.txt", body)

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "sub/b.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusInsufficientStorage {
		t.Fatalf("目标子目录配额不足应 507, got %d: %s", rr.Code, rr.Body.String())
	}
	if resp := decodeResp(t, rr); resp.Message != "目标目录配额不足" {
		t.Fatalf("Message=%q want 目标目录配额不足", resp.Message)
	}
	if got := mustReadUserFile(t, env, "alice", "user/a.txt"); got != body {
		t.Fatalf("配额拒绝分支不得移动源文件: %q", got)
	}
}

// TestService_Rename_CrossScopeQuotaTransfer 覆盖跨 bucket_limits 子目录重命名的成功分支：
// 目标子 Scope 先预留再按实际大小 Commit，源桶键释放（用户桶聚合占用不变，子 Scope 从 0 → size）。
func TestService_Rename_CrossScopeQuotaTransfer(t *testing.T) {
	env := newDirsEnv(t)
	env.bucketLimits = map[string]int64{"user/sub": 1 << 20} // 充足上限，但足以形成独立子 Scope
	env.enableWriteDefaults()

	const body = "quota-ok" // 8 字节
	writeUserFile(t, env, "alice", "user/a.txt", body)

	fromScope := env.quotaScopeFor("alice", "user/a.txt")
	toScope := env.quotaScopeFor("alice", "user/sub/b.txt")
	if fromScope == nil || toScope == nil {
		t.Fatal("两个功能桶 Scope 都应可用")
	}
	if fromScope == toScope {
		t.Fatal("bucket_limits 子目录应解析为独立 Scope（否则不触发跨键转移分支）")
	}
	// 预置源键已确认占用（模拟上传后的 committed），使释放可观测。
	res, err := fromScope.TryReserve(int64(len(body)))
	if err != nil {
		t.Fatalf("源 Scope 预留失败: %v", err)
	}
	res.Commit(int64(len(body)))
	if got := toScope.Usage(); got != 0 {
		t.Fatalf("转移前目标子 Scope 占用应为 0, got %d", got)
	}

	rr := httptest.NewRecorder()
	env.svc.Rename(rr, renameReq("alice", "a.txt", "sub/b.txt", sha256Hex([]byte(body))))
	if rr.Code != http.StatusOK {
		t.Fatalf("重命名应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := toScope.Usage(); got != int64(len(body)) {
		t.Fatalf("转移后目标子 Scope 占用=%d want %d", got, len(body))
	}
	if got := fromScope.Usage(); got != int64(len(body)) {
		t.Fatalf("用户桶聚合占用应不变（%d），got %d", len(body), got)
	}
	if got := mustReadUserFile(t, env, "alice", "user/sub/b.txt"); got != body {
		t.Fatalf("目标内容=%q want %q", got, body)
	}
}

// TestService_BatchRename_CrossScopeQuota 覆盖批量重命名的跨 bucket_limits 子目录配额分支：
// 目标子 Scope 上限不足 → 该条「目标目录配额不足」；上限充足 → 该条成功并完成记账转移。
func TestService_BatchRename_CrossScopeQuota(t *testing.T) {
	env := newDirsEnv(t)
	env.bucketLimits = map[string]int64{"user/small": 1, "user/big": 1 << 20}
	env.enableWriteDefaults()

	writeUserFile(t, env, "alice", "user/s1.txt", "0123456789") // 10 字节 > 1
	writeUserFile(t, env, "alice", "user/s2.txt", "OK")

	req := postJSONReq(t, "alice", "/api/batch/rename", BatchRenameRequest{Operations: []BatchRenameOp{
		{From: "s1.txt", To: "small/a.txt", Checksum: sha256Hex([]byte("0123456789"))},
		{From: "s2.txt", To: "big/b.txt", Checksum: sha256Hex([]byte("OK"))},
	}})
	rr := httptest.NewRecorder()
	env.svc.BatchRename(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("批量应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeBatch(t, rr)
	if len(resp.Results) != 2 {
		t.Fatalf("应 2 条结果, got %+v", resp.Results)
	}
	if resp.Results[0].Success || resp.Results[0].Message != "目标目录配额不足" {
		t.Fatalf("结果[0]=%+v want 目标目录配额不足", resp.Results[0])
	}
	if !resp.Results[1].Success || resp.Results[1].Message != "重命名成功" {
		t.Fatalf("结果[1]=%+v want 重命名成功", resp.Results[1])
	}
	if got := mustReadUserFile(t, env, "alice", "user/big/b.txt"); got != "OK" {
		t.Fatalf("第二条应已重命名, 内容=%q", got)
	}
	if got := env.quotaScopeFor("alice", "user/big/b.txt").Usage(); got != 2 {
		t.Fatalf("目标子 Scope 应记 2 字节, got %d", got)
	}
}
