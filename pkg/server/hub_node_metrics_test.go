// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// hub_node_metrics_test.go 验证节点级状态仪表（roadmap P2 节点级状态仪表）：
//  1. /metrics 输出 per-node 质量明细（sproxy_hub_node_quality / retransmits / errors）。
//  2. /api/hub/nodes 带 quality 分档字段（#501 扩展 per-node 明细）。
//  3. RTT/延迟入 /metrics（sproxy_hub_node_rtt_ms）与 /api/hub/nodes（rtt_ms）（11.1-⑦）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestHubNodeMetrics_PerNodeMetrics /metrics 输出 per-node 质量明细。
func TestHubNodeMetrics_PerNodeMetrics(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.metrics = NewMetrics()

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	mm := m.Metrics()
	mm.Retransmits.Add(5)
	mm.Errors.Add(2)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.MetricsHandler(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "sproxy_hub_node_quality") {
		t.Fatalf("/metrics 应含 per-node quality: %s", body[:min(len(body), 300)])
	}
	if !strings.Contains(body, `sproxy_hub_node_quality{node="node-a"} 1`) {
		t.Fatalf("node-a quality 应为 degraded(1): %s", body[:min(len(body), 400)])
	}
	if !strings.Contains(body, `sproxy_hub_node_retransmits{node="node-a"} 5`) {
		t.Fatalf("node-a retransmits 应为 5")
	}
}

// TestMetrics_RTTGaugeOutput 节点带 mux → /metrics 含 sproxy_hub_node_rtt_ms{node}
// 且值=LastRTTNanos/1e6 毫秒（缺失/单位错误 → 红）。
func TestMetrics_RTTGaugeOutput(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.metrics = NewMetrics()

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	m.Metrics().LastRTTNanos.Store(12 * time.Millisecond.Nanoseconds()) // 12ms

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.MetricsHandler(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `sproxy_hub_node_rtt_ms{node="node-a"} 12`) {
		t.Fatalf("RTT gauge 缺失或单位错误（应为 12ms）: %s", body[:min(len(body), 400)])
	}
}

// TestMetrics_RTTGaugeUnknownIsMinusOne RTT 无采样（LastRTTNanos==0）→ gauge 显式写 -1
// （禁静默当 0：新节点/未采样不能假报健康）。
func TestMetrics_RTTGaugeUnknownIsMinusOne(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}
	h.metrics = NewMetrics()

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.MetricsHandler(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `sproxy_hub_node_rtt_ms{node="node-a"} -1`) {
		t.Fatalf("无采样节点 RTT gauge 应为 -1（显式未知）: %s", body[:min(len(body), 400)])
	}
}

// TestHubNodes_QualityDetail /api/hub/nodes 带 quality 字段。
func TestHubNodes_QualityDetail(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)

	req := httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil)
	w := httptest.NewRecorder()
	h.hubNodesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var nodes []struct {
		ID      string `json:"id"`
		Quality string `json:"quality"`
	}
	if err := json.NewDecoder(w.Body).Decode(&nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "node-a" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].Quality != "healthy" {
		t.Fatalf("quality = %q, want healthy", nodes[0].Quality)
	}
}

// TestHubNodesAPI_RTTField /api/hub/nodes 响应带 rtt_ms 字段（同 /metrics 数据源）。
func TestHubNodesAPI_RTTField(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	m.Metrics().LastRTTNanos.Store(25 * time.Millisecond.Nanoseconds()) // 25ms

	req := httptest.NewRequest(http.MethodGet, "/api/hub/nodes", nil)
	w := httptest.NewRecorder()
	h.hubNodesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var nodes []struct {
		ID    string `json:"id"`
		RTTMs int64  `json:"rtt_ms"`
	}
	if err := json.NewDecoder(w.Body).Decode(&nodes); err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 1 || nodes[0].ID != "node-a" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if nodes[0].RTTMs != 25 {
		t.Fatalf("rtt_ms = %d, want 25", nodes[0].RTTMs)
	}
}
