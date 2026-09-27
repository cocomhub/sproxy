// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ratelimit_config_test.go 钉住 roadmap 12.1-6 片 2（config 接线）：
//  1. RateLimitConfig 新增 endpoints / endpoint_default / max_concurrent 字段的
//     YAML 解析与默认零值（零回归：不配置 = 新维度全关）。
//  2. RegisterRoutes 装配点调用 UpdateDimensions：配置 endpoints/max_concurrent 后
//     rateLimiter 立即按新维度拒绝（per-endpoint 429 / 并发 429）。
//  3. PUT /api/config 热更新路径调用 UpdateDimensions：cfgPtr 里的新维度值经 PUT
//     同步到 rateLimiter（max_concurrent 收紧即时生效）。
//  4. /metrics 输出 sproxy_rate_limit_rejected_total{scope=...} 拒绝计数。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestRateLimitConfig_NewDimensionsYAMLRoundtrip endpoints/endpoint_default/max_concurrent
// 经 SaveConfig → LoadConfig 的 yaml 往返后保持（window 以纳秒数字序列化，与现有
// RateLimitConfig.Window 行为一致）。
func TestRateLimitConfig_NewDimensionsYAMLRoundtrip(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.Requests = 100
	cfg.RateLimit.Window = time.Second
	cfg.RateLimit.Endpoints = map[string]EndpointLimit{
		"/download": {Limit: 2, Window: time.Hour},
	}
	cfg.RateLimit.EndpointDefault = EndpointLimit{Limit: 5, Window: time.Minute}
	cfg.RateLimit.MaxConcurrent = 7

	path := filepath.Join(t.TempDir(), "sproxy.yaml")
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := loaded.RateLimit.Endpoints["/download"]; got.Limit != 2 || got.Window != time.Hour {
		t.Fatalf("Endpoints[/download] = %+v, want {limit:2 window:1h0m0s}", got)
	}
	if len(loaded.RateLimit.Endpoints) != 1 {
		t.Fatalf("Endpoints 长度 = %d, want 1", len(loaded.RateLimit.Endpoints))
	}
	if d := loaded.RateLimit.EndpointDefault; d.Limit != 5 || d.Window != time.Minute {
		t.Fatalf("EndpointDefault = %+v, want {limit:5 window:1m0s}", d)
	}
	if loaded.RateLimit.MaxConcurrent != 7 {
		t.Fatalf("MaxConcurrent = %d, want 7", loaded.RateLimit.MaxConcurrent)
	}
}

// TestRateLimitConfig_NewDimensionsZeroByDefault 新维度默认零值（零回归：不配置全关）。
func TestRateLimitConfig_NewDimensionsZeroByDefault(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if len(cfg.RateLimit.Endpoints) != 0 {
		t.Fatalf("Endpoints 默认应为空, got %v", cfg.RateLimit.Endpoints)
	}
	if cfg.RateLimit.EndpointDefault != (EndpointLimit{}) {
		t.Fatalf("EndpointDefault 默认应为零值, got %+v", cfg.RateLimit.EndpointDefault)
	}
	if cfg.RateLimit.MaxConcurrent != 0 {
		t.Fatalf("MaxConcurrent 默认应为 0, got %d", cfg.RateLimit.MaxConcurrent)
	}
}

// buildRateLimitHandlers 装配启用限流的 RegisterRoutes（测试共用），返回 Handlers。
func buildRateLimitHandlers(t *testing.T, mutate func(*Config)) *Handlers {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	if mutate != nil {
		mutate(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "v",
		BuildAt:     "b",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// TestRateLimit_RegisterRoutesWiresDimensions 装配点调用 UpdateDimensions：
// endpoints 规则与 max_concurrent 并发闸在 RegisterRoutes 后立即生效（装配漏调 → 红）。
func TestRateLimit_RegisterRoutesWiresDimensions(t *testing.T) {
	t.Parallel()
	h := buildRateLimitHandlers(t, func(cfg *Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.Requests = 1000
		cfg.RateLimit.Window = time.Hour
		cfg.RateLimit.Endpoints = map[string]EndpointLimit{
			"/download": {Limit: 1, Window: time.Hour},
		}
	})
	if h.rateLimiter == nil {
		t.Fatal("rateLimiter 未装配")
	}
	mw := h.rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// /download limit=1：第 1 次放行、第 2 次 429（装配点 UpdateDimensions 生效）。
	if code := sendAllowReq(t, mw, "192.0.2.1:1", "/download"); code != http.StatusOK {
		t.Fatalf("/download 第 1 次: want 200, got %d", code)
	}
	if code := sendAllowReq(t, mw, "192.0.2.2:2", "/download"); code != http.StatusTooManyRequests {
		t.Fatalf("/download 第 2 次: want 429（装配点 UpdateDimensions 生效）, got %d", code)
	}
	// 未配置路径透传（endpoint 维度不干预）。
	if code := sendAllowReq(t, mw, "192.0.2.3:3", "/upload"); code != http.StatusOK {
		t.Fatalf("/upload 无规则: want 200, got %d", code)
	}
}

// TestRateLimit_RegisterRoutesWiresMaxConcurrent 装配点 max_concurrent=2：
// 占满 2 个并发槽后第 3 个并发请求 429（并发闸装配生效）。
func TestRateLimit_RegisterRoutesWiresMaxConcurrent(t *testing.T) {
	t.Parallel()
	h := buildRateLimitHandlers(t, func(cfg *Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.Requests = 1000
		cfg.RateLimit.Window = time.Hour
		cfg.RateLimit.MaxConcurrent = 2
	})
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	mw := h.rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		_ = sendAllowReq(t, mw, "192.0.2.1:1", "/")
	}()
	go func() {
		_ = sendAllowReq(t, mw, "192.0.2.2:2", "/")
	}()
	for range 2 {
		<-entered
	}
	// 第 3 个并发：sem 满 → 429。
	if code := sendAllowReq(t, mw, "192.0.2.3:3", "/"); code != http.StatusTooManyRequests {
		t.Fatalf("第 3 个并发: want 429 (max_concurrent=2 装配生效), got %d", code)
	}
	close(release)
}

// TestRateLimit_UpdateDimensionsViaPutConfig PUT /api/config 热更新路径调用
// UpdateDimensions：cfgPtr 中的新维度值（max_concurrent）经 PUT 同步到 rateLimiter。
func TestRateLimit_UpdateDimensionsViaPutConfig(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.Requests = 1000
	cfg.RateLimit.Window = time.Hour
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:         mux,
		CfgPtr:      &cfgPtr,
		Version:     "v",
		BuildAt:     "b",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(ts.Close)
	url := ts.URL

	// 更新 cfgPtr 副本：MaxConcurrent=1（模拟配置热改后重载到 cfgPtr）。
	newCfg := *cfgPtr.Load()
	newCfg.RateLimit.MaxConcurrent = 1
	cfgPtr.Store(&newCfg)

	// PUT /api/config 任意字段触发热更新段 → UpdateDimensions 读取 cfgPtr 新值。
	bodyStr := `{"rate_limit_requests":1000}`
	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(bodyStr))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	signBodyRequest(req, testAccessKey, testAccessSecret, []byte(bodyStr))
	resp, err := testHTTPClient(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/config: want 200, got %d", resp.StatusCode)
	}

	// max_concurrent=1 已同步：第 1 个请求进入后，第 2 个并发 429。
	entered := make(chan struct{})
	release := make(chan struct{})
	mw := h.rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		_ = sendAllowReq(t, mw, "192.0.2.1:1", "/")
	}()
	<-entered
	// 第 2 个并发：子 goroutine 内请求，结果经 channel 回传（避免变异下进入
	// 阻塞 handler 导致测试体悬挂）。变异（热更新漏调 UpdateDimensions → sem 未
	// 同步）时返回 200 → 断言失败，不悬挂。
	second := make(chan int, 1)
	go func() {
		second <- sendAllowReq(t, mw, "192.0.2.2:2", "/")
	}()
	select {
	case code := <-second:
		if code != http.StatusTooManyRequests {
			t.Fatalf("PUT 后并发闸: want 429 (max_concurrent=1 经 PUT 同步), got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("PUT 后并发闸: 第 2 个并发未返回（sem 未生效，请求被放行进入阻塞 handler）")
	}
	close(release)
}

// TestRateLimit_RejectedMetrics /metrics 输出 sproxy_rate_limit_rejected_total：
// per-endpoint 拒绝计 scope=endpoint、并发闸拒绝计 scope=concurrent。
func TestRateLimit_RejectedMetrics(t *testing.T) {
	t.Parallel()
	h := buildRateLimitHandlers(t, func(cfg *Config) {
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.Requests = 1000
		cfg.RateLimit.Window = time.Hour
		cfg.RateLimit.Endpoints = map[string]EndpointLimit{
			"/download": {Limit: 1, Window: time.Hour},
		}
		cfg.RateLimit.MaxConcurrent = 1
	})
	mw := h.rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	// 触发 endpoint 拒绝。
	_ = sendAllowReq(t, mw, "192.0.2.1:1", "/download")
	_ = sendAllowReq(t, mw, "192.0.2.2:2", "/download") // 429 endpoint

	// 触发 concurrent 拒绝：占满 1 槽后第 2 个并发 429。
	entered := make(chan struct{})
	release := make(chan struct{})
	mw2 := h.rateLimiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	go func() {
		_ = sendAllowReq(t, mw2, "192.0.2.3:3", "/")
	}()
	<-entered
	_ = sendAllowReq(t, mw2, "192.0.2.4:4", "/") // 429 concurrent
	close(release)

	// /metrics 渲染（不经 httptest server，直接调 handler）。
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	h.MetricsHandler(w, r)
	body := w.Body.String()
	if !strings.Contains(body, `sproxy_rate_limit_rejected_total{scope="endpoint",endpoint="/download"} 1`) {
		t.Fatalf("/metrics 缺 endpoint 拒绝计数:\n%s", body)
	}
	if !strings.Contains(body, `sproxy_rate_limit_rejected_total{scope="concurrent",endpoint="/"} 1`) {
		t.Fatalf("/metrics 缺 concurrent 拒绝计数:\n%s", body)
	}
}
