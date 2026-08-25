// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

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

func TestHubServicesHandler(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger()}

	// 注册一个节点并宣告服务（默认 mesh ""）
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.Add("", hub.NodeInfo{ID: "node-a", Mux: m, Connected: time.Now()}, nil)
	rt.Table("").SetServices("node-a", []hub.Service{
		{Name: "sg-ssh", Addr: "target.example.com:22"},
		{Name: "local-web", Addr: "127.0.0.1:8080"},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/hub/services", nil)
	w := httptest.NewRecorder()
	h.hubServicesHandler(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var svcs []struct {
		Name string `json:"name"`
		Node string `json:"node"`
		Addr string `json:"addr"`
	}
	if err := json.NewDecoder(w.Body).Decode(&svcs); err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(svcs), svcs)
	}
	// ListServices 按 (node, name) 稳定排序：local-web < sg-ssh
	want := []struct{ node, name, addr string }{
		{node: "node-a", name: "local-web", addr: "127.0.0.1:8080"},
		{node: "node-a", name: "sg-ssh", addr: "target.example.com:22"},
	}
	for i, w := range want {
		if svcs[i].Node != w.node || svcs[i].Name != w.name || svcs[i].Addr != w.addr {
			t.Fatalf("entry %d mismatch: got %+v, want %+v", i, svcs[i], w)
		}
	}
}

func TestHubServicesHandler_NotEnabled(t *testing.T) {
	h := &Handlers{logger: testutil.DiscardLogger()} // routeTable nil
	req := httptest.NewRequest(http.MethodGet, "/api/hub/services", nil)
	w := httptest.NewRecorder()
	h.hubServicesHandler(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestRouteTableServicesOf(t *testing.T) {
	rt := hub.NewRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	rt.AddWithInfo(hub.NodeInfo{ID: "n1", Mux: m, Connected: time.Now()})
	rt.SetServices("n1", []hub.Service{{Name: "svc1", Addr: "1.2.3.4:80"}})

	svcs := rt.ServicesOf("n1")
	if len(svcs) != 1 || svcs[0].Name != "svc1" {
		t.Fatalf("unexpected services: %+v", svcs)
	}
	if len(rt.ServicesOf("nope")) != 0 {
		t.Fatal("expected empty for unknown node")
	}
	rt.ClearServices("n1")
	if len(rt.ServicesOf("n1")) != 0 {
		t.Fatal("expected empty after ClearServices")
	}
}
