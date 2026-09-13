// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// read_contract_test.go 是只读面的**HTTP 契约钉住测试**（D-2 第 1 片的前置）。
//
// 为什么需要它：P2-a 把列表/搜索/stat/下载的领域逻辑搬进 `read_ops.go`、处理器降为薄适配。
// 「HTTP 契约零改动」不能只靠"测试都过了"来声称——既有用例覆盖的是**功能**，而本次重构
// 的真正风险在**失败路径的响应形状**（状态码 / body 形状 / 历史不对称）。本文件把那些
// 分支逐条钉成断言，从而：
//
//  1. 重构前跑：证明这些形状是**现状**（不是我以为的现状）；
//  2. 重构后跑：证明它们**没变**。
//
// 已知的历史不对称（**刻意保留**，改动它属契约变更、需单独决策）：
//   - `?volume= 不在视图` → 404 且 body **带**请求的 offset/limit；
//   - `subdir 非法` → 400 且 body **只含** files:[]（total/offset/limit 全为 0，即使请求带了 offset）；
//   - 旧装配路径的目录读取失败 → 500 且 body 是 `{"files":[]}`（**不是** ListResponse 形状）。
//
// 旧装配路径（VolSet 未装配）复用同一套用例：`dirsEnv.svc.rt.volSet()` 为 nil 时即该路径。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withFileResolve 装配下载路径解析替身（把 ?filename= 映射到 alice 的 user/<name>）并重建 Service。
//
// 为什么必须显式装配：`dirsEnv` 的 `resolveDownloadPath` 默认 nil，此时 Resolve 返回
// **零值 DownloadPath + nil error**（生产侧恒有装配层注入的真实解析器，故生产不存在该形态）。
// 未装配就直接调 Stat/Download 会 nil 解引用——本帮助函数即为此而设。
func withFileResolve(t *testing.T, env *dirsEnv, owner string) {
	t.Helper()
	tnt := env.tenantFor(owner)
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		name := r.URL.Query().Get("filename")
		return DownloadPath{Filename: name, Tenant: tnt, Rel: "user/" + name}, nil
	}
	env.rebuild()
}

// TestReadContract_ListVolumeNotInView_404WithPaging 钉住 404 分支：body 带请求的
// offset/limit（历史形状），Files 为空数组（非 null）。
func TestReadContract_ListVolumeNotInView_404WithPaging(t *testing.T) {
	env := newDirsEnv(t)
	// 必须走**多卷装配**：旧装配路径（VolSet nil）下 ?volume= 不参与过滤，恒 200。
	env.enableVolumes(t, "main")
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?volume=nope&offset=7&limit=3")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知卷应 404, got %d: %s", rr.Code, rr.Body.String())
	}
	// ListResponse 无 omitempty ⇒ 四个字段恒出现；历史形状的关键是 **offset/limit 带请求值**。
	if got := strings.TrimSpace(rr.Body.String()); got != `{"files":[],"total":0,"offset":7,"limit":3}` {
		t.Fatalf("404 body 应为 %s，got %s", `{"files":[],"total":0,"offset":7,"limit":3}`, got)
	}
}

// TestReadContract_ListBadSubdir_400BareFiles 钉住 400 分支：body **只含** files:[]，
// 即使请求带了 offset/limit 也不回填（历史不对称，刻意保留）。
func TestReadContract_ListBadSubdir_400BareFiles(t *testing.T) {
	env := newDirsEnv(t)
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=../etc&offset=7&limit=3")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("穿越 subdir 应 400, got %d: %s", rr.Code, rr.Body.String())
	}
	// 历史不对称：400 的 offset/limit 恒为 0（不回填请求值），即使客户端传了 offset=7&limit=3。
	if got := strings.TrimSpace(rr.Body.String()); got != `{"files":[],"total":0,"offset":0,"limit":0}` {
		t.Fatalf("400 body 应为全零分页（历史形状），got %s", got)
	}
}

// TestReadContract_ListEmptyDir_200WithPaging 钉住「目录不存在」成功分支：200 且带分页参数。
func TestReadContract_ListEmptyDir_200WithPaging(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "A")
	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?subdir=absent&offset=2&limit=5")
	if rr.Code != http.StatusOK {
		t.Fatalf("不存在的子目录应 200 空列表, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := strings.TrimSpace(rr.Body.String()); got != `{"files":[],"total":0,"offset":2,"limit":5}` {
		t.Fatalf("200 空列表 body 应为带请求分页的空数组，got %s", got)
	}
}

// TestReadContract_SearchEmptyQuery_400BareFiles 钉住搜索的空查询 400 形状（裸 files:[]）。
func TestReadContract_SearchEmptyQuery_400BareFiles(t *testing.T) {
	env := newDirsEnv(t)
	for _, target := range []string{"/api/files/search", "/api/files/search?q=", "/api/files/search?q=%20%20"} {
		rr := env.serve(env.svc.SearchFiles, "alice", "GET", target)
		if rr.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400, got %d: %s", target, rr.Code, rr.Body.String())
		}
		if got := strings.TrimSpace(rr.Body.String()); got != `{"files":[],"total":0,"offset":0,"limit":0}` {
			t.Fatalf("%s 的 400 body 应为全零分页，got %s", target, got)
		}
	}
}

// TestReadContract_SearchLimitEqualsTotal 钉住搜索的响应形状：Offset 恒 0、
// Limit == Total（历史语义：搜索不分页，用 limit 回填结果数）。
func TestReadContract_SearchLimitEqualsTotal(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/hit-1.txt", "A")
	writeUserFile(t, env, "alice", "user/sub/hit-2.txt", "B")

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=hit")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 || resp.Offset != 0 || resp.Limit != 2 {
		t.Fatalf("搜索应 Offset=0 且 Limit==Total=2, got total=%d offset=%d limit=%d", resp.Total, resp.Offset, resp.Limit)
	}
}

// TestReadContract_StatNotFound_PlainText404 钉住 stat 的 404 形状：**纯文本** "not found\n"
// （不是 JSON），因为 stat 用 http.Error 而非 sendJSON。
func TestReadContract_StatNotFound_PlainText404(t *testing.T) {
	env := newDirsEnv(t)
	withFileResolve(t, env, "alice")
	rr := env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=absent.txt")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rr.Code)
	}
	if got := rr.Body.String(); got != "not found\n" {
		t.Fatalf("stat 404 body 必须是纯文本 \"not found\\n\"，got %q", got)
	}
}

// TestReadContract_StatHeaders 钉住 stat 成功时的响应头集合与取值语义。
func TestReadContract_StatHeaders(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "AAA")
	env.checksumStoreFor("alice").Set("user/a.txt", sha256Hex([]byte("AAA")))
	withFileResolve(t, env, "alice")

	rr := env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=a.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-File-Size"); got != "3" {
		t.Fatalf("X-File-Size=%q want 3", got)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sha256Hex([]byte("AAA")) {
		t.Fatalf("%s=%q want 台账值", headerFileChecksum, got)
	}
	if got := rr.Header().Get(headerFileMTime); got == "" {
		t.Fatal("X-File-MTime 应存在")
	}
	if got := rr.Header().Get("X-File-IsDir"); got != "" {
		t.Fatalf("普通文件不应有 X-File-IsDir，got %q", got)
	}

	// 目录：X-File-IsDir=true 且 Size 仍给出（历史行为）
	if err := os.MkdirAll(filepath.Join(env.root, "alice", "user", "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	rr = env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=dir")
	if rr.Code != http.StatusOK {
		t.Fatalf("目录 stat 应 200, got %d", rr.Code)
	}
	if got := rr.Header().Get("X-File-IsDir"); got != "true" {
		t.Fatalf("目录应有 X-File-IsDir=true, got %q", got)
	}
}

// TestReadContract_DownloadHeaders 钉住下载成功时的响应头（Content-Disposition /
// Content-Type / Accept-Ranges / checksum / mtime）。
func TestReadContract_DownloadHeaders(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "AAA")
	env.checksumStoreFor("alice").Set("user/a.txt", sha256Hex([]byte("AAA")))
	withFileResolve(t, env, "alice")

	rr := env.serve(env.svc.Download, "alice", "GET", "/download?filename=a.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "AAA" {
		t.Fatalf("body=%q want AAA", got)
	}
	if got := rr.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges=%q want bytes", got)
	}
	if got := rr.Header().Get(headerContentType); got != contentTypeOctetStream {
		t.Fatalf("Content-Type=%q want %q", got, contentTypeOctetStream)
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "attachment") {
		t.Fatalf("Content-Disposition=%q 应含 attachment", got)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sha256Hex([]byte("AAA")) {
		t.Fatalf("%s=%q want 台账值", headerFileChecksum, got)
	}
	if got := rr.Header().Get(headerFileMTime); got == "" {
		t.Fatal("X-File-MTime 应存在")
	}
}

// TestReadContract_DownloadNotFound_JSON 钉住下载 404 形状：**JSON** UploadResponse
// （与 stat 的纯文本不同——两者历史上就不一致，刻意保留）。
func TestReadContract_DownloadNotFound_JSON(t *testing.T) {
	env := newDirsEnv(t)
	withFileResolve(t, env, "alice")
	rr := env.serve(env.svc.Download, "alice", "GET", "/download?filename=absent.txt")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404, got %d", rr.Code)
	}
	var resp UploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("下载 404 body 必须是 UploadResponse JSON: %v（body=%s）", err, rr.Body.String())
	}
	if resp.Success || resp.Message != errMsgFileNotFound {
		t.Fatalf("下载 404 body=%+v want {Success:false Message:%q}", resp, errMsgFileNotFound)
	}
}
