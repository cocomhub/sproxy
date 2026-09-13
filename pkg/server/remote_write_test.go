// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// remote_write_test.go 是 B 侧**写面**（Y 二期 P3-b）的域级测试：路由白名单、授权矩阵、
// 委派到 pkg/files 域方法的落盘效果、checksum 门禁、owner 不可由请求指定、审计留痕。
//
// 与只读面测试的分工一致：本文件不起真隧道（peerFingerprintProvider 用 fake 注入指纹），
// 传输与握手由 remote_write_listener_test.go（P3-b2）覆盖。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

// remoteWriteTestConfig 构造「单卷 + 一条 scope 可配的 mesh_readers」已校验 cfg。
// mode=allow 且 owners=[alice]：让 owner 本身过卷 ACL（第二重约束的第一半）。
func remoteWriteTestConfig(t *testing.T, scope string) *Config {
	t.Helper()
	cfg := remoteReadTestConfigACL(t, &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{testReaderOwner},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: testReaderNodeA, Fingerprint: testReaderFP, Owner: testReaderOwner, Scope: scope,
		}},
	})
	return cfg
}

// newRemoteWriteFixture 返回 (写面 handler, cfg, 审计缓冲)。scope 缺省 rw（写测试的常见前提）。
func newRemoteWriteFixture(t *testing.T, peerFP, scope string) (http.Handler, *Config, *bytes.Buffer) {
	t.Helper()
	cfg := remoteWriteTestConfig(t, scope)
	auditBuf := &bytes.Buffer{}
	h := newRemoteReadHandlers(t, cfg, auditBuf)
	return h.newRemoteWriteHandler(fakePeerFingerprint{fp: peerFP}), cfg, auditBuf
}

// doRemoteWrite 发一个写请求（body 非 nil 时带上；checksum 非空时带 X-File-Checksum）。
func doRemoteWrite(t *testing.T, h http.Handler, method, target string, body []byte, checksum string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rd)
	if checksum != "" {
		req.Header.Set(headerFileChecksum, checksum)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// diskUserFile 读 <volRoot>/<owner>/user/<rel> 的内容（不存在返回 ok=false）。
func diskUserFile(t *testing.T, cfg *Config, owner, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cfg.StorageRoot, owner, "user", filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// TestRemoteWrite_RouteWhitelistIsWriteOnly 钉住写面的**路由白名单**：
//   - 只有 4 个写 op 注册（POST）；
//   - 同一路径的其它方法 405（POST 限定）；
//   - **只读面的路径不在写面上**（404）——写面不是「读面 + 黑名单」，而是独立白名单。
func TestRemoteWrite_RouteWhitelistIsWriteOnly(t *testing.T) {
	h, _, _ := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)

	for _, target := range []string{
		"/remote/write?volume=main&path=a.txt",
		"/remote/rename?volume=main&from=a.txt&to=b.txt",
		"/remote/delete?volume=main&path=a.txt",
		"/remote/mkdir?volume=main&path=d",
	} {
		// 无 checksum/参数不全时的状态由后续用例钉；此处只要求「路由存在」= 不是 404/405。
		if rec := doRemoteWrite(t, h, http.MethodPost, target, nil, ""); rec.Code == http.StatusNotFound ||
			rec.Code == http.StatusMethodNotAllowed {
			t.Fatalf("%s 应已在写面注册（POST），got %d", target, rec.Code)
		}
	}

	// POST 限定：GET 同一路径应 405（路径已注册但方法不匹配）。
	if rec := doRemoteWrite(t, h, http.MethodGet, "/remote/write?volume=main&path=a.txt", nil, ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /remote/write 应 405（写面只注册 POST）, got %d", rec.Code)
	}

	// 只读路径不得出现在写面（独立白名单）。
	for _, target := range []string{"/remote/list", "/remote/stat", "/remote/download"} {
		if rec := doRemoteWrite(t, h, http.MethodPost, target+"?volume=main&path=/", nil, ""); rec.Code != http.StatusNotFound {
			t.Fatalf("只读路径 %s 不应出现在写面（应 404）, got %d", target, rec.Code)
		}
	}
	// 本地面路由也不在写面上。
	if rec := doRemoteWrite(t, h, http.MethodPost, "/upload", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("/upload 不应出现在远程写面（应 404）, got %d", rec.Code)
	}
}

// TestRemoteWrite_AuthorizationMatrix 钉住写面的授权矩阵：**scope 必须授予写**。
func TestRemoteWrite_AuthorizationMatrix(t *testing.T) {
	const target = "/remote/mkdir?volume=main&path=d"

	cases := []struct {
		name     string
		peerFP   string
		scope    string
		wantCode int
	}{
		{"scope=rw 放行", testReaderFP, volume.MeshScopeRW, http.StatusOK},
		{"scope=write 放行", testReaderFP, volume.MeshScopeWrite, http.StatusOK},
		{"scope=read 拒绝（读不隐含写）", testReaderFP, volume.MeshScopeRead, http.StatusNotFound},
		{"scope 缺省（=read）拒绝", testReaderFP, "", http.StatusNotFound},
		// 未知 scope 在**配置层**就被响亮拒绝（Config.Validate，见
		// config_mesh_readers_test.go 的 ScopeValidate），装配层再兜底丢弃条目
		// （ParseVolumeACLDropsInvalidScope）——故此处构造不出「未知 scope 到达 handler」
		// 的状态，本用例不重复造不可达前提。
		{"指纹未列入拒绝", "sha256:" + strings.Repeat("0", 64), volume.MeshScopeRW, http.StatusNotFound},
		{"空指纹（未认证）拒绝", "", volume.MeshScopeRW, http.StatusUnauthorized},
		{"卷不存在拒绝", testReaderFP, volume.MeshScopeRW, http.StatusNotFound}, // 见下：改 target
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, _ := newRemoteWriteFixture(t, tc.peerFP, tc.scope)
			tgt := target
			if tc.name == "卷不存在拒绝" {
				tgt = "/remote/mkdir?volume=nope&path=d"
			}
			rec := doRemoteWrite(t, h, http.MethodPost, tgt, nil, "")
			if rec.Code != tc.wantCode {
				t.Fatalf("状态=%d want %d (body=%q)", rec.Code, tc.wantCode, rec.Body.String())
			}
		})
	}

	// 缺 volume 参数：与只读面同语义（404，不泄露对象存在性）。
	h, _, _ := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/mkdir?path=d", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("缺 volume 应 404, got %d", rec.Code)
	}
}

// TestRemoteWrite_HappyPathDelegatesDomainOps 钉住写面**委派到 pkg/files 域方法**的落盘效果：
// mkdir → write（含 checksum 台账与响应 checksum）→ rename → delete 全链路。
func TestRemoteWrite_HappyPathDelegatesDomainOps(t *testing.T) {
	h, cfg, _ := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)
	const owner = testReaderOwner

	// mkdir
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/mkdir?volume=main&path=sub", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("mkdir 状态=%d body=%q", rec.Code, rec.Body.String())
	}
	if fi, err := os.Stat(filepath.Join(cfg.StorageRoot, owner, "user", "sub")); err != nil || !fi.IsDir() {
		t.Fatalf("目录未落盘: err=%v", err)
	}

	// write（body + checksum）
	const body = "remote-write-body"
	sum := sha256hex([]byte(body))
	rec := doRemoteWrite(t, h, http.MethodPost, "/remote/write?volume=main&path=sub/hello.txt", []byte(body), sum)
	if rec.Code != http.StatusOK {
		t.Fatalf("write 状态=%d body=%q", rec.Code, rec.Body.String())
	}
	var wr remoteWriteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &wr); err != nil {
		t.Fatalf("解析 write 响应: %v body=%q", err, rec.Body.String())
	}
	if !wr.Success || wr.Checksum != sum {
		t.Fatalf("write 响应=%+v want success+checksum=%q", wr, sum)
	}
	if got, ok := diskUserFile(t, cfg, owner, "sub/hello.txt"); !ok || got != body {
		t.Fatalf("文件未落盘或内容不符: ok=%v got=%q", ok, got)
	}

	// rename
	rec = doRemoteWrite(t, h, http.MethodPost,
		"/remote/rename?volume=main&from=sub/hello.txt&to=sub/moved.txt", nil, sum)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename 状态=%d body=%q", rec.Code, rec.Body.String())
	}
	if _, ok := diskUserFile(t, cfg, owner, "sub/hello.txt"); ok {
		t.Fatal("rename 后源仍在")
	}
	if got, ok := diskUserFile(t, cfg, owner, "sub/moved.txt"); !ok || got != body {
		t.Fatalf("rename 后目标不符: ok=%v got=%q", ok, got)
	}

	// delete（checksum 须匹配）
	rec = doRemoteWrite(t, h, http.MethodPost, "/remote/delete?volume=main&path=sub/moved.txt", nil, sum)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete 状态=%d body=%q", rec.Code, rec.Body.String())
	}
	if _, ok := diskUserFile(t, cfg, owner, "sub/moved.txt"); ok {
		t.Fatal("delete 后文件仍在")
	}
}

// TestRemoteWrite_ChecksumGate 钉住 checksum 门禁在远程面同样生效（域方法承担，不复制）：
// 缺 checksum 400；写内容与声明不符 400 且**不落盘**；删/改名 checksum 不符 400 且文件保留。
func TestRemoteWrite_ChecksumGate(t *testing.T) {
	h, cfg, _ := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)
	const owner = testReaderOwner
	const body = "gate-body"
	sum := sha256hex([]byte(body))

	// 缺 checksum 的写：400，不落盘
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/write?volume=main&path=a.txt", []byte(body), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 checksum 的写应 400, got %d", rec.Code)
	}
	if _, ok := diskUserFile(t, cfg, owner, "a.txt"); ok {
		t.Fatal("缺 checksum 的写不得落盘")
	}

	// 内容与声明不符：400，不落盘
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/write?volume=main&path=a.txt", []byte("other"), sum); rec.Code != http.StatusBadRequest {
		t.Fatalf("checksum 不符的写应 400, got %d", rec.Code)
	}
	if _, ok := diskUserFile(t, cfg, owner, "a.txt"); ok {
		t.Fatal("checksum 不符的写不得落盘")
	}

	// 正常写入后：删/改名缺 checksum → 400 且文件保留
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/write?volume=main&path=a.txt", []byte(body), sum); rec.Code != http.StatusOK {
		t.Fatalf("正常写状态=%d", rec.Code)
	}
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/delete?volume=main&path=a.txt", nil, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("删缺 checksum 应 400, got %d", rec.Code)
	}
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/rename?volume=main&from=a.txt&to=b.txt", nil, "deadbeef"); rec.Code != http.StatusBadRequest {
		t.Fatalf("改名 checksum 不符应 400, got %d", rec.Code)
	}
	if got, ok := diskUserFile(t, cfg, owner, "a.txt"); !ok || got != body {
		t.Fatalf("被拒的删/改名不得改动文件: ok=%v got=%q", ok, got)
	}
}

// TestRemoteWrite_OwnerFromConfigNotRequest 钉住红线：**owner 恒来自配置**，请求无法指定；
// 路径穿越仍被域方法的路径校验拒绝。
func TestRemoteWrite_OwnerFromConfigNotRequest(t *testing.T) {
	h, cfg, _ := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)
	const owner = testReaderOwner

	// 请求里塞 owner 参数不影响归属：文件仍落在配置的 owner 下。
	rec := doRemoteWrite(t, h, http.MethodPost,
		"/remote/mkdir?volume=main&path=owned&owner=bob", nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("mkdir 状态=%d", rec.Code)
	}
	if _, err := os.Stat(filepath.Join(cfg.StorageRoot, owner, "user", "owned")); err != nil {
		t.Fatalf("应落在配置 owner 下: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.StorageRoot, "bob")); err == nil {
		t.Fatal("不得在请求指定的 owner 下创建任何东西")
	}

	// 路径穿越被拒（域方法的 pathguard）。
	for _, target := range []string{
		"/remote/mkdir?volume=main&path=../bob/user/x",
		"/remote/write?volume=main&path=../../etc/passwd",
		"/remote/delete?volume=main&path=..%2Fbob%2Fuser%2Fsecret",
	} {
		rec := doRemoteWrite(t, h, http.MethodPost, target, []byte("x"), sha256hex([]byte("x")))
		if rec.Code < 400 || rec.Code >= 500 {
			t.Fatalf("穿越路径应 4xx, got %d (%s)", rec.Code, target)
		}
	}
}

// TestRemoteWrite_AuditRecordsActionAndStatus 钉住写面审计：放行与拒绝都留痕，action=mesh_write。
func TestRemoteWrite_AuditRecordsActionAndStatus(t *testing.T) {
	h, _, auditBuf := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRW)
	if rec := doRemoteWrite(t, h, http.MethodPost, "/remote/mkdir?volume=main&path=audited", nil, ""); rec.Code != http.StatusOK {
		t.Fatalf("mkdir 状态=%d", rec.Code)
	}
	if !strings.Contains(auditBuf.String(), "mesh_write") {
		t.Fatalf("放行应记 mesh_write 审计, got %q", auditBuf.String())
	}

	// 拒绝路径（scope=read）也必须留痕。
	hDeny, _, denyBuf := newRemoteWriteFixture(t, testReaderFP, volume.MeshScopeRead)
	if rec := doRemoteWrite(t, hDeny, http.MethodPost, "/remote/mkdir?volume=main&path=x", nil, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("拒绝应 404, got %d", rec.Code)
	}
	if !strings.Contains(denyBuf.String(), "mesh_write") {
		t.Fatalf("拒绝也应记 mesh_write 审计, got %q", denyBuf.String())
	}
}
