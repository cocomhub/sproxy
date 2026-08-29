// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
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
// 未显式指定的 hub/token/relay-token/node-id 从配置回落；显式指定的不被覆盖。
func TestP2PFlags_ApplyConfigFallback(t *testing.T) {
	provider := &mockCfgProvider{cfg: &client.Config{
		HubURL: "wss://hub.example.com/ws", AuthToken: "at", RelayToken: "rt", NodeID: "node-x",
	}}

	var f p2pFlags
	f.applyConfigFallback(provider)
	if f.hub != "wss://hub.example.com/ws" || f.tok != "at" || f.relayTok != "rt" || f.node != "node-x" {
		t.Fatalf("配置回落未生效: %+v", f)
	}

	// 显式指定的 flag 不被覆盖
	explicit := p2pFlags{hub: "ws://explicit", tok: "explicit-tok"}
	explicit.applyConfigFallback(provider)
	if explicit.hub != "ws://explicit" || explicit.tok != "explicit-tok" || explicit.relayTok != "rt" || explicit.node != "node-x" {
		t.Fatalf("显式 flag 被配置覆盖: %+v", explicit)
	}

	// nil provider 是 no-op
	empty := p2pFlags{}
	empty.applyConfigFallback(nil)
	if empty.hub != "" || empty.tok != "" {
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
	for _, name := range []string{"peer", "tcp", "listen", "hub", "token", "relay-token", "node-id"} {
		if f := connect.Flags().Lookup(name); f == nil {
			t.Errorf("p2p connect 缺少 flag: %s", name)
		}
	}
}

func TestNewCmdP2PListen_Flags(t *testing.T) {
	cmd := NewCmdP2P(cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	listen := cmd.Commands()[1]
	for _, name := range []string{"service", "dial-allow-cidr", "hub", "token", "relay-token", "node-id"} {
		if f := listen.Flags().Lookup(name); f == nil {
			t.Errorf("p2p listen 缺少 flag: %s", name)
		}
	}
}

// TestP2PFlagsRelayToken 验证 p2p 自动注册 relay_token 选择（B17）：
// --relay-token 优先，否则回落 --token（对齐 mesh 的 meshRelayToken fallback 链）。
func TestP2PFlagsRelayToken(t *testing.T) {
	f := &p2pFlags{}
	// 显式 --relay-token 优先
	f.relayTok = "relay-token"
	f.tok = "signal-token"
	if got := f.relayToken(); got != "relay-token" {
		t.Fatalf("relayToken() with relay-token set = %q, want relay-token", got)
	}
	// 空 relay → 回落 --token
	f.relayTok = ""
	if got := f.relayToken(); got != "signal-token" {
		t.Fatalf("relayToken() fallback = %q, want signal-token", got)
	}
	// 两者皆空 → 空串
	f.tok = ""
	if got := f.relayToken(); got != "" {
		t.Fatalf("relayToken() both empty = %q, want empty", got)
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
	if opts := buildP2PServeOpts(nil, nil, ios); opts != nil {
		t.Fatalf("无配置应返回 nil: %v", opts)
	}
	if opts := buildP2PServeOpts([]string{"invalid-no-colon"}, nil, ios); opts != nil {
		t.Fatalf("全部无效服务应返回 nil: %v", opts)
	}
}

// TestBuildP2PServeOpts_ServiceAllowsExact 验证 --service 宣告地址精确放行、
// 未宣告的 loopback 拒绝、公网目标回落放行（I45）。
func TestBuildP2PServeOpts_ServiceAllowsExact(t *testing.T) {
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}
	opts := buildP2PServeOpts([]string{"ssh:127.0.0.1:22"}, nil, ios)
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
	opts := buildP2PServeOpts(nil, []string{"192.168.0.0/16"}, ios)
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
