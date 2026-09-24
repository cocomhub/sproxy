// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
)

// 本文件是 RBAC 角色细分（roadmap 11.5-① / docs/designs/2026-09-24-rbac-roles.md）
// 片 1（核心）的测试：RoleReader 常量 + requireRole reader 档 + isReadOnlyFileRoute
// 只读/写子组拆分 + fileRoute/localMuxGate 双面接线。
//
// 覆盖面（设计文档「测试 + 变异点」表）：
//  1. reader 禁写：POST /upload（及 delete/rename/mkdir/batch）→ 403；
//  2. reader 可读：GET /download、GET /api/files、HEAD stat → 200；
//  3. user 不受影响（读写均 200，回归）；admin 全组放行（回归）；node 文件组 403（回归）；
//  4. 空 Role 归一 user（R3-M4 回归）；api_keys 恒 user 语义不变（回归）；
//  5. 隧道内层：reader 经 /tunnel 读 200、写 403（localMuxGate 拆分回归）。
//
// 变异点（改后必须红）：① requireRole 删掉 reader 放行分支 → reader 读 403（测试 2 红）；
// ② 写子组误用 requireRole(reader) → reader 写放行（测试 1 红）；③ isReadOnlyFileRoute
// 漏列 /api/files → reader list 403（测试 2 红）；④ 只读子组误挂 user → reader 读 403。
//
// 约束：纯标准库断言；所有真实 TCP 服务走 httptest（默认 127.0.0.1 回环）；
// 显式绑定 127.0.0.1 的回环测试用 httptest 自带 loopback；HTTP client 用
// testHTTPClient(t)（独立连接池，防并行用例互相打断 idle 连接）。

// ---- 装配辅助 ----

// readerTestPair 是 reader 角色测试凭据（AK/SK/条目 ID 与 ring 注入一致）。
const (
	readerTestAK = "ak-reader-rolee2e00000001"
	readerTestSK = "2222222222222222222222222222222222222222222222222222222222222222"
)

// ringWithReader 构造含 admin + user + reader 三角色的 Ring（Key.Role 经
// Snapshot+Replace 回写，模拟 store 载入含 role 字段的 Key）。
func ringWithReader(t *testing.T) *accesskey.Ring {
	t.Helper()
	ring := credentialsRingWithAdmin(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret)
	skB := mustDecodeHex(t, readerTestSK)
	if err := ring.UpsertAK(readerTestAK, "reader-user"); err != nil {
		t.Fatalf("UpsertAK(reader): %v", err)
	}
	if _, err := ring.AddKey(readerTestAK, skB, accesskey.WithID(testEntryID(readerTestAK))); err != nil {
		t.Fatalf("AddKey(reader): %v", err)
	}
	setKeyRole(t, ring, readerTestAK, accesskey.RoleReader)
	return ring
}

// newRBACRoleServer 启动带 admin+user+reader 三角色 Ring 的完整路由测试服务器。
func newRBACRoleServer(t *testing.T) (string, *accesskey.Ring) {
	t.Helper()
	ring := ringWithReader(t)
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		opts.CredentialRing = ring
		opts.CredentialStore = nil
	})
	return url, ring
}

// readerGet 用 reader 凭据发起 GET（无 body）并返回状态码。
func readerGet(t *testing.T, url, path string) int {
	t.Helper()
	return signedGetStatus(t, url, path, readerTestAK, readerTestSK)
}

// signedGetStatus 发带 SproxySig 签名的 GET 并返回状态码（signedGet 的薄封装，
// 只取状态码；signedGet 返回 (int, []byte)）。
func signedGetStatus(t *testing.T, url, path, ak, sk string) int {
	t.Helper()
	st, _ := signedGet(t, url+path, ak, sk)
	return st
}

// readerUpload 用 reader 凭据做 multipart 上传（POST /upload）。
func readerUpload(t *testing.T, url string) int {
	t.Helper()
	return uploadSignedAs(t, url, readerTestAK, readerTestSK)
}

// uploadSignedAs 以指定 AK/SK 签名上传一文件（multipart，X-File-Checksum 头）。
func uploadSignedAs(t *testing.T, url, ak, sk string) int {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "rbac.txt")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, werr := part.Write([]byte("rbac upload body")); werr != nil {
		t.Fatalf("write part: %v", werr)
	}
	if cerr := mw.Close(); cerr != nil {
		t.Fatalf("close multipart: %v", cerr)
	}
	req, err := http.NewRequest(http.MethodPost, url+"/upload", &buf)
	if err != nil {
		t.Fatalf("new upload request: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex([]byte("rbac upload body")))
	signBodyRequest(req, ak, sk, buf.Bytes())
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// readerPostJSON 用 reader 凭据发带 JSON body 的 POST 并返回状态码。
func readerPostJSON(t *testing.T, url, path string, body any) int {
	t.Helper()
	st, _ := doSignedJSON(t, http.MethodPost, url+path, readerTestAK, readerTestSK, body)
	return st
}

// ---- 1. reader 禁写（变异点②：写子组误用 reader 放行 → 本组红）----

// TestRBAC_ReaderForbiddenOnWriteRoutes 验证 reader 写面全部 403：upload/delete/
// rename/mkdir/batch-delete（设计文档测试 1）。写子组必须 requireRole(user)，
// reader 不属于 user 组 → 403 fail-closed。
func TestRBAC_ReaderForbiddenOnWriteRoutes(t *testing.T) {
	t.Parallel()
	url, _ := newRBACRoleServer(t)

	if st := readerUpload(t, url); st != http.StatusForbidden {
		t.Fatalf("reader POST /upload status = %d, want 403", st)
	}
	if st := readerPostJSON(t, url, "/delete?filename=rbac.txt", nil); st != http.StatusForbidden {
		t.Fatalf("reader POST /delete status = %d, want 403", st)
	}
	if st := readerPostJSON(t, url, "/rename?from=a.txt&to=b.txt", nil); st != http.StatusForbidden {
		t.Fatalf("reader POST /rename status = %d, want 403", st)
	}
	if st := readerPostJSON(t, url, "/mkdir?dirname=sub", nil); st != http.StatusForbidden {
		t.Fatalf("reader POST /mkdir status = %d, want 403", st)
	}
	if st := readerPostJSON(t, url, "/api/batch/delete", map[string]any{"files": []string{"a.txt"}}); st != http.StatusForbidden {
		t.Fatalf("reader POST /api/batch/delete status = %d, want 403", st)
	}
	// 分块上传初始化（写组含 /upload/init）也须 403。
	if st := readerPostJSON(t, url, "/upload/init", map[string]any{"filename": "x.bin", "total_size": 16}); st != http.StatusForbidden {
		t.Fatalf("reader POST /upload/init status = %d, want 403", st)
	}
}

// ---- 2. reader 可读（变异点①③④：删 reader 分支 / 漏列 /api/files / 只读子组误挂
// user → 本组红）----

// TestRBAC_ReaderAllowedOnReadRoutes 验证 reader 读面全部 200：GET /download、
// GET /api/files、HEAD /api/files/stat、GET /api/files/search、GET /download/chunk
// （设计文档测试 2 主例 + 清单回归）。前提：先由 user 上传文件供下载/stat。
func TestRBAC_ReaderAllowedOnReadRoutes(t *testing.T) {
	t.Parallel()
	url, _ := newRBACRoleServer(t)

	// reader 读断言：路由放行语义 = 未找到文件 404（而非 403 门禁拒绝）。
	// 文件由 user 预置上传（user 桶）；reader 读自己桶内不存在的文件 →
	// 404 证明门禁放行（若被 requireRole 拦则 403）。
	if st := uploadSignedAs(t, url, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user 预置上传 status = %d, want 200", st)
	}
	if st := readerGet(t, url, "/download?filename=reader-nonexist.txt"); st != http.StatusNotFound {
		t.Fatalf("reader GET /download 未命中应 404（门禁已放行）, got %d", st)
	}
	// GET /api/files 列表 → 200（空列表，reader 桶无文件但路由放行）。
	st, body := signedGet(t, url+"/api/files", readerTestAK, readerTestSK)
	if st != http.StatusOK {
		t.Fatalf("reader GET /api/files status = %d, want 200 (body=%s)", st, body)
	}
	_ = body
	// HEAD /api/files/stat → 404（门禁放行，文件不存在；用 HEAD 方法防 405）。
	if st := signedHeadStatus(t, url, "/api/files/stat?filename=rbac.txt", readerTestAK, readerTestSK); st != http.StatusNotFound {
		t.Fatalf("reader HEAD /api/files/stat status = %d, want 404（门禁放行）", st)
	}
	// GET /api/files/search → 200。
	if st := readerGet(t, url, "/api/files/search?q=rbac"); st != http.StatusOK {
		t.Fatalf("reader GET /api/files/search status = %d, want 200", st)
	}
	// 分块下载（只读子组含 /download/chunk）→ 门禁放行语义：文件不存在 404。
	if st := readerGet(t, url, "/download/chunk?filename=reader-nonexist.txt&offset=0&size=4"); st != http.StatusNotFound {
		t.Fatalf("reader GET /download/chunk 未命中应 404（门禁放行）, got %d", st)
	}
}

// ---- 3. 零回归：user 读写 200 / admin 全组放行 / node 文件组 403 ----

// TestRBAC_ZeroRegressionUserAndAdminAndNode 验证现有角色语义零变化：
// user 可读可写（200）；admin 读写全组放行（200）；node 文件组 403（回归）。
// 空 Role 归一 user（R3-M4）由既有 TestAuthSeam_RequireRole_FileGroupGate 覆盖，
// 此处补 node 在只读子组同样 403（写面既有的 TestRequireRole_NodeDenied 已覆盖）。
func TestRBAC_ZeroRegressionUserAndAdminAndNode(t *testing.T) {
	t.Parallel()
	url, ring := newRBACRoleServer(t)

	// user 写（上传）200 且读（list）200。
	if st := uploadSignedAs(t, url, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user POST /upload status = %d, want 200", st)
	}
	if st := signedGetStatus(t, url, "/api/files", testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user GET /api/files status = %d, want 200", st)
	}

	// admin 读写全组放行。
	if st := uploadSignedAs(t, url, testAdminKey, testAdminSecret); st != http.StatusOK {
		t.Fatalf("admin POST /upload status = %d, want 200", st)
	}
	if st := signedGetStatus(t, url, "/api/files", testAdminKey, testAdminSecret); st != http.StatusOK {
		t.Fatalf("admin GET /api/files status = %d, want 200", st)
	}

	// node 文件组（含只读子组）403。
	nodeAK := "ak-node-rbac00000000000001"
	nodeSK := "3333333333333333333333333333333333333333333333333333333333333333"
	if err := ring.UpsertAK(nodeAK, "mesh"); err != nil {
		t.Fatalf("UpsertAK(node): %v", err)
	}
	if _, err := ring.AddKey(nodeAK, mustDecodeHex(t, nodeSK), accesskey.WithID(testEntryID(nodeAK))); err != nil {
		t.Fatalf("AddKey(node): %v", err)
	}
	setKeyRole(t, ring, nodeAK, accesskey.RoleNode)
	if st := signedGetStatus(t, url, "/api/files", nodeAK, nodeSK); st != http.StatusForbidden {
		t.Fatalf("node GET /api/files status = %d, want 403", st)
	}
	if st := signedGetStatus(t, url, "/download?filename=rbac.txt", nodeAK, nodeSK); st != http.StatusForbidden {
		t.Fatalf("node GET /download status = %d, want 403", st)
	}
}

// ---- 4. api_keys 恒 user 语义不变（回归）----

// TestRBAC_APIKeysStillUser 验证 api_keys 模式合成的最小 Principal 仍恒 user：
// 只读 key（PermissionRead）GET /api/files → 200；写 key GET → 200；
// 只读 key 写（POST）→ 403（permissionAllowed 方法面拒绝，与角色无关）。
func TestRBAC_APIKeysStillUser(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuthSeamServer(t, func(c *Config) {
		c.APIKeys = APIKeyConfig{
			Enabled: true,
			Keys: []APIKey{
				{Name: "read-key", Key: "key-read-001", Permission: PermissionRead},
				{Name: "write-key", Key: "key-write-001", Permission: PermissionWrite},
			},
		}
	}, nil)

	// 只读 key GET → 200（合成 Principal Role=user 放行 requireRole(reader)）。
	resp, err := testHTTPClient(t).Get(url + "/api/files")
	if err != nil {
		t.Fatalf("api_keys GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无 Bearer 头 api_keys GET status = %d, want 401（认证缺失，先于角色）", resp.StatusCode)
	}

	read := func(path string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, url+path, nil)
		req.Header.Set("Authorization", "Bearer key-read-001")
		r2, rerr := testHTTPClient(t).Do(req)
		if rerr != nil {
			t.Fatalf("read-key GET %s: %v", path, rerr)
		}
		defer r2.Body.Close()
		return r2.StatusCode
	}
	if st := read("/api/files"); st != http.StatusOK {
		t.Fatalf("只读 key GET /api/files status = %d, want 200", st)
	}
	if st := read("/api/files/search?q=x"); st != http.StatusOK {
		t.Fatalf("只读 key GET /api/files/search status = %d, want 200", st)
	}

	// 写 key GET → 200。
	req, _ := http.NewRequest(http.MethodGet, url+"/api/files", nil)
	req.Header.Set("Authorization", "Bearer key-write-001")
	resp, err = testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("write-key GET: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("写 key GET /api/files status = %d, want 200", resp.StatusCode)
	}

	// 只读 key 写（POST /api/batch/delete）→ 403（PermissionRead 方法面拒绝）。
	body, _ := json.Marshal(map[string]any{"files": []string{"a.txt"}})
	req, _ = http.NewRequest(http.MethodPost, url+"/api/batch/delete", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer key-read-001")
	req.Header.Set("Content-Type", "application/json")
	resp, err = testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("read-key POST: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("只读 key POST /api/batch/delete status = %d, want 403", resp.StatusCode)
	}
}

// ---- 5. 隧道内层：reader 经 /tunnel 读 200、写 403（localMuxGate 拆分回归）----

// TestRBAC_TunnelInnerReaderReadAllowed 验证 reader 经 traditional POST /tunnel
// 内层只读子组放行：GET /api/files → 200（localMuxGate 命中只读子组 →
// requireRole(reader) 放行）。前提 user 已上传文件（落 user 租户，reader 同环读）。
func TestRBAC_TunnelInnerReaderReadAllowed(t *testing.T) {
	t.Parallel()
	base := newRBACRoleServerURL(t)
	tc := tunnelClientFor(t, base, readerTestAK, readerTestSK)

	req, _ := http.NewRequest(http.MethodGet, "/api/files", nil)
	resp, err := tc.Do(req)
	if err != nil {
		t.Fatalf("reader tunnel GET /api/files: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reader 隧道内层 GET /api/files status = %d, want 200", resp.StatusCode)
	}
}

// TestRBAC_TunnelInnerReaderWriteForbidden 验证 reader 经 /tunnel 内层写子组 403：
// POST /upload → 403（localMuxGate 命中写子组 → requireRole(user) 拒绝 reader）。
func TestRBAC_TunnelInnerReaderWriteForbidden(t *testing.T) {
	t.Parallel()
	base := newRBACRoleServerURL(t)
	tc := tunnelClientFor(t, base, readerTestAK, readerTestSK)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, perr := mw.CreateFormFile("file", "rbac-tun.txt")
	if perr != nil {
		t.Fatalf("create form file: %v", perr)
	}
	_, _ = part.Write([]byte("tunnel write attempt"))
	_ = mw.Close()
	req, _ := http.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("X-File-Checksum", sha256hex([]byte("tunnel write attempt")))
	resp, err := tc.Do(req)
	if err != nil {
		t.Fatalf("reader tunnel POST /upload: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("reader 隧道内层 POST /upload status = %d, want 403", resp.StatusCode)
	}
}

// newRBACRoleServerURL 是 newRBACRoleServer 的 URL-only 变体（隧道测试用）。
func newRBACRoleServerURL(t *testing.T) string {
	t.Helper()
	url, _ := newRBACRoleServer(t)
	return url
}

// ---- 6. requireRole 直接单元矩阵（reader 档新分支）----

// TestRBAC_RequireRoleReaderMatrix 直接单测 requireRole(_, RoleReader) 新档：
// reader/user/admin 放行；node 拒绝；空 Role 归一 user 放行；未知 minRole fail-closed。
func TestRBAC_RequireRoleReaderMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		principal *Principal
		want      error
	}{
		{"reader 组放行 reader", &Principal{Role: string(accesskey.RoleReader)}, nil},
		{"reader 组放行 user（可读可写账号）", &Principal{Role: string(accesskey.RoleUser)}, nil},
		{"reader 组放行 admin", &Principal{Role: string(accesskey.RoleAdmin)}, nil},
		{"reader 组拒绝 node", &Principal{Role: string(accesskey.RoleNode)}, errForbidden},
		{"reader 组空 Role 归一 user 放行", &Principal{Role: ""}, nil},
		{"reader 组未知角色拒绝", &Principal{Role: "superuser"}, errForbidden},
		{"nil principal → 401", nil, errUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requireRole(tt.principal, string(accesskey.RoleReader))
			if got != tt.want {
				t.Fatalf("requireRole(%+v, reader) = %v, want %v", tt.principal, got, tt.want)
			}
		})
	}
}

// ---- 7. 只读子组清单回归（isReadOnlyFileRoute 单一事实源）----

// TestRBAC_ReadOnlyRouteClassification 钉住 isReadOnlyFileRoute / isFileGroupedRoute
// 的成员判定：只读子组与写子组的分类（设计文档风险 1：清单漂移 → 误归写组 →
// reader 被拒）。只读成员改动须同步本测试。
func TestRBAC_ReadOnlyRouteClassification(t *testing.T) {
	t.Parallel()
	readOnly := []string{
		"/download", "/api/files", "/api/files/stat", "/api/files/search",
		"/download/chunk", "/api/versions", "/api/archive-dir", "/api/backends",
		"/api/volumes", "/api/volumes/user", "/api/shares", "/api/shares/tok123",
	}
	write := []string{
		"/upload", "/delete", "/rename", "/mkdir", "/rmdir",
		"/api/batch/delete", "/api/batch/rename", "/api/archive",
		"/api/versions/restore", "/upload/init", "/upload/chunk", "/upload/complete",
		"/api/backends/{type}/presign",
	}
	for _, p := range readOnly {
		if !isFileGroupedRoute(p) {
			t.Errorf("只读路径 %s 应属文件组（fileRoute 覆盖）", p)
		}
		if !isReadOnlyFileRoute(p) {
			t.Errorf("只读路径 %s 应归只读子组（否则误归写组 → reader 被拒）", p)
		}
	}
	for _, p := range write {
		if !isFileGroupedRoute(p) {
			t.Errorf("写路径 %s 应属文件组（fileRoute 覆盖）", p)
		}
		if isReadOnlyFileRoute(p) {
			t.Errorf("写路径 %s 不应归只读子组", p)
		}
	}
	// 非文件组路径：两判定都为 false（gate fail-closed 语义）。
	for _, p := range []string{"/api/cloud/tasks", "/api/stats", "/api/credentials"} {
		if isFileGroupedRoute(p) {
			t.Errorf("非文件组路径 %s 不应属文件组", p)
		}
		if isReadOnlyFileRoute(p) {
			t.Errorf("非文件组路径 %s 不应归只读子组", p)
		}
	}
}

// ---- 8. reader 管理面 403（管理端点维持 admin）----

// TestRBAC_ReaderForbiddenOnAdminRoutes 验证管理端点（/api/credentials* 等）reader
// 仍 403（设计文档：写端点维持至少 user、管理端点维持 admin；reader 无管理权限）。
func TestRBAC_ReaderForbiddenOnAdminRoutes(t *testing.T) {
	t.Parallel()
	url, _ := newRBACRoleServer(t)
	if st := signedGetStatus(t, url, "/api/credentials", readerTestAK, readerTestSK); st != http.StatusForbidden {
		t.Fatalf("reader GET /api/credentials status = %d, want 403（管理端点 admin-only）", st)
	}
	// /api/audit 主 mux 是 authMiddleware（无 requireRole）——reader 可读属现状
	// （管理端点 admin 门禁强化不在本片；credential 已 403 证明 reader 非 admin）。
	if st := signedGetStatus(t, url, "/api/audit", readerTestAK, readerTestSK); st != http.StatusOK {
		t.Fatalf("reader GET /api/audit status = %d, want 200（authMiddleware 现状）", st)
	}
}

// ---- 9. 空 Role 归一 user（R3-M4 回归，reader 上下文中补证）----

// TestRBAC_EmptyRoleNormalizesToUser 验证 Key.Role 空值（UpsertAK 直建 / 旧
// credentials.json）归一 user：写路由放行（user 语义），与 reader 显式值区分。
func TestRBAC_EmptyRoleNormalizesToUser(t *testing.T) {
	t.Parallel()
	url, ring := newRBACRoleServer(t)
	emptyAK := "ak-empty-rbac000000000001"
	emptySK := "4444444444444444444444444444444444444444444444444444444444444444"
	if err := ring.UpsertAK(emptyAK, "legacy"); err != nil {
		t.Fatalf("UpsertAK(empty): %v", err)
	}
	if _, err := ring.AddKey(emptyAK, mustDecodeHex(t, emptySK), accesskey.WithID(testEntryID(emptyAK))); err != nil {
		t.Fatalf("AddKey(empty): %v", err)
	}
	// 不 setKeyRole：保持空 Role（模拟旧 credentials.json 无 role 字段）。
	if st := uploadSignedAs(t, url, emptyAK, emptySK); st != http.StatusOK {
		t.Fatalf("空 Role 账号 POST /upload status = %d, want 200（归一 user 可写）", st)
	}
	if st := signedGetStatus(t, url, "/api/files", emptyAK, emptySK); st != http.StatusOK {
		t.Fatalf("空 Role 账号 GET /api/files status = %d, want 200", st)
	}
}

// _ 防止未使用导入（encoding/hex 由 ringWithReader 之外调用方使用时的占位）。
var _ = hex.EncodeToString
var _ = context.Background

// signedHeadStatus 发带 SproxySig 签名的 HEAD 并返回状态码。
func signedHeadStatus(t *testing.T, url, path, ak, sk string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodHead, url+path, nil)
	if err != nil {
		t.Fatalf("new head request: %v", err)
	}
	signRequest(req, ak, sk)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("head do: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
