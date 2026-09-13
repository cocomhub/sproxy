// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// config_mesh_node_test.go 钉住 **B 侧 mesh node 角色**（`mesh.node` 段；S5）的启动期校验。
//
// 背景：B 侧要把本机 `remote_read`/`remote_write` 面宣告到 mesh 并允许出口拨号，此前依赖外部
// sidecar（`sclient mesh node --service volread:… --service volwrite:… --dial-allow`）。本片让
// sproxy 进程内可选地承担该角色；本文件只钉**配置校验**（装配与 RunNode 调用在 cmd/sproxy 侧钉）。

import (
	"strings"
	"testing"
)

// meshNodeCfgFixture 构造「只读面启用 + kind=mesh 远端」的可校验配置（mesh node 角色的典型场景）。
func meshNodeCfgFixture(t *testing.T, mutate func(*Config)) *Config {
	t.Helper()
	// 先建**基线**（只读面启用 + ACL 有 pin），再应用用例的 mutate——顺序很重要：mutate 最后跑，
	// 否则基线会覆盖用例想关闭的开关（本文件第一版就踩了这个坑）。
	cfg := meshCfgFixture(t, nil)
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: cfg.StorageRoot, ACL: &VolumeACLConfig{
		Mode:   VolumeACLAllow,
		Owners: []string{"alice"},
		MeshReaders: []VolumeMeshReaderConfig{{
			Node: "nodeA", Fingerprint: testReaderFP, Owner: "alice", Scope: "read",
		}},
	}}}
	cfg.RemoteRead.Enabled = true
	cfg.RemoteRead.Listen = "127.0.0.1:19000"
	cfg.SetDefaults()
	if mutate != nil {
		mutate(cfg)
	}
	return cfg
}

func TestMeshNodeConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string // 空 = 期望通过
	}{
		{"未启用（默认）通过", func(c *Config) {}, ""},
		{"启用 + hub.node_id + 只读面宣告 通过", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
		}, ""},
		{"启用 + mesh.node_id 通过", func(c *Config) {
			c.Mesh.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
		}, ""},
		{"启用但无任何 node_id 拒绝", func(c *Config) {
			c.Mesh.Node.Enabled = true
		}, "node_id"},
		{"启用但没有可宣告的服务拒绝", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.RemoteRead.Enabled = false
		}, "服务"},
		{"启用 + 额外服务（无远近面）通过", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.RemoteRead.Enabled = false
			c.Mesh.Node.ExtraServices = []string{"ssh:127.0.0.1:22"}
		}, ""},
		{"启用 + 远端 hub（node.hub_url）缺凭据拒绝", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.Node.HubURL = "https://hub.example.com:18083"
		}, "access_key"},
		{"启用 + 远端 hub（mesh.hub_url）缺凭据拒绝", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.HubURL = "https://hub.example.com:18083"
		}, "access_key"},
		{"启用 + node.hub_url 非法拒绝", func(c *Config) {
			c.Hub.NodeID = "node-b"
			c.Mesh.Node.Enabled = true
			c.Mesh.Node.HubURL = "not-a-url"
		}, "hub_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := meshNodeCfgFixture(t, tc.mutate)
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

// TestMeshNodeConfig_Defaults 钉住默认关闭（不启用 = 与今天完全一致，零回归）。
func TestMeshNodeConfig_Defaults(t *testing.T) {
	cfg := Default()
	if cfg.Mesh.Node.Enabled || cfg.Mesh.Node.WebRTC || len(cfg.Mesh.Node.ExtraServices) != 0 {
		t.Fatalf("mesh.node 段应默认关闭且为空, got %+v", cfg.Mesh.Node)
	}
}
