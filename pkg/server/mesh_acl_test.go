// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mesh_acl_test.go 钉住 `GET /api/mesh/acl`（W3）：**仅 owner 自身**可见的跨节点授权视图。
//
// 安全口径（用户 2026-09-13 决策）：调用者只能看到 `mesh_readers.owner == 自己 owner` 的条目；
// 别人的授权既不出现在列表里，也不通过计数泄露。故下面每个用例都**显式断言「别人的指纹不出现」**。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

const (
	aclOtherFP = "sha256:aa11bb22cc33dd44ee55ff6677889900aabbccddeeff00112233445566778899"
	aclThirdFP = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// meshACLTestConfig 造两卷：main 上 alice(rw)+bob(read)，share 上 alice(缺省 scope)+carol(rw)。
func meshACLTestConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.Volumes = []VolumeConfig{
		{Name: "main", Root: t.TempDir(), ACL: &VolumeACLConfig{
			Mode: VolumeACLDeny,
			MeshReaders: []VolumeMeshReaderConfig{
				{Node: "node-a", Fingerprint: testReaderFP, Owner: "alice", Scope: volume.MeshScopeRW},
				{Node: "node-b", Fingerprint: aclOtherFP, Owner: "bob", Scope: volume.MeshScopeRead},
			},
		}},
		{Name: "share", Root: t.TempDir(), ACL: &VolumeACLConfig{
			Mode: VolumeACLDeny,
			MeshReaders: []VolumeMeshReaderConfig{
				{Node: "node-c", Fingerprint: aclThirdFP, Owner: "alice"}, // 缺省 scope = read
				{Node: "node-d", Fingerprint: aclOtherFP, Owner: "carol", Scope: volume.MeshScopeWrite},
			},
		}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	return cfg
}

// TestMeshACL_OwnerScoped 钉住核心安全口径：只返回自己 owner 的条目（跨卷聚合），且顺序稳定。
func TestMeshACL_OwnerScoped(t *testing.T) {
	cfg := meshACLTestConfig(t)

	got := meshACLEntriesForOwner(cfg, "alice")
	if len(got) != 2 {
		t.Fatalf("alice 应看到 2 条（两卷各一），得到 %d: %+v", len(got), got)
	}
	if got[0].Volume != "main" || got[0].Node != "node-a" || got[0].Scope != volume.MeshScopeRW {
		t.Errorf("第 1 条不符（卷声明序）: %+v", got[0])
	}
	if got[1].Volume != "share" || got[1].Node != "node-c" {
		t.Errorf("第 2 条不符（卷声明序）: %+v", got[1])
	}
	// 缺省 scope 归一为 read（与 volume.NormalizeMeshScope 语义一致，显式值原样保留）。
	if got[1].Scope != volume.MeshScopeRead {
		t.Errorf("缺省 scope 应为 read，得到 %q", got[1].Scope)
	}
	// 别人的授权绝不出现（连计数都不泄露：总数必须恰为 2）。
	for _, e := range got {
		if e.Fingerprint == aclOtherFP || e.Node == "node-b" || e.Node == "node-d" {
			t.Fatalf("泄露了他 owner 的授权条目: %+v", e)
		}
	}
	// 其它 owner 视角：bob 只 1 条，carol 只 1 条，无人可见的 owner 为空。
	if n := len(meshACLEntriesForOwner(cfg, "bob")); n != 1 {
		t.Errorf("bob 应看到 1 条，得到 %d", n)
	}
	if n := len(meshACLEntriesForOwner(cfg, "carol")); n != 1 {
		t.Errorf("carol 应看到 1 条，得到 %d", n)
	}
	if n := len(meshACLEntriesForOwner(cfg, "dave")); n != 0 {
		t.Errorf("无授权的 owner 应看到 0 条，得到 %d", n)
	}
}

// newMeshACLHandlers 注册带测试凭据的 Handlers（与 /api/mesh/status 同款：主 mux + SproxySig）。
//
// 用 RegisterRoutes 而非裸 &Handlers{}：cfgPtr 是**指针**字段，裸结构体上 Store 会 nil panic；
// 且这样顺带证明「认证面可达 + actor→owner 口径生效」（测试凭据的 owner 恒为 testAccessKey）。
func newMeshACLHandlers(t *testing.T, cfg *Config) *Handlers {
	t.Helper()
	var cp atomic.Pointer[Config]
	cp.Store(cfg)
	opts := RegisterRoutesOpts{
		Mux:     http.NewServeMux(),
		CfgPtr:  &cp,
		Version: "test",
		BuildAt: "test",
		Logger:  testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// doMeshACL 发一次 GET /api/mesh/acl（可带查询串）并解析响应。
func doMeshACL(t *testing.T, h *Handlers, query string) MeshACLResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/mesh/acl"+query, nil)
	req.Body = http.NoBody // 真实服务端请求经 net/http 规范化后 Body 恒非 nil
	ak, sk, entryID, ok := h.SelfCredential()
	if !ok {
		t.Fatal("fixture 应带凭据")
	}
	signRequestEntry(req, ak, entryID, sk)
	rec := httptest.NewRecorder()
	h.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var out MeshACLResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v body=%s", err, rec.Body.String())
	}
	return out
}

// TestMeshACL_HandlerUsesActorOwner 钉住 handler 以「已认证 actor」为 owner 口径（不具备可绕过性）。
func TestMeshACL_HandlerUsesActorOwner(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode: VolumeACLDeny,
		MeshReaders: []VolumeMeshReaderConfig{
			{Node: "node-self", Fingerprint: testReaderFP, Owner: testAccessKey, Scope: volume.MeshScopeRW},
			{Node: "node-other", Fingerprint: aclOtherFP, Owner: "alice", Scope: volume.MeshScopeRead},
		},
	}}}
	cfg.SetDefaults()
	h := newMeshACLHandlers(t, cfg)

	// 带上「想冒充别人」的查询参数：必须无效（口径只认 actor）。
	resp := doMeshACL(t, h, "?owner=alice")
	if resp.Owner != testAccessKey {
		t.Errorf("owner = %q, want %q（查询参数不得覆盖 actor）", resp.Owner, testAccessKey)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("本人应看到 1 条，得到 %d: %+v", len(resp.Entries), resp.Entries)
	}
	if resp.Entries[0].Node != "node-self" || resp.Entries[0].Scope != volume.MeshScopeRW {
		t.Errorf("条目不符: %+v", resp.Entries[0])
	}
	if resp.Entries[0].Fingerprint == aclOtherFP {
		t.Fatalf("响应泄露他 owner 指纹: %+v", resp.Entries[0])
	}
}

// TestMeshACL_EmptyWhenNoConfig 钉住未装配配置时不 panic，返回空数组（`[]` 而非 `null`）。
func TestMeshACL_EmptyWhenNoConfig(t *testing.T) {
	entries := meshACLEntriesForOwner(nil, "alice")
	if entries == nil {
		t.Fatal("应返回空切片而非 nil（nil 序列化为 null，客户端渲染不友好）")
	}
	if len(entries) != 0 {
		t.Fatalf("应为空: %+v", entries)
	}
	// 卷无 ACL（nil）时同样安全跳过。
	cfg := Default()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: t.TempDir()}}
	if got := meshACLEntriesForOwner(cfg, "alice"); len(got) != 0 {
		t.Fatalf("无 ACL 卷应无条目: %+v", got)
	}
}

// TestMeshACL_AnonymousActor 钉住无认证部署（actor 空）归入 anonymous owner 口径。
func TestMeshACL_AnonymousActor(t *testing.T) {
	cfg := Default()
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: t.TempDir(), ACL: &VolumeACLConfig{
		Mode:        VolumeACLDeny,
		MeshReaders: []VolumeMeshReaderConfig{{Node: "node-a", Fingerprint: testReaderFP, Owner: "anonymous"}},
	}}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cfg.Validate: %v", err)
	}
	if got := meshACLEntriesForOwner(cfg, "anonymous"); len(got) != 1 {
		t.Fatalf("anonymous 应看到 1 条: %+v", got)
	}
	// 无认证部署经主 mux 请求（actor 为空 ⇒ owner=anonymous）：
	baseURL, _ := newTestServerWithAllRoutes(t, func(c *Config) {
		c.Volumes = []VolumeConfig{{Name: "main", Root: c.StorageRoot, ACL: &VolumeACLConfig{
			Mode:        VolumeACLDeny,
			MeshReaders: []VolumeMeshReaderConfig{{Node: "node-a", Fingerprint: testReaderFP, Owner: "anonymous"}},
		}}}
	})
	resp, err := http.Get(baseURL + "/api/mesh/acl")
	if err != nil {
		t.Fatalf("GET /api/mesh/acl: %v", err)
	}
	defer resp.Body.Close()
	var body MeshACLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if body.Owner != "anonymous" || len(body.Entries) != 1 {
		t.Fatalf("无认证面口径不符: %+v", body)
	}
}

// TestMeshACL_RouteRegistered 钉住路由**真的挂上了**（主 mux 面；漏注册是静默 404）。
func TestMeshACL_RouteRegistered(t *testing.T) {
	baseURL, _ := newTestServerWithAllRoutes(t, func(c *Config) {
		c.Volumes = []VolumeConfig{{Name: "main", Root: c.StorageRoot, ACL: &VolumeACLConfig{
			Mode:        VolumeACLDeny,
			MeshReaders: []VolumeMeshReaderConfig{{Node: "node-a", Fingerprint: testReaderFP, Owner: "anonymous"}},
		}}}
	})

	resp, err := http.Get(baseURL + "/api/mesh/acl")
	if err != nil {
		t.Fatalf("GET /api/mesh/acl: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d, want 200（路由未注册会 404）", resp.StatusCode)
	}
	var body MeshACLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("JSON 解析失败: %v", err)
	}
	if len(body.Entries) != 1 || body.Entries[0].Node != "node-a" {
		t.Fatalf("路由返回不符合预期: %+v", body)
	}
}
