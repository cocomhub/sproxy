// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// federation_services_test.go 验证跨 hub 服务发现（roadmap P2 mesh 集群化 F1）：
//  1. federationServicesHandler 返回本 hub 节点宣告服务。
//  2. /api/hub/services 聚合联邦候选服务（mesh 过滤 + node+name 去重）。

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

// TestFederationServicesHandler 服务端返回节点宣告服务。
func TestFederationServicesHandler(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	rt.Table("").SetServices("node-a", []hub.Service{{Name: "sg-ssh", Addr: "t:22"}})

	req := httptest.NewRequest(http.MethodGet, "/api/hub/federation/services", nil)
	w := httptest.NewRecorder()
	h.federationServicesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var svcs []struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(w.Body).Decode(&svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 1 || svcs[0].Name != "sg-ssh" {
		t.Fatalf("svcs = %+v", svcs)
	}
}

// TestHubServices_MergeFederation /api/hub/services 聚合联邦候选服务。
func TestHubServices_MergeFederation(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	fc, err := hub.NewFederationClient(nil, time.Hour, time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handlers{routeTable: rt, fedClient: fc, logger: testutil.DiscardLogger()}

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "local", Mux: m, Connected: time.Now()}, nil)
	rt.Table("").SetServices("local", []hub.Service{{Name: "local-svc", Addr: "l:80"}})

	// 注入联邦候选服务（远程 hub 节点）。
	fc.SetCandidateServices("peer1", []hub.FederationService{{Node: "remote", Mesh: "", Name: "remote-svc", Addr: "r:80"}})

	req := httptest.NewRequest(http.MethodGet, "/api/hub/services", nil)
	w := httptest.NewRecorder()
	h.hubServicesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var svcs []struct {
		Node string `json:"node"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(w.Body).Decode(&svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("应含本地+联邦 2 服务, got %d: %+v", len(svcs), svcs)
	}
	names := map[string]bool{}
	for _, s := range svcs {
		names[s.Name] = true
	}
	if !names["local-svc"] || !names["remote-svc"] {
		t.Fatalf("应含 local-svc + remote-svc: %+v", names)
	}
}
