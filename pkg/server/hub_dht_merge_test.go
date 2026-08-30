// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// TestHubNodesHandler_MergesDHT：/api/hub/nodes 合并 DHT 候选节点——
// 路由表权威节点 + DHT 独有候选节点，去重（路由表优先）。
// 满足"路由表仍 hub 权威；DHT 只提供候选节点/发现，不改状态"。
func TestHubNodesHandler_MergesDHT(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	// 路由表权威节点 node-a。
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000"}, nil)

	// DHT：node-a（与路由表重复）+ node-b（DHT 独有候选）。
	dht := hub.NewDHT()
	if err := dht.Register(context.Background(), hub.PeerInfo{ID: "node-a", Addrs: []string{"192.168.1.1:9000"}, Meta: map[string]string{"mesh": "", "addr": "192.168.1.1:9000"}}); err != nil {
		t.Fatalf("dht register node-a: %v", err)
	}
	if err := dht.Register(context.Background(), hub.PeerInfo{ID: "node-b", Addrs: []string{"192.168.1.2:9000"}, Meta: map[string]string{"mesh": "", "addr": "192.168.1.2:9000"}}); err != nil {
		t.Fatalf("dht register node-b: %v", err)
	}

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.SetDHT(dht)

	w := httptest.NewRecorder()
	h.hubNodesHandler(w, httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp []struct {
		ID   string `json:"id"`
		Addr string `json:"addr,omitempty"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	ids := make(map[string]int)
	for _, n := range resp {
		ids[n.ID]++
	}
	if ids["node-a"] != 1 {
		t.Errorf("node-a 应出现一次（路由表权威 + DHT 去重）, got %d", ids["node-a"])
	}
	if ids["node-b"] != 1 {
		t.Errorf("DHT 独有候选 node-b 应合并进发现列表, got %d", ids["node-b"])
	}
	// omitzero 回归（审查 #9）：DHT 候选无连接时间，JSON 不应输出 connected 字段。
	if strings.Contains(w.Body.String(), `"connected"`) {
		t.Errorf("DHT 候选不应输出 connected（omitzero 省略零值时间）: %s", w.Body.String())
	}
}

// TestHubNodesHandler_DHTMeshIsolation（安全审查 #1 回归）：命名 mesh 请求者不得
// 看到默认 mesh（cm==""）的 DHT 候选——DHT 合并按 mesh 严格隔离，防跨 mesh 节点
// 泄漏（信令按 node-id 存转，泄漏可被利用跨 mesh 拨号）。
func TestHubNodesHandler_DHTMeshIsolation(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("B", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)

	dht := hub.NewDHT()
	if err := dht.Register(context.Background(), hub.PeerInfo{ID: "node-b", Meta: map[string]string{"mesh": "B"}}); err != nil {
		t.Fatalf("dht register node-b: %v", err)
	}
	if err := dht.Register(context.Background(), hub.PeerInfo{ID: "node-default", Meta: map[string]string{"mesh": ""}}); err != nil {
		t.Fatalf("dht register node-default: %v", err)
	}

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.SetDHT(dht)

	// mesh-B 请求者。
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
		t.Fatalf("mesh-B 请求者不应看到默认 mesh 候选 node-default（mesh 隔离泄漏）, got %+v", resp)
	}
}

// TestHubNodesHandler_NoDHT_Unchanged：未注入 DHT 时，/api/hub/nodes 行为不变
// （只返回路由表节点）。
func TestHubNodesHandler_NoDHT_Unchanged(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000"}, nil)

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()} // dht nil

	w := httptest.NewRecorder()
	h.hubNodesHandler(w, httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil))
	var resp []struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp) != 1 || resp[0].ID != "node-a" {
		t.Fatalf("未注入 DHT 应只返回路由表节点, got %+v", resp)
	}
}

// TestHubNodesHandler_VirtualIP 校验 /api/hub/nodes 响应携带 virtual_ip 字段
// （供 mesh node / 一次性 CLI 构建 vipTable）。
func TestHubNodesHandler_VirtualIP(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "192.168.1.1:9000", VirtualIP: netip.MustParseAddr("100.64.0.2")}, nil)
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-b"), Addr: "192.168.1.2:9000"}, nil)

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	w := httptest.NewRecorder()
	h.hubNodesHandler(w, httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp []struct {
		ID        string `json:"id"`
		VirtualIP string `json:"virtual_ip"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := make(map[string]string, len(resp))
	for _, n := range resp {
		got[n.ID] = n.VirtualIP
	}
	if got["node-a"] != "100.64.0.2" {
		t.Fatalf("node-a virtual_ip = %q, want 100.64.0.2", got["node-a"])
	}
	if got["node-b"] != "" {
		t.Fatalf("node-b 无虚拟 IP 应省略, got %q", got["node-b"])
	}
}
