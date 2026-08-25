// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// noRedirectClient returns an http.Client that returns any redirect as the
// direct response rather than following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestHubNodesHandler_Disabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Hub 未启用时 /api/hub/nodes 路由未注册，返回 404。
	// （GET /{$} 精确匹配根路径，不再 catch-all 拦截未知路径，避免与 /ws 等冲突）
	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/api/hub/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 when hub disabled, got %d", resp.StatusCode)
	}
}

func TestHubStatsHandler_Disabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  cfgPtr,
		Version: "test",
		BuildAt: "now",
		Logger:  testLogger(),
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/api/hub/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 when hub disabled, got %d", resp.StatusCode)
	}
}

func TestHubNodesHandler_Enabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.NodeID = "test-node"
	cfgPtr.Store(cfg)

	rt := hub.NewMeshRouteTable()

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     cfgPtr,
		Version:    "test",
		BuildAt:    "now",
		Logger:     testLogger(),
		RouteTable: rt,
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/hub/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when hub enabled, got %d", resp.StatusCode)
	}

	// hubNodesHandler returns a raw JSON array (e.g. [])
	// Decode as generic JSON to verify it's an array
	var result any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result == nil {
		t.Error("expected non-nil response")
	}
}

func TestHubRemoveNodeHandler_Enabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfgPtr.Store(cfg)

	rt := hub.NewMeshRouteTable()

	// 先注册一个节点，否则删除会返回 404（节点不存在）
	rt.AddNode("", "node-1", nil)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     cfgPtr,
		Version:    "test",
		BuildAt:    "now",
		Logger:     testLogger(),
		RouteTable: rt,
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, err := http.NewRequest("DELETE", srv.URL+"/api/hub/nodes/node-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on remove node, got %d", resp.StatusCode)
	}
}

func TestHubStatsHandler_Enabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.NodeID = "test-node"
	cfgPtr.Store(cfg)

	rt := hub.NewMeshRouteTable()

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     cfgPtr,
		Version:    "test",
		BuildAt:    "now",
		Logger:     testLogger(),
		RouteTable: rt,
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/hub/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when hub enabled, got %d", resp.StatusCode)
	}
}

// TestHubNodesHandler_MeshIsolation（M-9 集成验收）：/api/hub/nodes 按调用方 AK 的
// mesh 过滤——mesh-a 的 AK 只见 mesh-a 节点，mesh-b 的 AK 只见 mesh-b 节点。
func TestHubNodesHandler_MeshIsolation(t *testing.T) {
	t.Parallel()

	const (
		akA = "sk-mesh-a-0011223344556677" // AccessKeyMesh → "mesh-a"
		akB = "sk-mesh-b-8899aabbccddeeff" // AccessKeyMesh → "mesh-b"
		sk  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.AccessKeys = []AccessKeyConfig{{Key: akA, Secret: sk}, {Key: akB, Secret: sk}}
	cfgPtr.Store(cfg)

	rt := hub.NewMeshRouteTable()
	a, _ := xfertest.Pipe()
	mA := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = mA.Close() })
	rt.Add("mesh-a", hub.NodeInfo{ID: "node-a", Mux: mA, Connected: time.Now()}, nil)
	b, _ := xfertest.Pipe()
	mB := mux.New(b, mux.RoleDialer)
	t.Cleanup(func() { _ = mB.Close() })
	rt.Add("mesh-b", hub.NodeInfo{ID: "node-b", Mux: mB, Connected: time.Now()}, nil)

	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        mux,
		CfgPtr:     cfgPtr,
		Version:    "test",
		BuildAt:    "now",
		Logger:     testLogger(),
		RouteTable: rt,
	})
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	type nodeResp struct {
		ID string `json:"id"`
	}
	listNodes := func(t *testing.T, ak string) []nodeResp {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/hub/nodes", nil)
		signRequest(req, ak, sk)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/hub/nodes (ak=%s) = %d, want 200", ak, resp.StatusCode)
		}
		var nodes []nodeResp
		if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
			t.Fatal(err)
		}
		return nodes
	}

	nodesA := listNodes(t, akA)
	if len(nodesA) != 1 || nodesA[0].ID != "node-a" {
		t.Fatalf("mesh-a AK 请求只见 mesh-a 节点, got %+v", nodesA)
	}
	nodesB := listNodes(t, akB)
	if len(nodesB) != 1 || nodesB[0].ID != "node-b" {
		t.Fatalf("mesh-b AK 请求只见 mesh-b 节点, got %+v", nodesB)
	}
}
