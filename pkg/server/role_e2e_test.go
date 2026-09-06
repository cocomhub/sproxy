// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/client"
)

// roleE2ERegistered 是 register 响应的前端解析结果（与 registeredPair 异源豆，
// 只填本文件消费的字段，避免与服务端响应结构耦合）。
type roleE2ERegistered struct {
	AK     string `json:"ak"`
	SK     string `json:"sk"`
	SkeyID string `json:"skey_id"`
}

// ---- 任务⑤ 4B-1 全链路黑盒测试（httptest + 真实 FileClient SproxySig 签名）----
//
// 说明：下列场景中「简单注册→session 请求 200」「首 admin」「角色门禁」
// 「loopback 门禁」已在 task③ register_handler_test.go 覆盖——本文件只补 task③
// **未覆盖**的全链路缺口（每处注释标注）：
//
//  1. TestRegisterSimple_FullChain：注册后用**真实 pkg/client.FileClient**（WithAccessKey
//     + WithAccessKeyID，走 ConfigSigner 客户端完整签名/发送路径）请求 GET /api/files、
//     上传一文件并再次列目录——全链路（HTTP 面真实签名 → RingAuthenticator 验签 →
//     requireRole 放行 → 会话 AK 租户落桶）闭合。
//  2. TestAdminRole_PersistAfterRestart：Role 持久化——注册持久化经 CredentialStore
//     落盘 → 重新装配（boot），重启后首个注册用户 getRole()==admin。
//  3. TestAdminRole_AKDeleteRejected_FullChain：I6 e2e——DELETE 目标是
//     Key.Role==admin 的 AK（confirm+force 齐）→ 400 且 body 含「admin」提示。
//  4. TestRoleGate_NodeForbidden_FileClient：requireRole——Role==node 的 AK 经真实
//     FileClient 请求 GET /api/files → 403；user/admin → 200。
//
// 约束：纯标准库（依托仓库既有 pkg/client 自身链）；127.0.0.1 loopback；-race 全绿。

// newRoleE2EServer 启动带显式凭据 Ring + store 的真实 TCP 服务（httptest.NewServer）。
// 返回 URL 与 cfgPtr；凭据用 sessionAK 提供的命令注册决定。
func newRoleE2EServer(t *testing.T, ring *accesskey.Ring, store *CredentialStore) (string, *atomic.Pointer[Config]) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.ChunkSize = 4 << 10
	cfg.LogLevel = "error"
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)

	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "v",
		BuildAt:        "t",
		Logger:         testLogger(),
		CredentialRing: ring,
	}
	if store != nil {
		opts.CredentialStore = store
	}

	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })

	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, &cfgPtr
}

// registerViaRealHTTP 用真实 HTTP 客户端注册（RemoteAddr 天然来自回环）并返回注册响应。
func registerViaRealHTTP(t *testing.T, url string) roleE2ERegistered {
	t.Helper()
	resp, err := http.Post(url+"/api/credentials/register", "application/json", strings.NewReader(`{"owner":"role-e2e"}`))
	if err != nil {
		t.Fatalf("register POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("register status = %d", resp.StatusCode)
	}
	var out struct {
		AK     string `json:"ak"`
		SK     string `json:"sk"`
		SkeyID string `json:"skey_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	if out.SK == "" || out.AK == "" || out.SkeyID == "" {
		t.Fatalf("register 响应字段缺失: %+v", out)
	}
	return roleE2ERegistered{AK: out.AK, SK: out.SK, SkeyID: out.SkeyID}
}

// newRegisteredClient 构造携带注册下发凭据、走 ConfigSigner 真实签名的 FileClient。
func newRegisteredClient(url string, reg roleE2ERegistered) *client.FileClient {
	return client.NewFileClient(url,
		client.WithAccessKey(reg.AK, reg.SK),
		client.WithAccessKeyID(reg.SkeyID),
	)
}

// ---- 1. 简单注册 → 真实 FileClient 会话请求 200 ----

// TestRegisterSimple_FullChain 是全链路收口：注册（回环，拿 ak+sk+skey_id）→ 用
// 真实 pkg/client.FileClient（SproxySig ConfigSigner 完整签名路径）GET /api/files
// → 200；随后 `Upload` 一文件并再次 List → 列表含该文件名（会话 AK 租户落桶）。
// task③ 已用 handler 直发签名请求覆盖「注册后 GET 200」，本测试补**真实客户端
// SDK 发送 + 响应契约 + 上传全链路**这层（新断言）。响应中 files 为 null（空用户桶）
// 也视为 200 可达——端点语义是「列目录成功」，null vs [] 非本测试关注点。
func TestRegisterSimple_FullChain(t *testing.T) {
	url, _ := newRoleE2EServer(t, accesskey.NewRing(), nil)
	reg := registerViaRealHTTP(t, url)
	c := newRegisteredClient(url, reg)

	if _, err := c.List(context.Background()); err != nil {
		t.Fatalf("注册会话 GET /api/files: %v", err)
	}

	srcDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "hello.txt")
	if err := os.WriteFile(srcPath, []byte("role-e2e upload"), 0o644); err != nil {
		t.Fatalf("写本地文件: %v", err)
	}
	up, err := c.Upload(context.Background(), srcPath, "hello.txt")
	if err != nil {
		t.Fatalf("注册会话 Upload: %v", err)
	}
	if !up.Success {
		t.Fatalf("Upload.Success = false: %s", up.Message)
	}

	files, err := c.List(context.Background())
	if err != nil {
		t.Fatalf("注册会话 List(after upload): %v", err)
	}
	found := false
	for _, f := range files {
		if f.Name == "hello.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("上传后列表应含 hello.txt，实际 %+v", files)
	}
}

// ---- 2. Role 持久化重启 ----

// TestAdminRole_PersistAfterRestart 验证 DEC-A Role 持久化：注册成功即持久化
// （credential_store.Save 落盘）→ 用同一 store 重建 Ring 重新装配（模拟重启，
// bootstrapCredentials 从 store 载入 Role 字段）→ getRole()==admin。
// 关键前提：credentials.json 的 Key.Role 由 register 经 AddRegistration 写入且随
// Snapshot 落盘；reload 回读后 getRole 判定（R3-M4：空 Role 归一 user 正向不适用）。
func TestAdminRole_PersistAfterRestart(t *testing.T) {
	tmpDir := t.TempDir()
	store := NewCredentialStore(filepath.Join(tmpDir, "anonymous", "meta"))
	ring := accesskey.NewRing()

	url, cfgPtr := newRoleE2EServer(t, ring, store)
	reg := registerViaRealHTTP(t, url)
	k, ok := ring.GetKey(reg.AK)
	if !ok || k.Role != accesskey.RoleAdmin {
		t.Fatalf("注册后 Role = %+v（应为 admin）", k)
	}

	// 重新装配（store 已落盘）：新 Ring 从 store 载入。
	ring2 := accesskey.NewRing()
	keys, err := store.Load()
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if len(keys) == 0 {
		t.Fatalf("重启后 store 不应为空（注册已持久化）")
	}
	if err := ring2.Replace(keys); err != nil {
		t.Fatalf("ring2.Replace: %v", err)
	}
	h := &Handlers{cfgPtr: cfgPtr, credentialRing: ring2, logger: testLogger()}
	if got := h.getRole(reg.AK); got != "admin" {
		t.Fatalf("重启后 getRole = %q, want admin（Role 持久化）", got)
	}
}

// ---- 3. I6：admin AK 不可删除（e2e，含 body 文案）----

// TestAdminRole_AKDeleteRejected_FullChain 验证 I6 的黑盒形态（task③ 已断言 400 状态码，
// 未断言 body 文案——本测试补齐）：DELETE /api/credentials/{ak} 目标是 Key.Role==admin
// 的 AK（confirm+force 齐）→ 400，且 body 含「admin 角色 AK 不可删除」。连环确认：
// confirm 不匹配也 400（拒绝门槛先于 admin 检查）。
func TestAdminRole_AKDeleteRejected_FullChain(t *testing.T) {
	ring := credentialsRingWithAdmin(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret)
	url, _ := newRoleE2EServer(t, ring, nil)

	// confirm 必须等于目标 AK：错误 confirm → 400（门槛在前，与 admin 检查解耦）。
	stWrong, bodyWrong := doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+testAdminKey, testAdminKey, testAdminSecret,
		map[string]any{"confirm": "wrong-ak", "force": true})
	if stWrong != http.StatusBadRequest {
		t.Fatalf("confirm 不匹配 status = %d, want 400 (body=%s)", stWrong, bodyWrong)
	}

	// confirm+force 齐 → 400（admin 角色 AK 不可删除）。
	st, body := doSignedJSON(t, http.MethodDelete, url+"/api/credentials/"+testAdminKey, testAdminKey, testAdminSecret,
		map[string]any{"confirm": testAdminKey, "force": true})
	if st != http.StatusBadRequest {
		t.Fatalf("删除 admin AK status = %d, want 400 (body=%s)", st, body)
	}
	if !strings.Contains(string(body), "admin") {
		t.Errorf("400 body 应提示 admin 角色不可删: %s", body)
	}
}

// ---- 4. requireRole：node 403 / user 200（真实 FileClient）----

// TestRoleGate_NodeForbidden_FileClient 验证 requireRole 门禁经真实客户端签名路径：
// Role==node 的 AK → FileClient.List → 403（错误中含 403）；user/admin → 200。
// task③ 用裸 signedGet 覆盖门禁，本测试通过完整客户端请求编排（canonical 含
// ?subdir= 查询串）再次闭环——钉住客户端签名产物被服务端验签接受 + 门禁交互。
// admin 凭据条目 ID 用 testEntryID 确定性生成（credentialsRingWithAdmin 同源）。
func TestRoleGate_NodeForbidden_FileClient(t *testing.T) {
	nodeAK := "ak-node-rolee2e0000000011"
	nodeSK := strings.Repeat("11", 32)
	ring := credentialsRingWithAdmin(t, testAdminKey, testAdminSecret, testAccessKey, testAccessSecret)
	_ = ring.UpsertAK(nodeAK, "mesh")
	_, _ = ring.AddKey(nodeAK, mustDecodeHex(t, nodeSK), accesskey.WithID(testEntryID(nodeAK)))
	setKeyRole(t, ring, nodeAK, accesskey.RoleNode)
	url, _ := newRoleE2EServer(t, ring, nil)

	nodeClient := client.NewFileClient(url,
		client.WithAccessKey(nodeAK, nodeSK),
		client.WithAccessKeyID(testEntryID(nodeAK)))
	if _, err := nodeClient.List(context.Background()); err == nil {
		t.Fatalf("node 访问 GET /api/files 应 403（requireRole fail-closed）")
	} else if !strings.Contains(err.Error(), "403") {
		t.Fatalf("node 403 错误应含 HTTP 状态码提示: %v", err)
	}

	for _, pair := range []struct {
		ak, sk string
	}{
		{testAccessKey, testAccessSecret},
		{testAdminKey, testAdminSecret},
	} {
		c := client.NewFileClient(url,
			client.WithAccessKey(pair.ak, pair.sk),
			client.WithAccessKeyID(testEntryID(pair.ak)))
		if _, err := c.List(context.Background()); err != nil {
			t.Fatalf("AK %s GET /api/files: %v", pair.ak, err)
		}
	}
}
