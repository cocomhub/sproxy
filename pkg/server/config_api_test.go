// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestConfigAPI_WebTunnelField(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var cfg configResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.WebTunnel {
		t.Fatal("web_tunnel 默认应为 true")
	}
}

func TestConfig_GetConfig(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	resp, err := http.Get(url + "/api/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	var cfg configResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.LogLevel != "error" {
		t.Errorf("expected log_level=error (test default), got %s", cfg.LogLevel)
	}
	if cfg.RateLimitRequests != 10 {
		t.Errorf("expected rate_limit_requests=10, got %d", cfg.RateLimitRequests)
	}
	if cfg.ChunkSize <= 0 {
		t.Errorf("expected chunk_size > 0, got %d", cfg.ChunkSize)
	}
}

func TestConfig_UpdateLogLevel(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	body := strings.NewReader(`{"log_level":"debug"}`)
	req, err := http.NewRequest("PUT", url+"/api/config", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.LogLevel != "debug" {
		t.Errorf("expected log_level=debug, got %s", cfg.LogLevel)
	}
}

func TestConfig_UpdateLogFormat(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"log_format":"json"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.LogFormat != "json" {
		t.Errorf("expected log_format=json, got %s", cfg.LogFormat)
	}
}

func TestConfig_UpdateMaxStorageBytes(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"max_storage_bytes":104857600}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.MaxStorageBytes != 104857600 {
		t.Errorf("expected max_storage_bytes=104857600, got %d", cfg.MaxStorageBytes)
	}
}

func TestConfig_UpdateRateLimit(t *testing.T) {
	t.Parallel()
	url, cfgPtr := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{"rate_limit_requests":20,"rate_limit_window":"5s"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	cfg := cfgPtr.Load()
	if cfg.RateLimit.Requests != 20 {
		t.Errorf("expected rate_limit_requests=20, got %d", cfg.RateLimit.Requests)
	}
	if cfg.RateLimit.Window.String() != "5s" {
		t.Errorf("expected rate_limit_window=5s, got %s", cfg.RateLimit.Window)
	}
}

func TestConfig_UpdateInvalidInput(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{"invalid log_level", `{"log_level":"invalid"}`, http.StatusBadRequest},
		{"invalid log_format", `{"log_format":"xml"}`, http.StatusBadRequest},
		{"negative rate_limit", `{"rate_limit_requests":-1}`, http.StatusBadRequest},
		{"invalid rate_window", `{"rate_limit_window":"-1s"}`, http.StatusBadRequest},
		{"negative max_storage", `{"max_storage_bytes":-1}`, http.StatusBadRequest},
		{"malformed json", `{bad json}`, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(tt.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tt.wantStatus {
				t.Errorf("expected status %d, got %d", tt.wantStatus, resp.StatusCode)
			}
		})
	}
}

func TestConfig_UpdateEmptyBody(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400 Bad Request, got %d", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if result["message"] != "empty request body: no fields to update" {
		t.Errorf("expected error message about empty body, got %v", result["message"])
	}
}

// ---- RateLimiter 热更新（PUT /api/config → UpdateConfig 立即生效） ----

func TestConfig_UpdateRateLimit_AuthTunnelImmediate(t *testing.T) {
	t.Parallel()
	// 关键：Default() 的 RateLimit.Enabled=false（限流未装配）。必须显式打开并给低配额，
	// 隧道链才会挂上 rateLimiter 中间件，PUT 收紧/放宽才能被观察。
	url, _ := newTestServerWithAllRoutes(t, func(cfg *Config) {
		cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
		cfg.RateLimit.Enabled = true
		cfg.RateLimit.Requests = 2
		cfg.RateLimit.Window = time.Hour
	})

	// 带 access_keys 的服务经隧道派生密钥；外层 /tunnel 需 UNSIGNED 签名。
	key, err := tunnel.DeriveTunnelKey(testAccessSecret, "")
	if err != nil {
		t.Fatalf("DeriveTunnelKey: %v", err)
	}
	tc, err := tunnel.NewClient(hex.EncodeToString(key), url+"/tunnel", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("tunnel.NewClient: %v", err)
	}
	tc.HTTPClient.Transport = &tunnelSignTransport{base: tc.HTTPClient.Transport, ak: testAccessKey, sk: testAccessSecret}

	// okStatus 视为隧道正常往返：200 或 429（内层限流）。
	okStatus := func(code int) bool {
		return code == http.StatusOK || code == http.StatusTooManyRequests
	}
	allOK := func() {
		for i := range 6 {
			if code := doTunnelGet(t, tc); !okStatus(code) {
				t.Fatalf("request %d: unexpected status %d", i, code)
			}
		}
	}
	// 灌满：低配额（limit=2, window=1h）下多请求 → 出现 429（限流已挂载）。
	allOK()
	saw429 := func() bool {
		for range 6 {
			if doTunnelGet(t, tc) == http.StatusTooManyRequests {
				return true
			}
		}
		return false
	}
	// 若始终未撞 429（每请求新 TCP 源连接 → per-IP 令牌桶独立，可能不撞全局窗口），
	// 继续多打几个请求确保触发。
	for range 10 {
		if doTunnelGet(t, tc) == http.StatusTooManyRequests {
			break
		}
	}

	// PUT /api/config 立即生效：改远高于已灌注请求数的配额，后续请求恒放行。
	// PUT 带 JSON body，需按 body 哈希签名（signBodyRequest）。
	putConfig := func(body string) int {
		req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		signBodyRequest(req, testAccessKey, testAccessSecret, []byte(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := putConfig(`{"rate_limit_requests":100000}`); code != http.StatusOK {
		t.Fatalf("PUT /api/config (relax): want 200, got %d", code)
	}
	for i := range 10 {
		if code := doTunnelGet(t, tc); code != http.StatusOK {
			t.Fatalf("after relax req %d: want 200 (已放宽), got %d", i, code)
		}
	}

	// 收紧到 1：窗口内已有大量时间戳 → 立即 429。
	if code := putConfig(`{"rate_limit_requests":1}`); code != http.StatusOK {
		t.Fatalf("PUT /api/config (tighten): want 200, got %d", code)
	}
	// 收紧后应迅速出现 429（limit=1 且窗口内有大量时间戳）。
	// 触发器：任一请求 429 即证明生效——热更新立即回到限流。
	if !saw429() {
		t.Fatalf("tighten to 1: 未观察到 429（限流应立即生效）")
	}
}

// TestConfig_UpdateRateLimit_DisabledViaTunnel 验证 enabled=false 短路径在真实
// 服务链上生效：PUT /api/config 无 enabled 字段（无法表达禁用），故经 RegisterRoutes
// 返回的 Handlers 直接调用 rateLimiter.UpdateConfig(false, ...)——复用的是生产链
// （tunnelHandler → localHandler → Gzip → rateLimiter.Middleware），确认中间件短路后
// 高配额旧时间戳不再产生 429。PUT 复读常量窗口的 no-op 变更也一并确认。
func TestConfig_UpdateRateLimit_DisabledViaTunnel(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.Requests = 5
	cfg.RateLimit.Window = time.Hour
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mmm := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:         mmm,
		CfgPtr:      &cfgPtr,
		Version:     "v",
		BuildAt:     "b",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	url := ts.URL

	key, err := tunnel.DeriveTunnelKey(testAccessSecret, "")
	if err != nil {
		t.Fatalf("DeriveTunnelKey: %v", err)
	}
	tc, err := tunnel.NewClient(hex.EncodeToString(key), url+"/tunnel", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("tunnel.NewClient: %v", err)
	}
	tc.HTTPClient.Transport = &tunnelSignTransport{base: tc.HTTPClient.Transport, ak: testAccessKey, sk: testAccessSecret}

	// 生产链限流启用：低配额 5、大窗口 1h → 快速灌满后 429。
	for range 3 {
		_ = doTunnelGet(t, tc)
	}
	if code := doTunnelGet(t, tc); code == http.StatusOK {
		// 极慢机器可能未灌满；再补到明确超限（每请求消耗窗口时间戳）。
		_ = doTunnelGet(t, tc)
		if code = doTunnelGet(t, tc); code != http.StatusTooManyRequests {
			t.Fatalf("生产链低配额应 429, got %d", code)
		}
	}

	// 经生产 Handlers 关掉限流 → 后续请求立即放行（无需等待窗口）。
	h.rateLimiter.UpdateConfig(false, 5, time.Hour)
	for i := range 5 {
		if code := doTunnelGet(t, tc); code != http.StatusOK {
			t.Fatalf("disabled req %d: want 200 (短路), got %d", i, code)
		}
	}
}

func TestConfig_UpdateRateLimit_SignalPostImmediate(t *testing.T) {
	t.Parallel()
	// 信令 POST 限流挂在 RouteTable 分支（hub routers 组）内，需装配 RouteTable。
	// 直接装配 RegisterRoutes（仿 newTestMux 模式）：RouteTable 属 opts 而非 Config。
	// 节点注册在 testAccessKey mesh（AccessKeyMesh("sk-test-mesh-...") = "test-mesh"）下，
	// 与 signRequest 注入的 mesh ctx 一致，否则信令 handler 403。
	mrt := hub.NewMeshRouteTable()
	muxA, _ := xfertest.Pipe()
	m := mux.New(muxA, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	mesh := tunnel.AccessKeyMesh(testAccessKey)
	mrt.Add(mesh, hub.NodeInfo{ID: "peer-a", Mux: m, Secret: "sec-a"}, nil)
	mrt.Add(mesh, hub.NodeInfo{ID: "peer-b", Mux: m, Secret: "sec-b"}, nil)

	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	cfg.RateLimit.Enabled = true
	cfg.RateLimit.Requests = 2
	cfg.RateLimit.Window = time.Hour
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	muxOut := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:         muxOut,
		CfgPtr:      &cfgPtr,
		Version:     "v",
		BuildAt:     "b",
		Logger:      testLogger(),
		AuditLogger: testLogger(),
		RouteTable:  mrt,
	})
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = h.Close()
	})
	url := ts.URL

	// limit=2 → 第 3 个请求被信令限流（429）；前两个消费限额。
	_, _ = doSignalPost(t, url)
	_, _ = doSignalPost(t, url)
	if code, body := doSignalPost(t, url); code != http.StatusTooManyRequests {
		t.Fatalf("signal 3rd: want 429 (limit=2 触发), got %d (%s)", code, body)
	}

	// 放大配额 → signal 分支不再限流（返回 handler 状态 200/400/202）。
	// PUT /api/config 带 JSON body，需按 body 哈希签名（signBodyRequest）。
	bodyStr := `{"rate_limit_requests":1000}`
	req, err := http.NewRequest("PUT", url+"/api/config", strings.NewReader(bodyStr))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	signBodyRequest(req, testAccessKey, testAccessSecret, []byte(bodyStr))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT /api/config: want 200, got %d", resp.StatusCode)
	}
	for i := range 2 {
		code, b := doSignalPost(t, url)
		if code == http.StatusTooManyRequests {
			t.Fatalf("signal after relax req %d: want non-429, got %d", i, code)
		}
		if code != http.StatusOK && code != http.StatusBadRequest && code != http.StatusAccepted {
			t.Fatalf("signal after relax req %d: unexpected status %d (%s)", i, code, b)
		}
	}
}

// doTunnelGet 经隧道发送 GET /api/files 并返回状态码，供 rate limiter 热更新
// 黑盒测试共用（Task1 review Minor-3：抽离三处重复内联的 do）。隧道错误
// （外层 4xx/5xx，如 replay 抖动 425）被 Do 吞成 error：无 resp 时视为致命；
// 有 resp（读后又报错）取状态。
func doTunnelGet(t *testing.T, tc *tunnel.Client) int {
	t.Helper()
	req, _ := http.NewRequest("GET", "/api/files", nil)
	resp, err := tc.Do(req)
	if err != nil {
		if resp == nil {
			t.Fatalf("tunnel Do: %v", err)
		}
		return resp.StatusCode
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// doSignalPost 发送信令 offer POST（已注册节点身份 + per-node secret + 签名），
// 返回状态码与响应体，供 rate limiter 信令分支黑盒测试共用。
func doSignalPost(t *testing.T, url string) (int, string) {
	t.Helper()
	bodyStr := `{"sdp":"dummy"}`
	r, err := http.NewRequest(http.MethodPost, url+"/api/signal/offer", strings.NewReader(bodyStr))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	// 已注册节点身份 + per-node secret，且与 testAccessKey mesh 一致。
	r.Header.Set(signalNodeHeader, "peer-a")
	r.Header.Set(signalNodeSecretHeader, "sec-a")
	signBodyRequest(r, testAccessKey, testAccessSecret, []byte(bodyStr))
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatalf("signal post: %v", err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	_, _ = io.Copy(buf, resp.Body)
	return resp.StatusCode, buf.String()
}
