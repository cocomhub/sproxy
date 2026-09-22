// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestPprof_DisabledByDefault 验证 pprof 默认关：/debug/pprof/ 404（零回归）。
func TestPprof_DisabledByDefault(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := testHTTPClient(t).Get(url + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("pprof 默认关应 404, got %d", resp.StatusCode)
	}
}

// TestPprof_UnauthorizedWhenEnabled 验证开启后未认证 401（安全开关可观测 + 认证保护）。
func TestPprof_UnauthorizedWhenEnabled(t *testing.T) {
	t.Parallel()
	// 用 Creds 版（凭据 Ring 非空）：未认证请求不被回环兜底放行 → 401。
	url, _ := newTestServerWithAllRoutesCreds(t, func(c *Config) {
		c.DebugPprofEnabled = true
	})

	resp, err := testHTTPClient(t).Get(url + "/debug/pprof/")
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("pprof 开启后未认证应 401, got %d", resp.StatusCode)
	}
}

// TestPprof_AuthorizedServesProfiles 验证开启 + 认证 → 200 且含 pprof 内容。
func TestPprof_AuthorizedServesProfiles(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutesCreds(t, func(c *Config) {
		c.DebugPprofEnabled = true
	})

	req, err := http.NewRequest("GET", url+"/debug/pprof/", nil)
	if err != nil {
		t.Fatal(err)
	}
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatalf("GET /debug/pprof/: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pprof 开启 + 认证应 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "goroutine") && !strings.Contains(string(body), "heap") {
		t.Fatalf("pprof 索引页应含 profile 列表, got %q", string(body))
	}

	// heap profile 可拉取（认证）。
	req2, err := http.NewRequest("GET", url+"/debug/pprof/heap", nil)
	if err != nil {
		t.Fatal(err)
	}
	signRequest(req2, testAccessKey, testAccessSecret)
	resp2, err := testHTTPClient(t).Do(req2)
	if err != nil {
		t.Fatalf("GET /debug/pprof/heap: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("heap profile 应 200, got %d", resp2.StatusCode)
	}
}

// TestPprof_HeapMetrics 验证 /metrics 暴露 sproxy_heap_* 系列。
func TestPprof_HeapMetrics(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := testHTTPClient(t).Get(url + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, name := range []string{"sproxy_heap_alloc_bytes", "sproxy_heap_objects", "sproxy_gc_cycles"} {
		if !strings.Contains(text, name) {
			t.Fatalf("/metrics 应含 %s, got %q", name, text)
		}
	}
}
