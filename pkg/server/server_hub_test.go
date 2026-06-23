// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
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

	// Hub 未启用时 /api/hub/nodes 路由未注册，catch-all GET / 返回 301 重定向
	client := noRedirectClient()
	resp, err := client.Get(srv.URL + "/api/hub/nodes")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// catch-all GET / 返回 301 MovedPermanently → /ui/，而不是 hub handler 的 200
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("expected 301 redirect when hub disabled, got %d", resp.StatusCode)
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
	if resp.StatusCode != http.StatusMovedPermanently {
		t.Errorf("expected 301 redirect when hub disabled, got %d", resp.StatusCode)
	}
}

func TestHubNodesHandler_Enabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.Hub.NodeID = "test-node"
	cfgPtr.Store(cfg)

	rt := hub.NewRouteTable()

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

	rt := hub.NewRouteTable()

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

	rt := hub.NewRouteTable()

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
