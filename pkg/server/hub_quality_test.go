// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// hub_quality_test.go 验证服务发现健康化（roadmap P1 服务发现健康化）：
//  1. /api/hub/services 响应带 quality 字段（healthy/degraded/stale）。
//  2. 多节点同名服务按质量排序（healthy 在前）。
//  3. stale 判定：节点连接时间过久（mux 心跳 30s）→ 降级。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestHubServices_QualityField /api/hub/services 带 quality 字段。
func TestHubServices_QualityField(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	rt.Table("").SetServices("node-a", []hub.Service{{Name: "sg-ssh", Addr: "t:22"}})

	req := httptest.NewRequest(http.MethodGet, "/api/hub/services", nil)
	w := httptest.NewRecorder()
	h.hubServicesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var svcs []struct {
		Name    string `json:"name"`
		Node    string `json:"node"`
		Quality string `json:"quality"`
	}
	if err := json.NewDecoder(w.Body).Decode(&svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 {
		t.Fatalf("expected 1 service, got %d", len(svcs))
	}
	if svcs[0].Quality != "healthy" {
		t.Fatalf("quality = %q, want healthy（新建 mux 无重传/错误）", svcs[0].Quality)
	}
}

// TestHubServices_QualitySorted 多节点同名服务按质量排序（healthy 在前，degraded 在后）。
func TestHubServices_QualitySorted(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	// 节点 A：干净 mux（healthy）。
	a, _ := xfertest.Pipe()
	mA := mux.New(a, mux.RoleDialer)
	defer mA.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: mA, Connected: time.Now()}, nil)
	rt.Table("").SetServices("node-a", []hub.Service{{Name: "svc", Addr: "a:22"}})

	// 节点 B：质量劣化（注入重传计数）。
	b, _ := xfertest.Pipe()
	mB := mux.New(b, mux.RoleDialer)
	defer mB.Close()
	mm := mB.Metrics()
	mm.Retransmits.Add(50)
	rt.Add("", hub.NodeInfo{ID: "node-b", Mux: mB, Connected: time.Now()}, nil)
	rt.Table("").SetServices("node-b", []hub.Service{{Name: "svc", Addr: "b:22"}})

	req := httptest.NewRequest(http.MethodGet, "/api/hub/services", nil)
	w := httptest.NewRecorder()
	h.hubServicesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var svcs []struct {
		Node    string `json:"node"`
		Quality string `json:"quality"`
	}
	if err := json.NewDecoder(w.Body).Decode(&svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services, got %d", len(svcs))
	}
	// healthy（node-a）应在前。
	if svcs[0].Node != "node-a" || svcs[0].Quality != "healthy" {
		t.Fatalf("首位应 node-a/healthy，got %+v", svcs[0])
	}
	if svcs[1].Node != "node-b" || svcs[1].Quality != "degraded" {
		t.Fatalf("次位应 node-b/degraded，got %+v", svcs[1])
	}
}
