// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// metrics_auth_test.go 验证 /metrics 的 token 认证（roadmap 6.x P1）：
//  1. 未配置 metrics_token → /metrics 匿名可读（零回归）。
//  2. 配置 metrics_token → 无凭据 401；?token= 正确 200；Bearer 正确 200。
//  3. 错误 token（query 与 Bearer）→ 401。
//  4. 其它端点不受 metrics_token 影响（仅 /metrics 门）。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"

	"log/slog"
)

func TestMetricsTokenAuth(t *testing.T) {
	t.Parallel()
	// 场景 1：未配置 → 匿名可读。
	{
		url, _, _ := newTestServer(t, nil)
		resp, err := http.Get(url + "/metrics")
		if err != nil {
			t.Fatalf("GET /metrics: %v", err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(body), "sproxy_requests_total") {
			t.Fatalf("未配置 token 应可读: status=%d body=%q", resp.StatusCode, body[:min(len(body), 80)])
		}
	}
	// 场景 2+3：配置 token。
	{
		const tok = "secret-token-123"
		url, _, _ := newTestServer(t, func(c *Config) { c.MetricsToken = tok })
		cases := []struct {
			name   string
			path   string
			header string
			want   int
		}{
			{"无凭据", "/metrics", "", 401},
			{"query 正确", "/metrics?token=" + tok, "", 200},
			{"Bearer 正确", "/metrics", "Bearer " + tok, 200},
			{"query 错误", "/metrics?token=wrong", "", 401},
			{"Bearer 错误", "/metrics", "Bearer wrong", 401},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				req, _ := http.NewRequest(http.MethodGet, url+tc.path, nil)
				if tc.header != "" {
					req.Header.Set("Authorization", tc.header)
				}
				cl := &http.Client{Transport: netutil.IsolatedTransport()}
				resp, err := cl.Do(req)
				if err != nil {
					t.Fatalf("%s: %v", tc.name, err)
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode != tc.want {
					t.Fatalf("%s: status=%d want %d", tc.name, resp.StatusCode, tc.want)
				}
			})
		}
	}
	// 场景 4：其它端点不受影响（/healthz 匿名可读）。
	{
		const tok = "another-token"
		url, _, _ := newTestServer(t, func(c *Config) { c.MetricsToken = tok })
		resp, err := http.Get(url + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("/healthz 应不受 metrics_token 影响: %d", resp.StatusCode)
		}
	}
}

// TestMetricsTokenAuth_WrongScheme 非 Bearer/query 凭据形态（Basic）→ 401。
func TestMetricsTokenAuth_WrongScheme(t *testing.T) {
	t.Parallel()
	const tok = "basic-token"
	url, _, _ := newTestServer(t, func(c *Config) { c.MetricsToken = tok })
	req, _ := http.NewRequest(http.MethodGet, url+"/metrics", nil)
	req.SetBasicAuth("admin", tok)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("Basic: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("Basic 凭据应 401: %d", resp.StatusCode)
	}
}

// 编译期引用（避免 unused 告警）。
var _ = context.Background
var _ = slog.Default
var _ atomic.Int64
var _ = httptest.NewServer
