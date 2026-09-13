// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// mesh_status_test.go 钉住 `GET /api/mesh/status`（W1）：跨节点面的**只读运维视图**。
//
// 设计：配置态（是否启用/监听/pin 数/角色）由 Handler 从 `cfg` 推导；**运行态**（实际监听地址、
// node 角色是否真的跑起来）由装配层经 `SetMeshRuntimeInfo` 注入——两者分开的原因：配置了但未起
// 成功（端口被占/凭据缺失）时必须显示真实状态，否则视图会误导运维。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/volume"
)

const otherReaderFP = "sha256:" + "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// meshStatusFixture 构造带两个 mesh_readers 条目（seek read / rw）与齐全 mesh 配置的 Handler。
func meshStatusFixture(t *testing.T) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{
			{Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice", Scope: volume.MeshScopeRead},
			{Node: "nodeC", Fingerprint: otherReaderFP, Owner: "alice", Scope: volume.MeshScopeRW},
		},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:19000"
	cfg.RemoteWrite.Enabled = true
	cfg.RemoteWrite.Listen = "127.0.0.1:19001"
	cfg.Mesh.HubURL = "https://hub.example.com:18083"
	cfg.Mesh.NodeID = "node-self-signaling"
	cfg.Mesh.Node.Enabled = true
	cfg.Mesh.Node.NodeID = "node-self"
	cfg.Mesh.Node.WebRTC = true
	cfg.SetDefaults()
	return newMeshStatusHandlers(t, cfg)
}

// newMeshStatusHandlers 用给定 cfg 注册 Handlers（带测试凭据）。
//
// 该路由注册在**主 mux**（受 SproxySig 保护）⇒ fixture 需带凭据、请求需签名（与真实客户端一致；
// 也顺带证明「认证面可达」）。
func newMeshStatusHandlers(t *testing.T, cfg *Config) *Handlers {
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

// doMeshStatus 发一次 GET /api/mesh/status 并解析响应。
func doMeshStatus(t *testing.T, h *Handlers) MeshStatus {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/mesh/status", nil)
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
	var out MeshStatus
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("解析响应: %v body=%s", err, rec.Body.String())
	}
	return out
}

// TestMeshStatus_ConfigAndRuntime 钉住字段来源与 pin 计数口径（读面=全部、写面=授写）。
func TestMeshStatus_ConfigAndRuntime(t *testing.T) {
	h := meshStatusFixture(t)
	h.SetMeshRuntimeInfo(func() MeshRuntimeInfo {
		return MeshRuntimeInfo{
			RemoteReadAddr:  "127.0.0.1:49000", // 运行态实际地址（配置是 :19000）
			NodeRoleRunning: true,
		}
	})

	st := doMeshStatus(t, h)

	if st.RemoteRead == nil || st.RemoteWrite == nil {
		t.Fatalf("两个面都应出现: %+v", st)
	}
	if st.RemoteRead.Addr != "127.0.0.1:49000" {
		t.Fatalf("读面地址应取**运行态**实际地址, got %q", st.RemoteRead.Addr)
	}
	if st.RemoteWrite.Addr != "127.0.0.1:19001" {
		t.Fatalf("写面未注入运行态时应回落配置地址, got %q", st.RemoteWrite.Addr)
	}
	if st.RemoteRead.Pinned != 2 {
		t.Fatalf("读面 pin 数=%d want 2（全部 mesh_readers）", st.RemoteRead.Pinned)
	}
	if st.RemoteWrite.Pinned != 1 {
		t.Fatalf("写面 pin 数=%d want 1（仅 scope 授予写）", st.RemoteWrite.Pinned)
	}
	if st.Node == nil || !st.Node.Running || st.Node.NodeID != "node-self" || !st.Node.WebRTC {
		t.Fatalf("node 角色状态不符: %+v", st.Node)
	}
	if st.HubURL != "https://hub.example.com:18083" || !st.SignalingEnabled {
		t.Fatalf("hub/signaling 不符: hub=%q signaling=%v", st.HubURL, st.SignalingEnabled)
	}
}

// TestMeshStatus_DisabledStaysEmpty 钉住默认（全关）时三个子对象都不出现（零回归：不泄露无关信息）。
func TestMeshStatus_DisabledStaysEmpty(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	h := newMeshStatusHandlers(t, cfg)

	st := doMeshStatus(t, h)
	if st.RemoteRead != nil || st.RemoteWrite != nil || st.Node != nil {
		t.Fatalf("未启用任何面/角色时不应出现子对象: %+v", st)
	}
	if st.SignalingEnabled || st.HubURL != "" {
		t.Fatalf("未配置信令/hub 时应为空: %+v", st)
	}
}

// TestMeshStatus_NodeRoleNotRunning 钉住「配置启用但角色没跑起来」的诚实显示。
func TestMeshStatus_NodeRoleNotRunning(t *testing.T) {
	h := meshStatusFixture(t)
	h.SetMeshRuntimeInfo(func() MeshRuntimeInfo { return MeshRuntimeInfo{} }) // 角色未起

	st := doMeshStatus(t, h)
	if st.Node == nil {
		t.Fatal("配置启用 node 角色时应出现 node 对象（即使未运行）")
	}
	if st.Node.Running {
		t.Fatal("未运行时 Running 必须为 false（诚实显示，避免误导运维）")
	}
	if st.Node.NodeID != "node-self" {
		t.Fatalf("未运行时仍应显示配置的 node_id, got %q", st.Node.NodeID)
	}
}
