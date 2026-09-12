// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// read_test.go 是只读面（列表 / 搜索 / 下载 / stat）的**域级测试**：环境与与 pkg/server 的
// 分工见 dirs_test.go 文件头（真实下层能力 + 注入的最小装配替身，不依赖装配层）。
//
// 其中 `TestParsePagination_*`（7 条）与 `TestSortFileEntries`（1 条）随处理器自
// `pkg/server/{handlers_test,list_handler_test}.go` **原样迁入**——它们直测的函数
// （parsePagination / sortFileEntries）已平铺到本包，用例名与断言语义逐字保留。
// 其余为本片新增的域级用例（列表聚合/过滤、下载 Range 与 checksum 响应头、stat 元信息、
// 递归搜索），覆盖搬迁后各处理器的关键分支。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestParsePagination_Defaults(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_NegativeOffset(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=-1", nil)
	offset, _ := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
}

func TestParsePagination_ZeroLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=0", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_LargeLimit(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?limit=99999", nil)
	_, limit := parsePagination(r)
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_Valid(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=10&limit=50", nil)
	offset, limit := parsePagination(r)
	if offset != 10 {
		t.Errorf("offset = %d, want 10", offset)
	}
	if limit != 50 {
		t.Errorf("limit = %d, want 50", limit)
	}
}

func TestParsePagination_NonNumeric(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=abc&limit=xyz", nil)
	offset, limit := parsePagination(r)
	if offset != 0 {
		t.Errorf("offset = %d, want 0", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestParsePagination_OffsetOnly(t *testing.T) {
	r := httptest.NewRequest("GET", "/api/files?offset=5", nil)
	offset, limit := parsePagination(r)
	if offset != 5 {
		t.Errorf("offset = %d, want 5", offset)
	}
	if limit != 1000 {
		t.Errorf("limit = %d, want 1000", limit)
	}
}

func TestSortFileEntries(t *testing.T) {
	t.Parallel()
	entries := []FileInfo{
		{Name: "b.txt", Size: 100, ModTime: 1000},
		{Name: "a.txt", Size: 200, ModTime: 500},
		{Name: "c.txt", Size: 50, ModTime: 2000},
	}

	// 按 name 升序
	sorted := make([]FileInfo, len(entries))
	copy(sorted, entries)
	sortFileEntries(sorted, "name", "asc")
	if sorted[0].Name != "a.txt" || sorted[2].Name != "c.txt" {
		t.Errorf("name asc: expected a,b,c got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}

	// 按 name 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "name", "desc")
	if sorted[0].Name != "c.txt" || sorted[2].Name != "a.txt" {
		t.Errorf("name desc: expected c,b,a got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}

	// 按 size 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "size", "asc")
	if sorted[0].Size != 50 || sorted[2].Size != 200 {
		t.Errorf("size asc: expected 50,100,200 got %d,%d,%d", sorted[0].Size, sorted[1].Size, sorted[2].Size)
	}

	// 按 size 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "size", "desc")
	if sorted[0].Size != 200 || sorted[2].Size != 50 {
		t.Errorf("size desc: expected 200,100,50 got %d,%d,%d", sorted[0].Size, sorted[1].Size, sorted[2].Size)
	}

	// 按 time 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "time", "asc")
	if sorted[0].ModTime != 500 || sorted[2].ModTime != 2000 {
		t.Errorf("time asc: expected 500,1000,2000 got %d,%d,%d", sorted[0].ModTime, sorted[1].ModTime, sorted[2].ModTime)
	}

	// 按 time 降序
	copy(sorted, entries)
	sortFileEntries(sorted, "time", "desc")
	if sorted[0].ModTime != 2000 || sorted[2].ModTime != 500 {
		t.Errorf("time desc: expected 2000,1000,500 got %d,%d,%d", sorted[0].ModTime, sorted[1].ModTime, sorted[2].ModTime)
	}

	// 默认按 name 升序
	copy(sorted, entries)
	sortFileEntries(sorted, "", "")
	if sorted[0].Name != "a.txt" || sorted[2].Name != "c.txt" {
		t.Errorf("default: expected a,b,c got %s,%s,%s", sorted[0].Name, sorted[1].Name, sorted[2].Name)
	}
}

// ---- 只读面域级用例（环境见 dirs_test.go） ----

// inflightName 构造一个**合法**的在途临时文件名（形态由 IsInflightTempName 定义：
// `.inflight-<16hex>-<uploadID>.part`）。
const inflightName = ".inflight-0123456789abcdef-up123.part"

// readReq 构造只读面请求（actor 空 = 未认证 → anonymous；与 dirsEnv.post 同约定）。
func readReq(actor, method, target string) *http.Request {
	req := httptest.NewRequest(method, target, nil)
	if actor != "" {
		req.Header.Set("X-Test-Actor", actor)
	}
	return req
}

// serve 直接驱动域处理器（不经 pkg/server 装配）。
func (e *dirsEnv) serve(h http.HandlerFunc, actor, method, target string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h(rr, readReq(actor, method, target))
	return rr
}

// writeUserFile 在 owner 的用户桶内写文件（按需建中间目录）。
func writeUserFile(t *testing.T, env *dirsEnv, owner, rel, content string) string {
	t.Helper()
	abs := filepath.Join(env.root, owner, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", abs, err)
	}
	return abs
}

// decodeList 解析列表响应。
func decodeList(t *testing.T, rr *httptest.ResponseRecorder) ListResponse {
	t.Helper()
	var out ListResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应体不是合法 JSON（%v）: %s", err, rr.Body.String())
	}
	return out
}

// findEntry 在条目列表中按名查找。
func findEntry(files []FileInfo, name string) (FileInfo, bool) {
	for _, f := range files {
		if f.Name == name {
			return f, true
		}
	}
	return FileInfo{}, false
}

// TestService_ListFiles_AttachesChecksumAndHidesInflightTemp 覆盖单卷（VolSet nil）列表：
// 目录条目 IsDir、文件条目带 checksum（取自 per-tenant 台账，key = "user/<name>"）、
// 在途临时文件不列出（任务 8 O-2 语义）。
func TestService_ListFiles_AttachesChecksumAndHidesInflightTemp(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/a.txt", "AAA")
	if err := os.MkdirAll(filepath.Join(env.root, "alice", "user", "dir"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeUserFile(t, env, "alice", "user/"+inflightName, "PARTIAL")

	cs := env.checksumStoreFor("alice")
	if cs == nil {
		t.Fatal("checksumStoreFor(alice) 应为非 nil")
	}
	cs.Set("user/a.txt", sha256Hex([]byte("AAA")))

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 || len(resp.Files) != 2 {
		t.Fatalf("应只列出 1 文件 + 1 目录（在途临时文件不可见），got total=%d files=%+v", resp.Total, resp.Files)
	}
	if _, ok := findEntry(resp.Files, inflightName); ok {
		t.Fatalf("在途临时文件不应出现在列表中: %+v", resp.Files)
	}
	fi, ok := findEntry(resp.Files, "a.txt")
	if !ok {
		t.Fatalf("应列出 a.txt: %+v", resp.Files)
	}
	if fi.IsDir || fi.Size != 3 || fi.Checksum != sha256Hex([]byte("AAA")) {
		t.Fatalf("a.txt 条目=%+v want {IsDir:false Size:3 Checksum:%s}", fi, sha256Hex([]byte("AAA")))
	}
	d, ok := findEntry(resp.Files, "dir")
	if !ok || !d.IsDir {
		t.Fatalf("应列出目录条目 dir: %+v", resp.Files)
	}
}

// TestService_ListFiles_MultiVolumeAggregatesVolumeField 覆盖多卷聚合列表：逐卷聚合且
// 文件条目带各自卷名；?volume= 指定不在视图的卷名 → 404（fail-closed，不泄卷存在性）。
func TestService_ListFiles_MultiVolumeAggregatesVolumeField(t *testing.T) {
	env := newDirsEnv(t)
	env.enableVolumes(t, "main", "disk2")
	writeUserFile(t, env, "alice", "user/m.txt", "M")

	disk2User := filepath.Join(env.volDirs["disk2"], "alice", "user")
	if err := os.MkdirAll(disk2User, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(disk2User, "d.txt"), []byte("DD"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	rr := env.serve(env.svc.ListFiles, "alice", "GET", "/api/files")
	if rr.Code != http.StatusOK {
		t.Fatalf("列表应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("应聚合两卷共 2 个文件，got total=%d files=%+v", resp.Total, resp.Files)
	}
	m, ok := findEntry(resp.Files, "m.txt")
	if !ok || m.Volume != "main" {
		t.Fatalf("m.txt 条目=%+v want Volume=main", m)
	}
	d, ok := findEntry(resp.Files, "d.txt")
	if !ok || d.Volume != "disk2" {
		t.Fatalf("d.txt 条目=%+v want Volume=disk2", d)
	}

	rr = env.serve(env.svc.ListFiles, "alice", "GET", "/api/files?volume=nope")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("未知卷名应 404, got %d: %s", rr.Code, rr.Body.String())
	}
}

// TestService_Download_ServesRangeAndChecksumHeaders 覆盖整文件下载：Range 命中返回 206 +
// Content-Range 且内容正确；checksum 响应头取自 per-tenant 台账。
func TestService_Download_ServesRangeAndChecksumHeaders(t *testing.T) {
	env := newDirsEnv(t)
	const body = "0123456789"
	writeUserFile(t, env, "alice", "user/f.txt", body)
	tnt := env.tenantFor("alice")
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{Filename: r.URL.Query().Get("filename"), Tenant: tnt, Rel: "user/f.txt"}, nil
	}
	env.rebuild()

	cs := env.checksumStoreFor("alice")
	cs.Set("user/f.txt", sha256Hex([]byte(body)))

	req := readReq("alice", "GET", "/download?filename=f.txt")
	req.Header.Set("Range", "bytes=2-5")
	rr := httptest.NewRecorder()
	env.svc.Download(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("Range 请求应 206, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); got != "2345" {
		t.Fatalf("Range 内容=%q want %q", got, "2345")
	}
	if cr := rr.Header().Get("Content-Range"); cr != "bytes 2-5/10" {
		t.Fatalf("Content-Range=%q want bytes 2-5/10", cr)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd == "" {
		t.Fatal("应设置 Content-Disposition")
	}
	if ar := rr.Header().Get("Accept-Ranges"); ar != "bytes" {
		t.Fatalf("Accept-Ranges=%q want bytes", ar)
	}
	if cs := rr.Header().Get(headerFileChecksum); cs != sha256Hex([]byte(body)) {
		t.Fatalf("X-File-Checksum=%q want %q", cs, sha256Hex([]byte(body)))
	}
	if mt := rr.Header().Get(headerFileMTime); mt == "" {
		t.Fatal("应设置 X-File-MTime")
	}
}

// TestService_Download_MissingFileReturns404 覆盖下载的文件不存在路径：404 + JSON 外壳。
func TestService_Download_MissingFileReturns404(t *testing.T) {
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		return DownloadPath{Filename: "missing.txt", Tenant: tnt, Rel: "user/missing.txt"}, nil
	}
	env.rebuild()

	rr := env.serve(env.svc.Download, "alice", "GET", "/download?filename=missing.txt")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("缺失文件应 404, got %d: %s", rr.Code, rr.Body.String())
	}
	var resp UploadResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应体不是合法 JSON（%v）: %s", err, rr.Body.String())
	}
	if resp.Success || resp.Message != errMsgFileNotFound {
		t.Fatalf("响应=%+v want {false, %s}", resp, errMsgFileNotFound)
	}
}

// TestService_Stat_ReturnsMetadataHeaders 覆盖 stat 的元信息响应头与两个失败/边界分支：
// 目录带 X-File-IsDir；文件不存在 → 404 not found；解析错误（HTTPError）按其状态码回包。
func TestService_Stat_ReturnsMetadataHeaders(t *testing.T) {
	env := newDirsEnv(t)
	const body = "hello"
	writeUserFile(t, env, "alice", "user/f.txt", body)
	if err := os.MkdirAll(filepath.Join(env.root, "alice", "user", "sub"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	tnt := env.tenantFor("alice")
	env.resolveDownloadPath = func(r *http.Request) (DownloadPath, error) {
		name := r.URL.Query().Get("filename")
		if name == "boom.txt" {
			return DownloadPath{}, &HTTPError{Status: http.StatusTeapot, Message: "解析失败"}
		}
		return DownloadPath{Filename: name, Tenant: tnt, Rel: "user/" + name}, nil
	}
	env.rebuild()
	env.checksumStoreFor("alice").Set("user/f.txt", sha256Hex([]byte(body)))

	rr := env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=f.txt")
	if rr.Code != http.StatusOK {
		t.Fatalf("stat 应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("X-File-Size"); got != "5" {
		t.Fatalf("X-File-Size=%q want 5", got)
	}
	if got := rr.Header().Get(headerFileChecksum); got != sha256Hex([]byte(body)) {
		t.Fatalf("X-File-Checksum=%q want %q", got, sha256Hex([]byte(body)))
	}
	if got := rr.Header().Get(headerFileMTime); got == "" {
		t.Fatal("应设置 X-File-MTime")
	}

	rr = env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=sub")
	if rr.Code != http.StatusOK || rr.Header().Get("X-File-IsDir") != "true" {
		t.Fatalf("目录 stat 应 200 + X-File-IsDir, got %d headers=%v", rr.Code, rr.Header())
	}

	rr = env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=missing.txt")
	if rr.Code != http.StatusNotFound {
		t.Fatalf("缺失文件应 404, got %d", rr.Code)
	}

	rr = env.serve(env.svc.Stat, "alice", "HEAD", "/api/files/stat?filename=boom.txt")
	if rr.Code != http.StatusTeapot {
		t.Fatalf("HTTPError 应原样按其状态码回包, got %d", rr.Code)
	}
}

// TestService_SearchFiles_RecursiveMatchHidesInflightTemp 覆盖递归搜索：命中嵌套路径
// （名以 "/" 归一）、在途临时文件不参与；q 为空 → 400。
func TestService_SearchFiles_RecursiveMatchHidesInflightTemp(t *testing.T) {
	env := newDirsEnv(t)
	writeUserFile(t, env, "alice", "user/keep_me.txt", "1")
	writeUserFile(t, env, "alice", "user/sub/also_keep_me.txt", "2")
	writeUserFile(t, env, "alice", "user/"+inflightName, "3")
	writeUserFile(t, env, "alice", "user/other.txt", "4")

	rr := env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=keep")
	if rr.Code != http.StatusOK {
		t.Fatalf("搜索应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	resp := decodeList(t, rr)
	if resp.Total != 2 {
		t.Fatalf("应命中 2 个文件，got total=%d files=%+v", resp.Total, resp.Files)
	}
	if _, ok := findEntry(resp.Files, "keep_me.txt"); !ok {
		t.Fatalf("应命中根层 keep_me.txt: %+v", resp.Files)
	}
	if _, ok := findEntry(resp.Files, "sub/also_keep_me.txt"); !ok {
		t.Fatalf("应命中嵌套 sub/also_keep_me.txt（名以 / 归一）: %+v", resp.Files)
	}
	for _, f := range resp.Files {
		if f.Name == inflightName {
			t.Fatalf("在途临时文件不应参与搜索: %+v", resp.Files)
		}
	}

	rr = env.serve(env.svc.SearchFiles, "alice", "GET", "/api/files/search?q=")
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("空 q 应 400, got %d", rr.Code)
	}
}
