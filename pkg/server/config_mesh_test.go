// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_mesh_test.go 钉住服务端 **A 侧 mesh 客户端配置**（`mesh` 段；Y 二期）的启动期校验与默认值。
//
// 与前几片的呼应：`sync_remotes[].kind=mesh` 决定「有哪些 mesh 远端」；本段决定「A 侧怎么接触 hub
// 与信令」——**hub 可以是远端**（不必是本机），这是本期明确要支持的能力。

import (
	"strings"
	"testing"
)

// meshCfgFixture 构造「一条 kind=mesh 的 sync 远端」+ 可选的 mesh 段配置。
func meshCfgFixture(t *testing.T, mutate func(*Config)) *Config {
	t.Helper()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.SyncRemotes = []SyncRemoteConfig{{
		Name: "r-mesh", Kind: "mesh", Node: "nodeB", Volume: "main",
		PeerPins: []string{"sha256:" + strings.Repeat("a", 64)},
	}}
	if mutate != nil {
		mutate(cfg)
	}
	cfg.SetDefaults()
	return cfg
}

func TestMeshConfig_Validate(t *testing.T) {
	const sk = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // 空 = 期望通过
	}{
		{"未配 mesh 段（回落到本机 hub）通过", nil, ""},
		{"远端 hub 完整凭据通过", func(c *Config) {
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.NodeID = "nodeA"
			c.Mesh.AccessKey, c.Mesh.AccessKeySecret, c.Mesh.SkeyID = "ak-x", sk, "skey-0123456789ab"
		}, ""},
		{"远端 hub 但缺凭据拒绝", func(c *Config) {
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.NodeID = "nodeA"
		}, "access_key"},
		{"远端 hub 缺 skey_id 拒绝", func(c *Config) {
			c.Mesh.HubURL = "https://hub.example.com:18083"
			c.Mesh.AccessKey, c.Mesh.AccessKeySecret = "ak-x", sk
		}, "skey_id"},
		{"远端 hub URL 非法拒绝", func(c *Config) {
			c.Mesh.HubURL = "not-a-url"
			c.Mesh.AccessKey, c.Mesh.AccessKeySecret, c.Mesh.SkeyID = "ak-x", sk, "skey-0123456789ab"
		}, "hub_url"},
		{"远端 hub 非 http(s) scheme 拒绝", func(c *Config) {
			c.Mesh.HubURL = "ftp://hub.example.com"
			c.Mesh.AccessKey, c.Mesh.AccessKeySecret, c.Mesh.SkeyID = "ak-x", sk, "skey-0123456789ab"
		}, "hub_url"},
		{"transport=webrtc 但缺 node_id 拒绝", func(c *Config) {
			c.SyncRemotes[0].Transport = "webrtc"
		}, "node_id"},
		{"transport=webrtc 且有 node_id 通过", func(c *Config) {
			c.SyncRemotes[0].Transport = "webrtc"
			c.Mesh.NodeID = "nodeA"
		}, ""},
		{"transport=auto 无 node_id 通过（退化为纯中继）", func(c *Config) {
			c.SyncRemotes[0].Transport = "auto"
		}, ""},
		{"transport=relay 无 node_id 通过", func(c *Config) {
			c.SyncRemotes[0].Transport = "relay"
		}, ""},
		{"无 mesh 远端时的 mesh 段配置不额外校验", func(c *Config) {
			c.SyncRemotes = nil
			c.Mesh.HubURL = "https://hub.example.com:18083" // 缺凭据：但没人用 mesh ⇒ 不校验
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := meshCfgFixture(t, tc.mutate)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应被拒绝（错误需提及 %q）", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应提及 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestMeshConfig_Defaults 钉住默认值：mesh 段全零 = 「用本机 hub、不打洞」（零回归：
// 旧配置不写 mesh 段，而 kind=mesh 远端的 transport 缺省 auto ⇒ 退化为纯中继，与此前一致）。
func TestMeshConfig_Defaults(t *testing.T) {
	cfg := Default()
	if cfg.Mesh.HubURL != "" || cfg.Mesh.NodeID != "" {
		t.Fatalf("mesh 段应默认全空, got %+v", cfg.Mesh)
	}
	if cfg.Mesh.InsecureTLS {
		t.Fatal("insecure_tls 应默认 false")
	}
	if len(cfg.Mesh.STUN) != 0 || len(cfg.Mesh.TURN) != 0 {
		t.Fatal("实例级 ICE 默认应为空（空 = 不启用实例配置）")
	}
}
