// SPDX-License-Identifier: Apache-2.0
//
// Copyright 2026 The Cocomhub Authors. All rights reserved.

package server

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestHubNodesHandler_Disabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Hub 未启用时 /api/hub/nodes 路由未注册 → 404
	resp, err := http.Get(srv.URL + "/api/hub/nodes")
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
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
	defer h.Close()

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/hub/stats")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 when hub disabled, got %d", resp.StatusCode)
	}
}
