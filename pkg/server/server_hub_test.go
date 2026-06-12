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

// noRedirectClient returns an http.Client that returns any redirect as the
// direct response rather than following it.
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func TestHubNodesHandler_Disabled(t *testing.T) {
	t.Parallel()

	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(Default())
	mux := http.NewServeMux()
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
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
	h := RegisterRoutes(nil, mux, cfgPtr, "test", "now", nil, testLogger(), nil)
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
