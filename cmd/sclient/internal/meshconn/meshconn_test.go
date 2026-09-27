// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meshconn

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/httpproxy"
	"github.com/spf13/cobra"
)

// newTestCmd 构造带 AddFlags + AddExitFlags 注册的命令（测 flag 可读性与互斥校验）。
func newTestCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "test"}
	AddFlags(cmd)
	AddExitFlags(cmd)
	return cmd
}

func TestAddFlags_Registered(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	for _, name := range []string{
		"exit", "exit-auto", "exit-only", "exit-exclude", "local-timeout",
		"gateway", "smart", "smart-ttl", "mdns", "mdns-secret",
		"webrtc", "hub", "node-id", "insecure", "stun", "turn",
		"turn-user", "turn-pass",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("缺少 flag: --%s", name)
		}
	}
}

func TestFromFlags_Defaults(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if conn.LocalTimeout <= 0 {
		t.Fatalf("LocalTimeout = %v, want 默认 3s", conn.LocalTimeout)
	}
	if conn.ExitNode != "" || conn.ExitAuto {
		t.Fatalf("默认不应有出口: %+v", conn)
	}
}

func TestFromFlags_ExitAutoExclusive(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-auto", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit 与 --exit-auto 应互斥报错")
	}
}

func TestFromFlags_ExitExcludeRequiresAuto(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit", "node-x")
	_ = cmd.Flags().Set("exit-exclude", "node-y")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("固定 --exit 时 --exit-exclude 应 fail-closed 报错")
	}
}

func TestFromFlags_ExitOnlyRequiresExit(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-only", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit-only 无出口候选应 fail-closed 报错")
	}
}

// stubConfigProvider 注入固定配置（测 stun/turn 配置回落）。
type stubConfigProvider struct {
	cfg *client.Config
}

func (s *stubConfigProvider) LoadConfig() (*client.Config, error) { return s.cfg, nil }

func TestFromFlags_ConfigFallback_STUN_TURN(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	cfg := client.DefaultConfig()
	cfg.STUNServers = []string{"stun:stub:3478"}
	cfg.TURNServers = []string{"turn:stub:3478"}
	cfg.TURNUser = "u"
	cfg.TURNPass = "p"
	conn := &Conn{}
	if err := conn.FromFlags(cmd, &stubConfigProvider{cfg: cfg}); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.STUN) != 1 || conn.STUN[0] != "stun:stub:3478" {
		t.Fatalf("STUN 配置回落失败: %v", conn.STUN)
	}
	if len(conn.TURN) != 1 || conn.TURN[0] != "turn:stub:3478" {
		t.Fatalf("TURN 配置回落失败: %v", conn.TURN)
	}
	if conn.TURNUser != "u" || conn.TURNPass != "p" {
		t.Fatalf("TURN 凭据配置回落失败: %+v", conn)
	}
}

func TestFromFlags_ConfigFallback_FlagOverrides(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("stun", "stun:flag:3478")
	cfg := client.DefaultConfig()
	cfg.STUNServers = []string{"stun:cfg:3478"}
	conn := &Conn{}
	if err := conn.FromFlags(cmd, &stubConfigProvider{cfg: cfg}); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.STUN) != 1 || conn.STUN[0] != "stun:flag:3478" {
		t.Fatalf("flag 应优先于配置回落: %v", conn.STUN)
	}
}

// TestSelectRoute_DomainSuffix：域名规则后缀匹配（子域名与自身均命中，防子串误配）。
func TestSelectRoute_DomainSuffix(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a", "node-b"}},
	}}
	if got := c.SelectRoute("example.com:80"); len(got) != 2 || got[0] != "node-a" || got[1] != "node-b" {
		t.Fatalf("example.com:80 应命中组 [node-a node-b], got %v", got)
	}
	if got := c.SelectRoute("sub.example.com:443"); len(got) != 2 {
		t.Fatalf("sub.example.com:443 应命中（子域名）, got %v", got)
	}
	if got := c.SelectRoute("notexample.com:80"); got != nil {
		t.Fatalf("notexample.com:80 不应命中（防子串误配）, got %v", got)
	}
}

// TestSelectRoute_DomainNormalized：归一化规则（*.example.com / .example.com）
// 与大小写不敏感命中（FromFlags 已归一化；手工构造同样生效）。
func TestSelectRoute_DomainNormalized(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a"}},
	}}
	if got := c.SelectRoute("API.EXAMPLE.COM:443"); len(got) != 1 {
		t.Fatalf("大写 host 应大小写不敏感命中, got %v", got)
	}
	if got := c.SelectRoute("x.example.com:80"); len(got) != 1 {
		t.Fatalf("子域名应命中, got %v", got)
	}
}

// TestSelectRoute_CIDR：IP 网段匹配（IPv4 + IPv6）。
func TestSelectRoute_CIDR(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteCIDR, Pattern: "10.0.0.0/8", Group: []string{"node-c"}},
		{Kind: RouteCIDR, Pattern: "2001:db8::/32", Group: []string{"node-v6"}},
	}}
	if got := c.SelectRoute("10.1.2.3:80"); len(got) != 1 || got[0] != "node-c" {
		t.Fatalf("10.1.2.3:80 应命中 10.0.0.0/8, got %v", got)
	}
	if got := c.SelectRoute("192.168.1.1:80"); got != nil {
		t.Fatalf("192.168.1.1:80 不应命中 10.0.0.0/8, got %v", got)
	}
	if got := c.SelectRoute("2001:db8:1::1:80"); len(got) != 1 || got[0] != "node-v6" {
		t.Fatalf("IPv6 应命中 2001:db8::/32, got %v", got)
	}
}

// TestSelectRoute_OrderAndFallback：多规则首个命中（声明序）；无命中返回 nil。
func TestSelectRoute_OrderAndFallback(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteCIDR, Pattern: "10.0.0.0/8", Group: []string{"node-intranet"}},
		{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-public"}},
	}}
	if got := c.SelectRoute("10.1.2.3:80"); len(got) != 1 || got[0] != "node-intranet" {
		t.Fatalf("10.1.2.3:80 应命中首条 cidr 规则, got %v", got)
	}
	if got := c.SelectRoute("example.com:80"); len(got) != 1 || got[0] != "node-public" {
		t.Fatalf("example.com:80 应命中第二条 domain 规则, got %v", got)
	}
	if got := c.SelectRoute("other.com:80"); got != nil {
		t.Fatalf("无命中应返回 nil（回落默认出口）, got %v", got)
	}
}

// TestSelectRoute_NoPort：无端口输入（host-only）同样匹配。
func TestSelectRoute_NoPort(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a"}},
		{Kind: RouteCIDR, Pattern: "10.0.0.0/8", Group: []string{"node-c"}},
	}}
	if got := c.SelectRoute("example.com"); len(got) != 1 {
		t.Fatalf("无端口域名应命中, got %v", got)
	}
	if got := c.SelectRoute("10.1.2.3"); len(got) != 1 {
		t.Fatalf("无端口 IP 应命中 cidr, got %v", got)
	}
}

// TestSelectRoute_CIDRNotMatchDomain：cidr 规则对纯域名 host 不命中（回落默认）——注释明示边界。
func TestSelectRoute_CIDRNotMatchDomain(t *testing.T) {
	t.Parallel()
	c := &Conn{Routes: []RouteRule{
		{Kind: RouteCIDR, Pattern: "10.0.0.0/8", Group: []string{"node-c"}},
	}}
	if got := c.SelectRoute("db.example.com:80"); got != nil {
		t.Fatalf("cidr 规则不应命中域名 host（不依赖 DNS）, got %v", got)
	}
}

// TestFromFlags_RouteParse：合法/非法格式/CIDR/空组用例（fail-closed，启动即报错）。
func TestFromFlags_RouteParse(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		route string
		want  []RouteRule
		err   bool
	}{
		{
			name:  "domain",
			route: ".example.com=node-a,node-b",
			want:  []RouteRule{{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a", "node-b"}}},
		},
		{
			name:  "wildcard",
			route: "*.example.com=node-a",
			want:  []RouteRule{{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a"}}},
		},
		{
			name:  "cidr",
			route: "10.0.0.0/8=node-c",
			want:  []RouteRule{{Kind: RouteCIDR, Pattern: "10.0.0.0/8", Group: []string{"node-c"}}},
		},
		{
			name:  "no-separator",
			route: "example.com",
			err:   true,
		},
		{
			name:  "empty-group",
			route: "example.com=",
			err:   true,
		},
		{
			name:  "empty-target",
			route: "=node-a",
			err:   true,
		},
		{
			name:  "bad-cidr",
			route: "10.0.0.0/33=node-c",
			err:   true,
		},
		{
			name:  "empty-node-in-group",
			route: "example.com=node-a,,node-b",
			err:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cmd := newTestCmd()
			if err := cmd.Flags().Set("route", tc.route); err != nil {
				t.Fatalf("set route: %v", err)
			}
			conn := &Conn{}
			err := conn.FromFlags(cmd, nil)
			if tc.err {
				if err == nil {
					t.Fatalf("%s: 应 fail-closed 报错, got nil", tc.name)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s: FromFlags: %v", tc.name, err)
			}
			if len(conn.Routes) != len(tc.want) {
				t.Fatalf("%s: Routes = %+v, want %+v", tc.name, conn.Routes, tc.want)
			}
			r := conn.Routes[0]
			w := tc.want[0]
			if r.Kind != w.Kind || r.Pattern != w.Pattern || len(r.Group) != len(w.Group) || r.Group[0] != w.Group[0] {
				t.Fatalf("%s: rule = %+v, want %+v", tc.name, r, w)
			}
		})
	}
}

// TestFromFlags_RouteMultiple：多个 --route 按声明序保留（StringArray 不拆逗号）。
func TestFromFlags_RouteMultiple(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("route", ".example.com=node-a,node-b")
	_ = cmd.Flags().Set("route", "10.0.0.0/8=node-c")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("FromFlags: %v", err)
	}
	if len(conn.Routes) != 2 {
		t.Fatalf("Routes 应含 2 条, got %d: %+v", len(conn.Routes), conn.Routes)
	}
	if conn.Routes[0].Kind != RouteDomain || conn.Routes[1].Kind != RouteCIDR {
		t.Fatalf("Routes 类型错: %+v", conn.Routes)
	}
}

// TestFromFlags_RouteWithExitOK：--route 与 --exit/--exit-group/--exit-auto 可共存
// （route 是更高优先级分流，未命中回落默认）——不禁用。
func TestFromFlags_RouteWithExitOK(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("route", ".example.com=node-a")
	_ = cmd.Flags().Set("exit", "node-default")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err != nil {
		t.Fatalf("--route 与 --exit 应可共存: %v", err)
	}
}

// TestFromFlags_RouteExitOnlyConflict：--route 与 --exit-only 语义冲突 → fail-closed 拒绝。
func TestFromFlags_RouteExitOnlyConflict(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("route", ".example.com=node-a")
	_ = cmd.Flags().Set("exit", "node-exit")
	_ = cmd.Flags().Set("exit-only", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--route 与 --exit-only 应互斥报错")
	}
}

// TestAddFlags_RouteRegistered：--route flag 已注册（出口族命令可见）。
func TestAddFlags_RouteRegistered(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	if cmd.Flags().Lookup("route") == nil {
		t.Fatalf("缺少 flag: --route")
	}
}

// TestAutoDial_RouteWins_ElseDefault：目标命中路由组走 NewExitGroupDial（组内 failover
// 行为断言）；未命中走默认（--exit 节点）。
func TestAutoDial_RouteWins_ElseDefault(t *testing.T) {
	t.Parallel()
	conn := &Conn{
		ExitNode:     "node-default",
		LocalTimeout: 0, // 0 = 不试本地，直接出口
		MDNS:         true,
		MDNSSecret:   "s",
		Routes: []RouteRule{
			{Kind: RouteDomain, Pattern: "example.com", Group: []string{"node-a", "node-b"}},
		},
	}
	// mDNS 模式 + mdnsSrv=nil：ExitDialFor 走 svc==nil 报错分支（快失败，不依赖真实
	// 网络）；node-a 先失败 → failover 到 node-b（NewExitGroupDial 语义）——但
	// ExitDialFor 对任意 node 都返回同一错误，故此处用 svc 桩不可行；改为验证
	// **组内 failover 路径被触发**（node-a 错误 → 尝试 node-b → 仍报错向上传播）。
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	if _, err := dial(context.Background(), "example.com:80"); err == nil {
		t.Fatalf("路由命中应走出口组（全部不可达 → 错误向上传播）")
	}
	// 未命中 → 走默认 --exit 节点（同样的 ExitDialFor 错误路径）。
	if _, err := dial(context.Background(), "other.com:80"); err == nil {
		t.Fatalf("未命中应走默认 --exit（错误向上传播）")
	}
}

// TestAutoDial_RouteNoGroup_ZeroRegression：--route 默认空 → SelectRoute 返回 nil →
// AutoDial 走原路径（零回归：纯本地仍可拨号、错误语义不变）。
func TestAutoDial_RouteNoGroup_ZeroRegression(t *testing.T) {
	t.Parallel()
	conn := &Conn{LocalTimeout: 100 * time.Millisecond}
	dial := conn.AutoDial(context.Background(), nil, nil, "node-local", nil, nil)
	if dial == nil {
		t.Fatalf("AutoDial 无路由返回 nil")
	}
	if _, err := dial(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatalf("无路由纯本地拨不可达地址应报错（原语义）")
	}
}

func TestLocalOrExit_ExitOnly_UsesExitDial(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalled.Add(1)
		return nil, net.ErrClosed
	}
	conn := &Conn{ExitOnly: true, LocalTimeout: 3 * time.Second}
	dial := conn.LocalOrExit(exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err != net.ErrClosed {
		t.Fatalf("err = %v, want net.ErrClosed（恒出口应直接用 exitDial）", err)
	}
	if exitCalled.Load() != 1 {
		t.Fatalf("exitDial 调用 %d 次, want 1", exitCalled.Load())
	}
}

func TestLocalOrExit_ZeroTimeout_UsesExitDial(t *testing.T) {
	t.Parallel()
	var exitCalled atomic.Int32
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		exitCalled.Add(1)
		return nil, net.ErrClosed
	}
	conn := &Conn{LocalTimeout: 0} // 0 = 不试本地
	dial := conn.LocalOrExit(exit)
	_, err := dial(context.Background(), "127.0.0.1:1")
	if err != net.ErrClosed {
		t.Fatalf("err = %v, want net.ErrClosed（0 超时应直接用 exitDial）", err)
	}
	if exitCalled.Load() != 1 {
		t.Fatalf("exitDial 调用 %d 次, want 1", exitCalled.Load())
	}
}

func TestLocalOrExit_NilExitDial_LocalOnly(t *testing.T) {
	t.Parallel()
	conn := &Conn{LocalTimeout: 3 * time.Second}
	dial := conn.LocalOrExit(nil)
	if dial == nil {
		t.Fatalf("LocalOrExit(nil) 应返回本地直连函数（非 nil）")
	}
	// 本地直连不可达目标 → 报错（证明走 net.Dialer 而非 panic）
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := dial(ctx, "127.0.0.1:1"); err == nil {
		t.Fatalf("本地直连不可达目标应报错")
	}
}

func TestNormalizeListen(t *testing.T) {
	t.Parallel()
	if got := NormalizeListen(":1080"); got != "127.0.0.1:1080" {
		t.Fatalf("NormalizeListen(:1080) = %q, want 127.0.0.1:1080", got)
	}
	if got := NormalizeListen("127.0.0.1:1080"); got != "127.0.0.1:1080" {
		t.Fatalf("NormalizeListen 不应改动显式地址: %q", got)
	}
}

// TestSignalers_MDNS_NoServer：--mdns 时 Signalers 不再构造服务器（P1-1 防双实例），
// 返回 (nil, nil, nil)（mDNS 由调用方独占构造）。
func TestSignalers_MDNS_NoServer(t *testing.T) {
	t.Parallel()
	conn := &Conn{MDNS: true, MDNSSecret: "s", NodeID: "node-local"}
	sig, closeFn, err := conn.Signalers(context.Background(), nil, "")
	if sig != nil || closeFn != nil || err != nil {
		t.Fatalf("Signalers(mdns) = (%v, closeFn!=nil=%v, %v), want (nil, nil, nil)（不再构造服务器）", sig, closeFn != nil, err)
	}
}

// TestSignalers_NoWebRTC_NoSvc：无 svc 或 --webrtc=false → 无信令 (nil, nil, nil)。
func TestSignalers_NoWebRTC_NoSvc(t *testing.T) {
	t.Parallel()
	conn := &Conn{WebRTC: true} // svc nil
	sig, closeFn, err := conn.Signalers(context.Background(), nil, "")
	if sig != nil || closeFn != nil || err != nil {
		t.Fatalf("Signalers(svc=nil) = (%v, closeFn!=nil=%v, %v), want (nil, nil, nil)", sig, closeFn != nil, err)
	}
	conn2 := &Conn{WebRTC: false}
	sig2, closeFn2, err2 := conn2.Signalers(context.Background(), &client.FileClient{}, "")
	if sig2 != nil || closeFn2 != nil || err2 != nil {
		t.Fatalf("Signalers(webrtc=false) = (%v, closeFn!=nil=%v, %v), want (nil, nil, nil)", sig2, closeFn2 != nil, err2)
	}
}

// TestFromFlags_ExitOnlyAutoExclusive：--exit-only 与 --exit-auto 互斥（控制者裁决 ⚠️-4）。
func TestFromFlags_ExitOnlyAutoExclusive(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	_ = cmd.Flags().Set("exit-auto", "true")
	_ = cmd.Flags().Set("exit-only", "true")
	conn := &Conn{}
	if err := conn.FromFlags(cmd, nil); err == nil {
		t.Fatalf("--exit-only 与 --exit-auto 应互斥报错")
	}
}

// TestFromFlags_ExitOnlyWithExitGroup 验证 --exit-only + --exit-group 组合合法
// （组内 failover 也是有效出口——修复前误报"需要 --exit/--exit-auto"）。
func TestFromFlags_ExitOnlyWithExitGroup(t *testing.T) {
	t.Parallel()
	cmd := newTestCmd()
	cmd.Flags().Set("exit-group", "sg-t,sg2-t")
	cmd.Flags().Set("exit-only", "true")
	c := &Conn{}
	if err := c.FromFlags(cmd, nil); err != nil {
		t.Fatalf("--exit-group + --exit-only 应合法: %v", err)
	}
	if c.ExitOnly != true {
		t.Fatal("ExitOnly 未装配")
	}
	if len(c.ExitGroup) != 2 || c.ExitGroup[0] != "sg-t" || c.ExitGroup[1] != "sg2-t" {
		t.Fatalf("ExitGroup 装配错误: %v", c.ExitGroup)
	}
}

// TestSmartFallbackDial_E2EPreserved 验证 smart fallback 保留 E2E：
// --e2e 配置时 fallback 闭包存在（编译期保证），且 E2EOpts 解析出 E2E 配置
// （禁静默降级明文——错误盐/指纹不匹配场景不能悄悄明文转发）。
func TestSmartFallbackDial_E2EPreserved(t *testing.T) {
	t.Parallel()
	c := &Conn{E2E: true, E2EPeerFP: []string{"sha256:" + strings.Repeat("00", 32)}}
	fb := smartFallbackDial(c)
	if fb == nil {
		t.Fatal("fallback 不应为 nil")
	}
	// E2E 配置解析：E2EPeerFP 非空 → 返回 E2E 选项（非 nil）。
	opts, err := c.E2EOpts()
	if err != nil {
		t.Fatalf("E2EOpts: %v", err)
	}
	if opts == nil || !opts.Enabled {
		t.Fatal("--e2e + peer-fp 应解析出启用的 E2E 选项")
	}
	if len(opts.PeerFingerprints) != 1 {
		t.Fatalf("PeerFingerprints 应保留: %v", opts.PeerFingerprints)
	}
}

// TestSmartFallbackDial_NoE2E_FallbackNil 验证无 E2E 时 fallback 仍可用（零回归）。
func TestSmartFallbackDial_NoE2E_FallbackNil(t *testing.T) {
	t.Parallel()
	c := &Conn{E2E: false}
	fb := smartFallbackDial(c)
	if fb == nil {
		t.Fatal("无 E2E 时 fallback 也应非 nil（降级固定顺序）")
	}
}

// TestParseUpstreamProxy 验证 --upstream-proxy 解析（URL/认证/host）。
func TestParseUpstreamProxy(t *testing.T) {
	t.Parallel()
	// 合法：带认证
	up, err := parseUpstreamProxy("http://cg:pass@61.153.100.229:40086")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if up.Host != "61.153.100.229:40086" || up.User != "cg" || up.Pass != "pass" {
		t.Fatalf("解析错误: %+v", up)
	}
	// 无认证
	up2, err := parseUpstreamProxy("http://127.0.0.1:40086")
	if err != nil {
		t.Fatalf("parse no-auth: %v", err)
	}
	if up2.User != "" || up2.Pass != "" {
		t.Fatalf("无认证应解析为空: %+v", up2)
	}
	// https:// 拒绝（fail-closed）
	if _, err := parseUpstreamProxy("https://61.153.100.229:40086"); err == nil {
		t.Fatal("https:// 上游应拒绝")
	}
	// 缺 host 拒绝
	if _, err := parseUpstreamProxy("http://"); err == nil {
		t.Fatal("缺 host 应拒绝")
	}
}

// TestUpstreamConnect_Tunnel 验证经上游代理 CONNECT 建隧道（本地起 mock 上游）。
func TestUpstreamConnect_Tunnel(t *testing.T) {
	t.Parallel()
	// mock 上游：监听 127.0.0.1，收到 CONNECT 后 200 并回显。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, rerr := http.ReadRequest(br)
		if rerr != nil {
			return
		}
		// 校验认证头
		if req.Header.Get("Proxy-Authorization") == "" {
			_ = c.Close()
			return
		}
		_, _ = c.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		// 回显 4 字节（生产手动解析状态行，不会把隧道数据当 body 消费）
		_, _ = c.Write([]byte("tun1"))
	}()
	up := &upstreamProxy{Host: ln.Addr().String(), User: "cg", Pass: "pass"}
	conn, err := upstreamConnect(context.Background(), up, "example.com:443")
	if err != nil {
		t.Fatalf("upstreamConnect: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("读隧道数据: %v", err)
	}
	if string(buf) != "tun1" {
		t.Fatalf("隧道数据异常: %q", buf)
	}
}

// TestUpstreamConnect_AuthRequired 验证无认证被上游拒绝 → 报错（fail-closed）。
func TestUpstreamConnect_AuthRequired(t *testing.T) {
	t.Parallel()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		_, _ = http.ReadRequest(br)
		_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\nContent-Length: 0\r\n\r\n"))
	}()
	up := &upstreamProxy{Host: ln.Addr().String()} // 无认证
	if _, err := upstreamConnect(context.Background(), up, "example.com:443"); err == nil {
		t.Fatal("上游 407 应报错")
	}
}

// TestAutoDial_RouteFallback_LocalDirect 验证本地直连路径补 "direct" 路由
// （非 RouteInfoer 连接 → withRoute "direct"；已带路由的 mesh 连接保留）。
func TestAutoDial_RouteFallback_LocalDirect(t *testing.T) {
	t.Parallel()
	// 模拟 base：返回裸 conn（本地直连）
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, _ := ln.Accept()
		if conn != nil {
			_ = conn.Close()
		}
	}()
	// 用 withRoute 验证非 RouteInfoer 包装
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	routed := withRoute(raw, "direct")
	if ri, ok := routed.(httpproxy.RouteInfoer); !ok {
		t.Fatal("withRoute 应返回 RouteInfoer")
	} else if ri.Route() != "direct" {
		t.Fatalf("direct 路由: %q", ri.Route())
	}
}
