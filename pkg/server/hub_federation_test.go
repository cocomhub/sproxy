// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// fedMockPeer 返回固定节点表的 mock 联邦对端（不带 mesh 过滤——模拟异常对端，
// 验证本端 merge 二次隔离兜底）。
func fedMockPeer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newFedClient 构造一个已同步一次 mock peer 的 FederationClient。
func newFedClient(t *testing.T, body string) *hub.FederationClient {
	t.Helper()
	srv := fedMockPeer(t, body)
	fc, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "peerB", URL: srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fc.Close)
	if err := fc.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll mock peer: %v", err)
	}
	return fc
}

// TestHubNodesHandler_MergesFederation：/api/hub/nodes 合并联邦候选节点——
// 本地路由表权威 + 联邦独有候选，按 node-id 去重（本地优先）。
func TestHubNodesHandler_MergesFederation(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000"}, nil)

	fc := newFedClient(t, `[{"id":"node-a","addr":"192.168.1.1:9000","mesh":""},{"id":"node-b","addr":"192.168.1.2:9000","mesh":""}]`)
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.SetFederationClient(fc)

	w := httptest.NewRecorder()
	h.hubNodesHandler(w, httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]int, len(resp))
	for _, n := range resp {
		ids[n.ID]++
	}
	if ids["node-a"] != 1 {
		t.Errorf("node-a 应出现一次（本地 + 联邦去重）, got %d", ids["node-a"])
	}
	if ids["node-b"] != 1 {
		t.Errorf("联邦独有候选 node-b 应合并进发现列表, got %d", ids["node-b"])
	}
}

// TestHubNodesHandler_FederationMeshIsolation：命名 mesh 请求者不得看到其它 mesh
// （含默认 mesh cm==""）的联邦候选——merge 按 mesh 严格隔离，防跨 mesh 泄漏。
func TestHubNodesHandler_FederationMeshIsolation(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("B", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)

	// mock 对端返回混合 mesh（含默认 mesh 节点），验证本端 merge 二次隔离兜底。
	fc := newFedClient(t, `[{"id":"node-b","addr":"192.168.1.2:9000","mesh":"B"},{"id":"node-default","addr":"192.168.1.3:9000","mesh":""}]`)
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.SetFederationClient(fc)

	req := httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil)
	req = req.WithContext(withMesh(req.Context(), "B"))
	w := httptest.NewRecorder()
	h.hubNodesHandler(w, req)
	var resp []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]bool, len(resp))
	for _, n := range resp {
		ids[n.ID] = true
	}
	if !ids["node-b"] {
		t.Errorf("mesh-B 请求者应看到 node-b, got %+v", resp)
	}
	if ids["node-default"] {
		t.Fatalf("mesh-B 请求者不应看到默认 mesh 联邦候选 node-default（mesh 隔离泄漏）, got %+v", resp)
	}
}

// TestFederationNodesEndpoint_ByMesh：联邦节点表端点按调用方 mesh 过滤（M-9），
// 返回带 mesh 字段供对端合并隔离。
func TestFederationNodesEndpoint_ByMesh(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("B", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-default"), Addr: "192.168.1.3:9000"}, nil)

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	req := httptest.NewRequest(http.MethodGet, "/api/hub/federation/nodes", nil)
	req = req.WithContext(withMesh(req.Context(), "B"))
	w := httptest.NewRecorder()
	h.federationNodesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp []struct {
		ID   string `json:"id"`
		Mesh string `json:"mesh"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 1 || resp[0].ID != "node-b" || resp[0].Mesh != "B" {
		t.Fatalf("mesh-B 请求者应只看到 node-b(mesh=B), got %+v", resp)
	}
}

// TestFederationNodesEndpoint_NoRouteTable：hub 未启用时联邦端点返回 404。
func TestFederationNodesEndpoint_NoRouteTable(t *testing.T) {
	h := &Handlers{logger: testutil.DiscardLogger()} // routeTable nil
	w := httptest.NewRecorder()
	h.federationNodesHandler(w, httptest.NewRequest(http.MethodGet, "/api/hub/federation/nodes", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// TestFederationEndpoint_AuthRequired：联邦节点表端点在 access_keys 配置后
// 受 SproxySig 认证保护——无凭据请求 401（fail-closed）。
func TestFederationEndpoint_AuthRequired(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	cfg := Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.UploadsDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Hub.Enabled = true
	cfg.Hub.Federation.Enabled = true
	// fail-closed：配置 access_keys 后联邦端点无凭据必须 401。
	cfg.AccessKeys = []AccessKeyConfig{{Key: "sk-test-0123456789abcdef", Secret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}}

	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     &cfgPtr,
		RouteTable: rt,
		Logger:     testutil.DiscardLogger(),
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	resp, err := http.Get(ts.URL + "/api/hub/federation/nodes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据请求联邦端点应 401, got %d", resp.StatusCode)
	}
}

// TestFederationSync_AuthSuccessAndFailure（DoD 2 认证两侧）：配置 access_keys 的
// hub 联邦端点上，正确 AK/SK 拉取成功、错误 SK 拉取返回错误（401，fail-closed）。
func TestFederationSync_AuthSuccessAndFailure(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)
	cfg := Default()
	cfg.UploadsDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Hub.Enabled = true
	cfg.Hub.Federation.Enabled = true
	const (
		testAK = "sk-0123456789abcdef"
		testSK = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAK, Secret: testSK}}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     &cfgPtr,
		RouteTable: rt,
		Logger:     testutil.DiscardLogger(),
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	// 正确凭据：拉取成功，候选含 node-b。
	fcOK, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: ts.URL, AccessKey: testAK, AccessKeySecret: testSK}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcOK.Close)
	if err := fcOK.SyncAll(context.Background()); err != nil {
		t.Fatalf("正确凭据拉取应成功: %v", err)
	}
	cands := fcOK.Candidates()
	found := false
	for _, c := range cands {
		if c.ID == "node-b" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("正确凭据拉取后候选应含 node-b, got %+v", cands)
	}

	// 错误 SK：拉取失败（401，fail-closed）。
	fcBad, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: ts.URL, AccessKey: testAK, AccessKeySecret: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcBad.Close)
	if err := fcBad.SyncAll(context.Background()); err == nil {
		t.Fatalf("错误 SK 拉取应返回错误（fail-closed 401）")
	}
}

// TestFederationNodesEndpoint_MeshFromAccessKey（DoD 2 mesh 派生链路）：入站联邦
// 端点按拉取方 AK 的 mesh 过滤——用命名 mesh 的 AK 签名拉取，只返回该 mesh 节点，
// 默认 mesh 节点不泄漏。
func TestFederationNodesEndpoint_MeshFromAccessKey(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("meshM", hub.NodeInfo{ID: hub.NodeID("node-m"), Addr: "192.168.1.2:9000"}, nil)
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-default"), Addr: "192.168.1.3:9000"}, nil)
	cfg := Default()
	cfg.UploadsDir = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Hub.Enabled = true
	cfg.Hub.Federation.Enabled = true
	// meshM 的 AK（sk-meshM-<16hex>）：authMiddleware 验签后按 AccessKeyMesh 派生 mesh=meshM。
	const (
		meshMAK = "sk-meshM-0123456789abcdef"
		meshMSK = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	)
	cfg.AccessKeys = []AccessKeyConfig{{Key: meshMAK, Secret: meshMSK}}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     &cfgPtr,
		RouteTable: rt,
		Logger:     testutil.DiscardLogger(),
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/hub/federation/nodes", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	sproxysig.SignRequest(req, meshMAK, meshMSK)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var nodes []struct {
		ID   string `json:"id"`
		Mesh string `json:"mesh"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes) != 1 || nodes[0].ID != "node-m" || nodes[0].Mesh != "meshM" {
		t.Fatalf("meshM 调用方应只看到 node-m(mesh=meshM), got %+v", nodes)
	}
}

// TestDualHubPeering_NodesVisible（DoD 1）：两 hub peering 后节点表互见。
// hub-A 的联邦客户端指向 hub-B，同步后 A 的 /api/hub/nodes 应同时看到
// 本地节点 node-a 与联邦节点 node-b。
func TestDualHubPeering_NodesVisible(t *testing.T) {
	// ---- hub-B（被拉取方）----
	rtB := hub.NewMeshRouteTable()
	rtB.Add("", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)
	cfgB := Default()
	cfgB.UploadsDir = t.TempDir()
	cfgB.LogLevel = "error"
	cfgB.Hub.Enabled = true
	cfgB.Hub.Federation.Enabled = true
	var cfgPtrB atomic.Pointer[Config]
	cfgPtrB.Store(cfgB)
	muxB := http.NewServeMux()
	hB := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        muxB,
		CfgPtr:     &cfgPtrB,
		RouteTable: rtB,
		Logger:     testutil.DiscardLogger(),
	})
	tsB := httptest.NewServer(hB.Handler())
	t.Cleanup(func() { tsB.Close(); _ = hB.Close() })

	// ---- hub-A（拉取方）----
	rtA := hub.NewMeshRouteTable()
	rtA.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000"}, nil)
	cfgA := Default()
	cfgA.UploadsDir = t.TempDir()
	cfgA.LogLevel = "error"
	cfgA.Hub.Enabled = true
	cfgA.Hub.Federation.Enabled = true
	var cfgPtrA atomic.Pointer[Config]
	cfgPtrA.Store(cfgA)
	muxA := http.NewServeMux()
	hA := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        muxA,
		CfgPtr:     &cfgPtrA,
		RouteTable: rtA,
		Logger:     testutil.DiscardLogger(),
	})
	tsA := httptest.NewServer(hA.Handler())
	t.Cleanup(func() { tsA.Close(); _ = hA.Close() })

	// A 的联邦客户端指向 B，同步一次。
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: tsB.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("hub-A 拉取 hub-B 节点表: %v", err)
	}
	hA.SetFederationClient(fcA)

	// A 的 /api/hub/nodes 应同时含 node-a（本地）与 node-b（联邦）。
	resp, err := http.Get(tsA.URL + "/api/hub/nodes")
	if err != nil {
		t.Fatalf("GET /api/hub/nodes: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var nodes []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if !ids["node-a"] {
		t.Errorf("hub-A 应看到本地 node-a, got %+v", nodes)
	}
	if !ids["node-b"] {
		t.Errorf("hub-A 应看到联邦节点 node-b（来自 hub-B）, got %+v", nodes)
	}
}

// TestDualHubPeering_MeshNotLeaked（DoD 3）：联邦同步不破坏 mesh 隔离——
// hub-B 命名 mesh 的节点不得泄漏给 hub-A 的其它 mesh 调用方。
// 场景：hub-B 的联邦端点按拉取方 mesh 过滤；hub-A 用默认 mesh（无凭据）拉取，
// 只能拿到默认 mesh 节点，命名 mesh 节点不进入 A 的候选，/api/hub/nodes 不泄漏。
func TestDualHubPeering_MeshNotLeaked(t *testing.T) {
	// hub-B：默认 mesh + 命名 mesh "private" 节点。
	rtB := hub.NewMeshRouteTable()
	rtB.Add("", hub.NodeInfo{ID: hub.NodeID("node-public"), Addr: "192.168.1.2:9000"}, nil)
	rtB.Add("private", hub.NodeInfo{ID: hub.NodeID("node-secret"), Addr: "192.168.1.9:9000"}, nil)
	cfgB := Default()
	cfgB.UploadsDir = t.TempDir()
	cfgB.LogLevel = "error"
	cfgB.Hub.Enabled = true
	cfgB.Hub.Federation.Enabled = true
	var cfgPtrB atomic.Pointer[Config]
	cfgPtrB.Store(cfgB)
	muxB := http.NewServeMux()
	hB := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        muxB,
		CfgPtr:     &cfgPtrB,
		RouteTable: rtB,
		Logger:     testutil.DiscardLogger(),
	})
	tsB := httptest.NewServer(hB.Handler())
	t.Cleanup(func() { tsB.Close(); _ = hB.Close() })

	// hub-A：默认 mesh 本地节点。
	rtA := hub.NewMeshRouteTable()
	rtA.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000"}, nil)
	cfgA := Default()
	cfgA.UploadsDir = t.TempDir()
	cfgA.LogLevel = "error"
	cfgA.Hub.Enabled = true
	cfgA.Hub.Federation.Enabled = true
	var cfgPtrA atomic.Pointer[Config]
	cfgPtrA.Store(cfgA)
	muxA := http.NewServeMux()
	hA := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        muxA,
		CfgPtr:     &cfgPtrA,
		RouteTable: rtA,
		Logger:     testutil.DiscardLogger(),
	})
	tsA := httptest.NewServer(hA.Handler())
	t.Cleanup(func() { tsA.Close(); _ = hA.Close() })

	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: tsB.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("hub-A 拉取 hub-B 节点表: %v", err)
	}
	hA.SetFederationClient(fcA)

	// A 的默认 mesh 调用方只能看到默认 mesh 节点：node-a + node-public，绝无 node-secret。
	resp, err := http.Get(tsA.URL + "/api/hub/nodes")
	if err != nil {
		t.Fatalf("GET /api/hub/nodes: %v", err)
	}
	defer resp.Body.Close()
	var nodes []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		ids[n.ID] = true
	}
	if !ids["node-public"] {
		t.Errorf("hub-A 应看到默认 mesh 联邦节点 node-public, got %+v", nodes)
	}
	if ids["node-secret"] {
		t.Fatalf("hub-A 不得看到 hub-B 命名 mesh 节点 node-secret（mesh 隔离泄漏）, got %+v", nodes)
	}
}
