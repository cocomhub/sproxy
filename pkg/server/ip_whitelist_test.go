// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// ip_whitelist_test.go 是「IP 白名单/信任代理」（auth.allow_ips + auth.trusted_proxies，
// roadmap 11.5-②）的测试：认证前 IP 门（门在认证链前——合法签名也 403）、信任代理
// X-Forwarded-For 解析（右向左首个非信任项）、零回归（双配置空 = 全放行）、坏 CIDR
// 启动校验拒绝、公开凭据端点同门、探活端点不门。
//
// 测试只绑 127.0.0.1（httptest 回环）；断言纯标准库；全部 t.Parallel()（R18）。

package server

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// newIPGateServer 装配带凭据 Ring 的完整路由 handler（IP 门集成测试用）。
// 认证驱动（withTestCreds + signRequest）：断言「门放行后认证链正常、门拒绝时合法
// 签名也 403」——直接证明门在认证前。
func newIPGateServer(t *testing.T, mod func(*Config)) (http.Handler, *atomic.Pointer[Config]) {
	t.Helper()
	tmpDir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.LogLevel = "error"
	if mod != nil {
		mod(cfg)
	}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	opts := RegisterRoutesOpts{
		Mux:            http.NewServeMux(),
		CfgPtr:         &cfgPtr,
		Version:        "test-version",
		BuildAt:        "test-buildat",
		Logger:         testLogger(),
		TotpRateLimit:  1000000,
		LoginRateLimit: 1000000,
	}
	withTestCreds(&opts)
	h := RegisterRoutes(t.Context(), opts)
	t.Cleanup(func() { _ = h.Close() })
	return h.Handler(), &cfgPtr
}

// ipGateReq 发送一个请求（RemoteAddr / XFF / 是否带合法 SproxySig 签名由调用方指定）。
func ipGateReq(t *testing.T, h http.Handler, method, path, remoteAddr, xff string, signed bool) int {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = remoteAddr
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	if signed {
		signRequest(req, testAccessKey, testAccessSecret)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w.Code
}

// ---- 白名单（auth.allow_ips）----

// TestIPWhitelist_AllowIPsHit：来源在白名单 → 门放行，认证链正常 → 200。
func TestIPWhitelist_AllowIPsHit(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"127.0.0.1/32"} })
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "127.0.0.1:56789", "", true); code != http.StatusOK {
		t.Fatalf("白名单命中 status = %d, want 200", code)
	}
}

// TestIPWhitelist_AllowIPsMiss：来源不在白名单 → 认证前 403（**含合法签名**——证明
// 门在认证链之前，未授权来源不泄露认证面）。
func TestIPWhitelist_AllowIPsMiss(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "127.0.0.1:56789", "", true); code != http.StatusForbidden {
		t.Fatalf("白名单未命中 status = %d, want 403（合法签名也拒绝——门在认证前）", code)
	}
}

// TestIPWhitelist_CIDRMatch：CIDR 命中（10.1.2.3 ∈ 10.0.0.0/8）→ 200。
func TestIPWhitelist_CIDRMatch(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "10.1.2.3:5555", "", true); code != http.StatusOK {
		t.Fatalf("CIDR 命中 status = %d, want 200", code)
	}
}

// TestIPWhitelist_GateBeforeAPIKeyAuth：IP 门在 api_keys 认证链前——合法 Bearer 也 403。
func TestIPWhitelist_GateBeforeAPIKeyAuth(t *testing.T) {
	t.Parallel()
	cfgPtr := &atomic.Pointer[Config]{}
	cfgPtr.Store(&Config{
		APIKeys: APIKeyConfig{Enabled: true, Keys: []APIKey{{Key: "mykey", Permission: "write"}}},
		Auth:    AuthConfig{AllowIPs: []string{"10.0.0.0/8"}},
	})
	h := &Handlers{cfgPtr: cfgPtr}
	called := false
	handler := h.authMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true }))
	r := httptest.NewRequest(http.MethodGet, "/upload", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Authorization", "Bearer mykey")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("IP 门应在 api_keys 认证前拒绝：got %d, want 403", w.Code)
	}
	if called {
		t.Fatal("门拒绝后不应进入 handler")
	}
}

// TestIPWhitelist_EmptyAllowsAll：allow_ips 空 = 特性不启用（零回归断言）——
// 认证链行为与现状完全一致（有签名 200 / 无签名 401）。
func TestIPWhitelist_EmptyAllowsAll(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, nil)
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "198.51.100.9:4444", "", true); code != http.StatusOK {
		t.Fatalf("空白名单 + 合法签名 status = %d, want 200（零回归）", code)
	}
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "198.51.100.9:4444", "", false); code != http.StatusUnauthorized {
		t.Fatalf("空白名单 + 无签名 status = %d, want 401（认证链原样）", code)
	}
}

// TestIPWhitelist_HealthzNotGated：探活端点不被门（白名单不含回环时 /healthz 仍 200）。
func TestIPWhitelist_HealthzNotGated(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if code := ipGateReq(t, h, http.MethodGet, "/healthz", "127.0.0.1:56789", "", false); code != http.StatusOK {
		t.Fatalf("/healthz 不应被 IP 门拦截：got %d, want 200", code)
	}
}

// ---- 信任代理（auth.trusted_proxies + X-Forwarded-For）----

// TestTrustProxy_XFFResolved：trusted_proxies 命中 RemoteAddr → 按 XFF 真实客户端判定。
func TestTrustProxy_XFFResolved(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) {
		c.Auth.AllowIPs = []string{"198.51.100.5/32"}
		c.Auth.TrustedProxies = []string{"127.0.0.1/32"}
	})
	// 直连来源 127.0.0.1（信任代理）+ XFF=198.51.100.5（真实客户端，在白名单）→ 200。
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "127.0.0.1:56789", "198.51.100.5", true); code != http.StatusOK {
		t.Fatalf("信任代理解析 XFF status = %d, want 200", code)
	}
}

// TestTrustProxy_XFFIgnoredWhenNoTrusted：未配 trusted_proxies → 一律忽略 XFF（防伪造）。
func TestTrustProxy_XFFIgnoredWhenNoTrusted(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) {
		c.Auth.AllowIPs = []string{"198.51.100.5/32"}
	})
	// 无 trusted_proxies：XFF 伪造被忽略 → 按直连 127.0.0.1 判定 → 不在白名单 → 403。
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "127.0.0.1:56789", "198.51.100.5", true); code != http.StatusForbidden {
		t.Fatalf("未配信任代理时 XFF 应被忽略：got %d, want 403", code)
	}
}

// TestTrustProxy_UntrustedProxyForgeIgnored：直连来源不在 trusted_proxies → XFF 忽略。
func TestTrustProxy_UntrustedProxyForgeIgnored(t *testing.T) {
	t.Parallel()
	h, _ := newIPGateServer(t, func(c *Config) {
		c.Auth.AllowIPs = []string{"198.51.100.5/32"}
		c.Auth.TrustedProxies = []string{"10.0.0.0/8"}
	})
	// RemoteAddr=203.0.113.9 不在信任列表 → XFF 伪造被忽略 → 按 203.0.113.9 → 403。
	if code := ipGateReq(t, h, http.MethodGet, "/api/files", "203.0.113.9:9999", "198.51.100.5", true); code != http.StatusForbidden {
		t.Fatalf("非信任来源伪造 XFF 应被忽略：got %d, want 403", code)
	}
}

// ---- resolveClientIP 单元测试（信任链标准语义）----

// TestTrustProxy_ResolveNoTrustedIgnoresXFF：未配信任代理一律忽略 XFF。
func TestTrustProxy_ResolveNoTrustedIgnoresXFF(t *testing.T) {
	t.Parallel()
	if got := resolveClientIP("127.0.0.1:1234", "198.51.100.5", nil); got != "127.0.0.1" {
		t.Fatalf("未配信任代理：resolve = %q, want 127.0.0.1", got)
	}
}

// TestTrustProxy_ResolveChainRightToLeft：从 XFF 链右向左取第一个非信任项（标准语义）。
func TestTrustProxy_ResolveChainRightToLeft(t *testing.T) {
	t.Parallel()
	// 信任最近一跳 + 中间跳：链 "1.2.3.4, 5.6.7.8, 9.9.9.9" → 右向左跳过信任项 → 1.2.3.4。
	trusted := parseIPNets([]string{"9.9.9.9/32", "5.6.7.8/32"})
	if got := resolveClientIP("9.9.9.9:443", "1.2.3.4, 5.6.7.8, 9.9.9.9", trusted); got != "1.2.3.4" {
		t.Fatalf("双信任跳解析 = %q, want 1.2.3.4", got)
	}
	// 仅信任最近一跳：链 "1.2.3.4, 9.9.9.9" → 1.2.3.4。
	oneHop := parseIPNets([]string{"9.9.9.9/32"})
	if got := resolveClientIP("9.9.9.9:443", "1.2.3.4, 9.9.9.9", oneHop); got != "1.2.3.4" {
		t.Fatalf("单信任跳解析 = %q, want 1.2.3.4", got)
	}
	// 空链（客户端直连代理、未加 XFF 条目）→ 回退 RemoteAddr。
	if got := resolveClientIP("9.9.9.9:443", "9.9.9.9", oneHop); got != "9.9.9.9" {
		t.Fatalf("全信任链 = %q, want 回退 RemoteAddr 9.9.9.9", got)
	}
}

// TestTrustProxy_ResolveAllTrustedFallback：链中所有条目都在信任列表 → 回退 RemoteAddr。
func TestTrustProxy_ResolveAllTrustedFallback(t *testing.T) {
	t.Parallel()
	trusted := parseIPNets([]string{"9.9.9.9/32", "5.6.7.8/32"})
	if got := resolveClientIP("9.9.9.9:443", "5.6.7.8, 9.9.9.9", trusted); got != "9.9.9.9" {
		t.Fatalf("全信任链 = %q, want 回退 RemoteAddr 9.9.9.9", got)
	}
}

// TestTrustProxy_ResolveMalformedFallback：畸形（非 IP / 超长 >64 段 / 空）XFF →
// 忽略整条回退 RemoteAddr（fail-closed，不 panic）。
func TestTrustProxy_ResolveMalformedFallback(t *testing.T) {
	t.Parallel()
	trusted := parseIPNets([]string{"9.9.9.9/32"})
	if got := resolveClientIP("9.9.9.9:443", "not-an-ip, 9.9.9.9", trusted); got != "9.9.9.9" {
		t.Fatalf("畸形 XFF = %q, want 回退 RemoteAddr", got)
	}
	long := strings.Repeat("1.2.3.4, ", 66) + "9.9.9.9"
	if got := resolveClientIP("9.9.9.9:443", long, trusted); got != "9.9.9.9" {
		t.Fatalf("超长 XFF 链 = %q, want 回退 RemoteAddr", got)
	}
	if got := resolveClientIP("9.9.9.9:443", "", trusted); got != "9.9.9.9" {
		t.Fatalf("空 XFF = %q, want 回退 RemoteAddr", got)
	}
}

// TestTrustProxy_ResolveIPv6：IPv6 信任代理与 XFF。
func TestTrustProxy_ResolveIPv6(t *testing.T) {
	t.Parallel()
	trusted := parseIPNets([]string{"::1/128"})
	if got := resolveClientIP("[::1]:56789", "2001:db8::5", trusted); got != "2001:db8::5" {
		t.Fatalf("IPv6 信任代理解析 = %q, want 2001:db8::5", got)
	}
	if got := resolveClientIP("[::1]:56789", "2001:db8::5", nil); got != "::1" {
		t.Fatalf("IPv6 未配信任代理 = %q, want ::1", got)
	}
}

// ---- 公开凭据端点同门（白名单部署所有入口一致）----

// TestIPWhitelist_RegisterGated：register 回环在白名单 → 200；不在 → 403（门在注册逻辑前）。
func TestIPWhitelist_RegisterGated(t *testing.T) {
	t.Parallel()
	h, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"127.0.0.1/32"} })
	if st, _ := serveRegister(t, h, loopRemoteV4, []byte(`{}`)); st != http.StatusOK {
		t.Fatalf("回环在白名单时 register status = %d, want 200", st)
	}
	h2, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if st, _ := serveRegister(t, h2, loopRemoteV4, []byte(`{}`)); st != http.StatusForbidden {
		t.Fatalf("回环不在白名单时 register status = %d, want 403", st)
	}
}

// TestIPWhitelist_NonceGated：nonce 端点同门。
func TestIPWhitelist_NonceGated(t *testing.T) {
	t.Parallel()
	h, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"127.0.0.1/32"} })
	if st, _ := serveNonce(t, h, loopRemoteV4, []byte(`{}`)); st != http.StatusOK {
		t.Fatalf("回环在白名单时 nonce status = %d, want 200", st)
	}
	h2, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if st, _ := serveNonce(t, h2, loopRemoteV4, []byte(`{}`)); st != http.StatusForbidden {
		t.Fatalf("回环不在白名单时 nonce status = %d, want 403", st)
	}
}

// TestIPWhitelist_LoginGated：login 端点同门——门放行后进入 handler（未知 login_type
// → 400），门拒绝 → 403。
func TestIPWhitelist_LoginGated(t *testing.T) {
	t.Parallel()
	body := []byte(`{"login_type":"bogus"}`)
	h, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"127.0.0.1/32"} })
	if st, _ := serveLogin(t, h, loopRemoteV4, body); st != http.StatusBadRequest {
		t.Fatalf("门放行后 login 应进入 handler（400 未知 login_type），got %d", st)
	}
	h2, _, _ := newRegisterTestServer(t, func(c *Config) { c.Auth.AllowIPs = []string{"10.0.0.0/8"} })
	if st, _ := serveLogin(t, h2, loopRemoteV4, body); st != http.StatusForbidden {
		t.Fatalf("回环不在白名单时 login status = %d, want 403", st)
	}
}

// ---- 配置校验（fail-closed）----

// TestIPWhitelist_ValidateBadCIDR：allow_ips / trusted_proxies 非法条目 → 拒绝启动。
func TestIPWhitelist_ValidateBadCIDR(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Auth.AllowIPs = []string{"not-a-cidr"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("allow_ips 含非法条目应拒绝启动（fail-closed）")
	}
	cfg2 := Default()
	cfg2.Auth.TrustedProxies = []string{"999.1.1.1/32"}
	if err := cfg2.Validate(); err == nil {
		t.Fatal("trusted_proxies 含非法条目应拒绝启动（fail-closed）")
	}
}

// TestIPWhitelist_ValidatePureIPNormalized：纯 IP 被接受并归一为 /32、/128。
func TestIPWhitelist_ValidatePureIPNormalized(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.Auth.AllowIPs = []string{"127.0.0.1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("纯 IP 应被接受: %v", err)
	}
	if cfg.Auth.AllowIPs[0] != "127.0.0.1/32" {
		t.Fatalf("纯 IP 应归一为 /32, got %q", cfg.Auth.AllowIPs[0])
	}
	cfg.Auth.AllowIPs = []string{"::1"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("IPv6 纯 IP 应被接受: %v", err)
	}
	if cfg.Auth.AllowIPs[0] != "::1/128" {
		t.Fatalf("IPv6 纯 IP 应归一为 /128, got %q", cfg.Auth.AllowIPs[0])
	}
}

// TestIPWhitelist_CoverLoopback：AllowIPsCoverLoopback 判定（启动告警辅助）。
func TestIPWhitelist_CoverLoopback(t *testing.T) {
	t.Parallel()
	if !AllowIPsCoverLoopback([]string{"127.0.0.1/32", "::1/128"}) {
		t.Fatal("v4+v6 回环均覆盖应为 true")
	}
	if AllowIPsCoverLoopback([]string{"127.0.0.1/32"}) {
		t.Fatal("仅 v4 回环应 false（缺 ::1）")
	}
	if AllowIPsCoverLoopback([]string{"10.0.0.0/8"}) {
		t.Fatal("不含任何回环应 false")
	}
}

// TestTrustProxy_ParseIPNets：parseIPNets 支持 CIDR 与纯 IP 归一。
func TestTrustProxy_ParseIPNets(t *testing.T) {
	t.Parallel()
	nets := parseIPNets([]string{"127.0.0.1", "10.0.0.0/8", "::1"})
	if len(nets) != 3 {
		t.Fatalf("parseIPNets len = %d, want 3", len(nets))
	}
	if !nets[0].Contains(net.ParseIP("127.0.0.1")) || nets[0].Contains(net.ParseIP("127.0.0.2")) {
		t.Fatal("纯 IP 127.0.0.1 应按 /32 归一")
	}
	if !nets[1].Contains(net.ParseIP("10.2.3.4")) || nets[1].Contains(net.ParseIP("11.0.0.1")) {
		t.Fatal("10.0.0.0/8 应命中 10.2.3.4、不命中 11.0.0.1")
	}
	if !nets[2].Contains(net.ParseIP("::1")) {
		t.Fatal("纯 IP ::1 应按 /128 归一")
	}
}
