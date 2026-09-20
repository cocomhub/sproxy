// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmock

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// 本文件补上 syncmock 自身的测试（此前该包无测试，被 `make notest` 门禁漏过——那道门禁当时
// 因调用方未传参而空转）。动机很直接：**这个包是 8+ 个 sync/server 测试的地基**，
// 它自己行为不对（列表过滤、stat 头、下载 checksum、上传校验），上层测试会给出误导性的结论。

// headClient 用每测试一个独立 Transport：所有并行测试共享的 http.DefaultTransport
// 会被其他用例的 httptest.Server.Close 一并打断 idle 连接（表现为
// "transport connection broken: CloseIdleConnections called"，run 34908170952 实证）。
// 连接池 per-test 隔离，避免该类半途断连（与 FileClient 隔离连接池同一个对策）。
func isolatedClient() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: netutil.IsolatedTransport(), // 本测试自己的连接池
	}
}

func TestList_ReportsSeededFilesAndDirs(t *testing.T) {
	t.Parallel()
	srv, m := NewServer(t)
	m.SeedFile("a/b.txt", "hello")
	m.SeedDir("a/sub")

	hc := isolatedClient()
	resp, err := hc.Get(srv.URL + "/api/files?subdir=a")
	if err != nil {
		t.Fatalf("GET /api/files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d", resp.StatusCode)
	}
	var body struct {
		Files []listItem `json:"files"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	var sawFile, sawDir bool
	for _, it := range body.Files {
		switch it.Name {
		case "b.txt":
			sawFile = true
			if it.IsDir || it.Size != len("hello") || it.Checksum != SHA256Hex([]byte("hello")) {
				t.Errorf("文件条目不符: %+v", it)
			}
		case "sub":
			sawDir = true
			if !it.IsDir {
				t.Errorf("目录条目 IsDir 应为 true: %+v", it)
			}
		}
	}
	if !sawFile || !sawDir {
		t.Fatalf("列表应同时含 b.txt 与 sub，实际 %+v", body.Files)
	}
}

func TestStat_FileDirAndMissing(t *testing.T) {
	t.Parallel()
	srv, m := NewServer(t)
	m.SeedFile("x.txt", "abc")
	m.SeedDir("d")

	head := func(name string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodHead, srv.URL+"/api/files/stat?filename="+name, nil)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		resp, err := isolatedClient().Do(req)
		if err != nil {
			t.Fatalf("HEAD stat: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	if resp := head("x.txt"); resp.StatusCode != http.StatusOK ||
		resp.Header.Get("X-File-Size") != "3" ||
		resp.Header.Get("X-File-Checksum") != SHA256Hex([]byte("abc")) ||
		resp.Header.Get("X-File-IsDir") != "false" {
		t.Fatalf("文件 stat 头不符: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := head("d"); resp.StatusCode != http.StatusOK || resp.Header.Get("X-File-IsDir") != "true" {
		t.Fatalf("目录 stat 应 200 且 IsDir=true: %d %v", resp.StatusCode, resp.Header)
	}
	if resp := head("nope"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失文件 stat 应 404, got %d", resp.StatusCode)
	}
}

func TestDownload_ReturnsBodyAndChecksum(t *testing.T) {
	t.Parallel()
	srv, m := NewServer(t)
	m.SeedFile("f.bin", "payload")

	hc := isolatedClient()
	resp, err := hc.Get(srv.URL + "/download?filename=f.bin")
	if err != nil {
		t.Fatalf("GET /download: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != "payload" {
		t.Fatalf("body = %q", string(data))
	}
	if got := resp.Header.Get("X-File-Checksum"); got != SHA256Hex([]byte("payload")) {
		t.Fatalf("checksum 头 = %q", got)
	}

	missing, err := isolatedClient().Get(srv.URL + "/download?filename=none")
	if err != nil {
		t.Fatalf("GET /download (missing): %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("缺失文件下载应 404, got %d", missing.StatusCode)
	}
}

func TestUpload_RequiresChecksumAndVerifiesIt(t *testing.T) {
	t.Parallel()
	srv, _ := NewServer(t)

	post := func(checksum string, content string) *http.Response {
		t.Helper()
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		part, err := w.CreateFormFile("file", "up.txt")
		if err != nil {
			t.Fatalf("构造 multipart: %v", err)
		}
		if _, werr := part.Write([]byte(content)); werr != nil {
			t.Fatalf("写 multipart: %v", werr)
		}
		_ = w.Close()
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/upload", &buf)
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		if checksum != "" {
			req.Header.Set("X-File-Checksum", checksum)
		}
		resp, err := isolatedClient().Do(req)
		if err != nil {
			t.Fatalf("POST /upload: %v", err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	if resp := post("", "x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("缺 checksum 应 400, got %d", resp.StatusCode)
	}
	if resp := post(SHA256Hex([]byte("mismatch")), "x"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("checksum 不符应 400, got %d", resp.StatusCode)
	}
	if resp := post(SHA256Hex([]byte("good")), "good"); resp.StatusCode != http.StatusOK {
		t.Fatalf("合法上传应 200, got %d", resp.StatusCode)
	}
}

func TestSnapshotFiles_IsDeepCopy(t *testing.T) {
	t.Parallel()
	_, m := NewServer(t)
	m.SeedFile("a.txt", "one")

	snap := m.SnapshotFiles()
	snap["a.txt"].Data[0] = 'X'
	snap["b.txt"] = &RemoteFile{Data: []byte("injected")}

	again := m.SnapshotFiles()
	if got := string(again["a.txt"].Data); got != "one" {
		t.Fatalf("快照应深拷贝（原数据被污染）: %q", got)
	}
	if _, ok := again["b.txt"]; ok {
		t.Fatal("快照中新增的键不应回写 mock")
	}
}

func TestMkdirRenameDelete_RoundTrip(t *testing.T) {
	t.Parallel()
	srv, m := NewServer(t)
	m.SeedFile("old.txt", "data")

	post := func(url string, headers map[string]string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+url, strings.NewReader(""))
		if err != nil {
			t.Fatalf("构造请求: %v", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := isolatedClient().Do(req)
		if err != nil {
			t.Fatalf("POST %s: %v", url, err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp.StatusCode
	}

	cs := SHA256Hex([]byte("data"))
	if code := post("/rename?from=old.txt&to=new.txt", map[string]string{"X-File-Checksum": cs}); code != http.StatusOK {
		t.Fatalf("rename 应 200, got %d", code)
	}
	if _, ok := m.SnapshotFiles()["new.txt"]; !ok {
		t.Fatal("rename 后应存在 new.txt")
	}
	if code := post("/mkdir?dirname=d1", nil); code != http.StatusOK {
		t.Fatalf("mkdir 应 200, got %d", code)
	}
	if code := post("/delete?filename=new.txt", map[string]string{"X-File-Checksum": cs}); code != http.StatusOK {
		t.Fatalf("delete 应 200, got %d", code)
	}
	if _, ok := m.SnapshotFiles()["new.txt"]; ok {
		t.Fatal("delete 后不应存在 new.txt")
	}
}
