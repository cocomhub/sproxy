// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/netip"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/spf13/cobra"
)

// mockCfgProvider 是 ConfigProvider 测试桩。
type mockCfgProvider struct {
	cfg *client.Config
	err error
}

func (m *mockCfgProvider) LoadConfig() (*client.Config, error) {
	return m.cfg, m.err
}

// TestP2PFlags_ApplyConfigFallback（P2-配置3）：
// 未显式指定的 hub/node-id 从配置回落；显式指定的不被覆盖。
func TestP2PFlags_ApplyConfigFallback(t *testing.T) {
	provider := &mockCfgProvider{cfg: &client.Config{
		HubURL: "wss://hub.example.com/ws", AccessKey: "ak", AccessKeySecret: "at", NodeID: "node-x",
	}}

	var f p2pFlags
	f.applyConfigFallback(provider)
	if f.hub != "wss://hub.example.com/ws" || f.node != "node-x" {
		t.Fatalf("配置回落未生效: %+v", f)
	}

	// 显式指定的 flag 不被覆盖
	explicit := p2pFlags{hub: "ws://explicit"}
	explicit.applyConfigFallback(provider)
	if explicit.hub != "ws://explicit" || explicit.node != "node-x" {
		t.Fatalf("显式 flag 被配置覆盖: %+v", explicit)
	}

	// nil provider 是 no-op
	empty := p2pFlags{}
	empty.applyConfigFallback(nil)
	if empty.hub != "" {
		t.Fatal("nil provider 不应填充")
	}
}

func TestNewCmdP2P_Subcommands(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	if cmd.Use != "p2p" {
		t.Fatalf("expected Use 'p2p', got %q", cmd.Use)
	}
	subs := map[string]bool{"connect": false, "listen": false}
	for _, c := range cmd.Commands() {
		if _, ok := subs[c.Name()]; ok {
			subs[c.Name()] = true
		}
	}
	for name, found := range subs {
		if !found {
			t.Errorf("missing subcommand: %s", name)
		}
	}
}

func TestNewCmdP2PConnect_Flags(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	connect := cmd.Commands()[0]
	for _, name := range []string{"peer", "tcp", "listen", "hub", "node-id"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("p2p connect 缺少 flag: %s", name)
		}
	}
}

func TestNewCmdP2PListen_Flags(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	listen := cmd.Commands()[1]
	for _, name := range []string{"service", "dial-allow-cidr", "hub", "node-id"} {
		if f := listen.Flags().Lookup(name); f == nil {
			t.Errorf("p2p listen 缺少 flag: %s", name)
		}
	}
}

// TestNewCmdP2PConnect_EmptyHubError 验证非 manual 模式 --hub 为空时前置报错（S64），
// 不再把晦涩的 unsupported protocol scheme 留到信令 post/poll 阶段。
func TestNewCmdP2PConnect_EmptyHubError(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"connect", "--peer", "peerA", "--tcp", "127.0.0.1:22"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --hub is empty")
	}
	if !strings.Contains(err.Error(), "--hub 不能为空") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNewCmdP2PConnect_ManualSameOfferAnswer 验证 manual 文件模式 --offer 与 --answer
// 同路径时前置拒绝（S67）。
func TestNewCmdP2PConnect_ManualSameOfferAnswer(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"connect", "--peer", "peerA", "--tcp", "127.0.0.1:22",
		"--manual", "--offer", "same.sdp", "--answer", "same.sdp"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --offer == --answer")
	}
	if !strings.Contains(err.Error(), "不能指向同一路径") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestNewCmdP2PListen_ManualSameOfferAnswer 验证 listen 侧同样前置拒绝同路径（S67）。
func TestNewCmdP2PListen_ManualSameOfferAnswer(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	cmd.SetArgs([]string{"listen", "--manual", "--offer", "same.sdp", "--answer", "same.sdp"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --offer == --answer")
	}
	if !strings.Contains(err.Error(), "不能指向同一路径") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestBuildP2PServeOpts_EmptyNil 验证无放行配置（或全部无效）时返回 nil，
// Serve 回落默认 DialAllowed（仅公网）。
func TestBuildP2PServeOpts_EmptyNil(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	if opts := buildP2PServeOpts(nil, nil, netip.Addr{}, netip.MustParsePrefix("100.64.0.0/10"), ios); opts != nil {
		t.Fatalf("无配置应返回 nil: %v", opts)
	}
	if opts := buildP2PServeOpts([]string{"invalid-no-colon"}, nil, netip.Addr{}, netip.MustParsePrefix("100.64.0.0/10"), ios); opts != nil {
		t.Fatalf("全部无效服务应返回 nil: %v", opts)
	}
}

// TestBuildP2PServeOpts_ServiceAllowsExact 验证 --service 宣告地址精确放行、
// 未宣告的 loopback 拒绝、公网目标回落放行（I45）。
func TestBuildP2PServeOpts_ServiceAllowsExact(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	opts := buildP2PServeOpts([]string{"ssh:127.0.0.1:22"}, nil, netip.Addr{}, netip.MustParsePrefix("100.64.0.0/10"), ios)
	if len(opts) != 1 || opts[0].DialPolicy == nil {
		t.Fatalf("应构造带 DialPolicy 的 ServeOptions: %v", opts)
	}
	resolved, ok := opts[0].DialPolicy("127.0.0.1:22")
	if !ok || resolved != "127.0.0.1:22" {
		t.Fatalf("宣告地址应精确放行: resolved=%q ok=%v", resolved, ok)
	}
	if _, ok := opts[0].DialPolicy("127.0.0.1:9999"); ok {
		t.Fatal("未宣告的 loopback 不应放行")
	}
	if _, ok := opts[0].DialPolicy("8.8.8.8:53"); !ok {
		t.Fatal("公网目标应放行")
	}
}

// TestBuildP2PServeOpts_CIDRAllowsPrivate 验证 --dial-allow-cidr 放行命中网段的
// 私网地址、未命中网段仍拒绝（I45）。
func TestBuildP2PServeOpts_CIDRAllowsPrivate(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	opts := buildP2PServeOpts(nil, []string{"192.168.0.0/16"}, netip.Addr{}, netip.MustParsePrefix("100.64.0.0/10"), ios)
	if len(opts) != 1 || opts[0].DialPolicy == nil {
		t.Fatalf("CIDR 应构造 DialPolicy: %v", opts)
	}
	if _, ok := opts[0].DialPolicy("192.168.1.10:22"); !ok {
		t.Fatal("命中白名单 CIDR 的私网地址应放行")
	}
	if _, ok := opts[0].DialPolicy("10.0.0.5:22"); ok {
		t.Fatal("未命中 CIDR 的私网地址应拒绝")
	}
}

// TestParseVIPSubnetFlag（S-1 回归）校验 --virtual-subnet 解析：默认 CGNAT、自定义
// 合法 IPv4、非法/非 IPv4 回落默认。
func TestParseVIPSubnetFlag(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	newCmd := func() *cobra.Command {
		cmd := &cobra.Command{}
		cmd.Flags().String("virtual-subnet", hub.DefaultVirtualSubnet, "")
		return cmd
	}
	// 默认。
	if p := parseVIPSubnetFlag(newCmd(), ios); p != netip.MustParsePrefix("100.64.0.0/10") {
		t.Fatalf("默认子网 = %v, want 100.64.0.0/10", p)
	}
	// 自定义合法。
	cmd := newCmd()
	_ = cmd.Flags().Set("virtual-subnet", "10.0.0.0/8")
	if p := parseVIPSubnetFlag(cmd, ios); p != netip.MustParsePrefix("10.0.0.0/8") {
		t.Fatalf("自定义子网 = %v, want 10.0.0.0/8", p)
	}
	// 非法回落默认。
	cmd = newCmd()
	_ = cmd.Flags().Set("virtual-subnet", "not-a-cidr")
	if p := parseVIPSubnetFlag(cmd, ios); p != netip.MustParsePrefix("100.64.0.0/10") {
		t.Fatalf("非法子网应回落默认, got %v", p)
	}
}

// TestNewCmdP2PListen_VirtualSubnetFlag 校验 p2p listen 提供 --virtual-subnet flag
// （默认 CGNAT），供自定义 hub.virtual_subnet 出口装配（S-1 回归）。
func TestNewCmdP2PListen_VirtualSubnetFlag(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	listen := cmd.Commands()[0] // p2p connect
	_ = listen
	var listenCmd *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Name() == "listen" {
			listenCmd = c
			break
		}
	}
	if listenCmd == nil {
		t.Fatal("p2p listen 子命令不存在")
	}
	got := listenCmd.Flags().Lookup("virtual-subnet")
	if got == nil {
		t.Fatal("p2p listen 应提供 --virtual-subnet flag（S-1）")
	}
	if got.DefValue != hub.DefaultVirtualSubnet {
		t.Fatalf("--virtual-subnet 默认值 = %q, want %q", got.DefValue, hub.DefaultVirtualSubnet)
	}
}
