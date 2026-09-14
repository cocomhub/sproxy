// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_node_test.go 钉住 **B 侧 mesh node 角色**（S5）在 `cmd/sproxy` 侧的构造：
//   - 服务宣告由**远近面监听地址自动派生**（volread/volwrite）+ extra_services 追加；
//   - `mesh.NodeConfig` 字段映射（node_id 回落链、DialAllow 恒开、WebRTC/Insecure 等）；
//   - `startMeshNodeRoleWithCreds` 在未启用时**不启动**、启用时把 RunNode 跑在后台并能随 ctx 收敛。

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// TestMeshNodeServiceDecls_DerivesFaces 钉住服务宣告派生：
// 只读面启用 ⇒ 宣告 volread:<readAddr>；写面启用 ⇒ 宣告 volwrite:<writeAddr>；
// extra_services 原样追加（顺序：面在前，额外在后，便于人读）。
func TestMeshNodeServiceDecls_DerivesFaces(t *testing.T) {
	cases := []struct {
		name      string
		readOn    bool
		writeOn   bool
		readAddr  string
		writeAddr string
		extra     []string
		wantDecls []string
	}{
		{name: "只读面", readOn: true, readAddr: "127.0.0.1:19000", wantDecls: []string{"volread:127.0.0.1:19000"}},
		{name: "读写两面", readOn: true, writeOn: true, readAddr: "127.0.0.1:19000", writeAddr: "127.0.0.1:19001",
			wantDecls: []string{"volread:127.0.0.1:19000", "volwrite:127.0.0.1:19001"}},
		{name: "只写面", writeOn: true, writeAddr: "127.0.0.1:19001", wantDecls: []string{"volwrite:127.0.0.1:19001"}},
		{name: "面未启用但额外服务", extra: []string{"ssh:127.0.0.1:22"}, wantDecls: []string{"ssh:127.0.0.1:22"}},
		{name: "面 + 额外服务", readOn: true, readAddr: "127.0.0.1:19000", extra: []string{"ssh:127.0.0.1:22"},
			wantDecls: []string{"volread:127.0.0.1:19000", "ssh:127.0.0.1:22"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := server.Default()
			cfg.RemoteRead.Enabled = tc.readOn
			cfg.RemoteWrite.Enabled = tc.writeOn
			cfg.Mesh.Node.ExtraServices = tc.extra

			got := meshNodeServiceDecls(cfg, tc.readAddr, tc.writeAddr)
			if len(got) != len(tc.wantDecls) {
				t.Fatalf("decls=%v want %v", got, tc.wantDecls)
			}
			for i := range got {
				if got[i] != tc.wantDecls[i] {
					t.Fatalf("decls[%d]=%q want %q（全部：%v）", i, got[i], tc.wantDecls[i], got)
				}
			}
		})
	}
}

// TestMeshNodeConfig_MapsFields 钉住 NodeConfig 字段映射（含 node_id 回落链与 DialAllow 恒开）。
func TestMeshNodeConfig_MapsFields(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.RemoteRead.Enabled = true // 只读面启用 ⇒ 自动宣告 volread:<readAddr>
	cfg.Mesh.Node.Enabled = true
	cfg.Mesh.Node.WebRTC = true
	cfg.Mesh.Node.Insecure = true
	cfg.Mesh.Node.DialAllowCIDRs = []string{"192.168.0.0/16"}
	cfg.Hub.NodeID = "from-hub" // node_id 回落链末位
	cfg.SetDefaults()

	creds := &meshHubCreds{AK: "ak-" + strings.Repeat("a", 32), SK: strings.Repeat("b", 64), SkeyID: "skey-0123456789ab"}
	nc, err := meshNodeConfig(cfg, "127.0.0.1:19000", "", creds, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if err != nil {
		t.Fatalf("meshNodeConfig: %v", err)
	}
	if nc.NodeID != "from-hub" {
		t.Fatalf("node_id 应回落到 hub.node_id, got %q", nc.NodeID)
	}
	if !nc.DialAllow {
		t.Fatal("DialAllow 必须恒开（否则对端 dial 帧会被出口策略拒绝）")
	}
	if !nc.EnableWebRTC {
		t.Fatal("EnableWebRTC 应随 mesh.node.webrtc")
	}
	if !nc.Insecure {
		t.Fatal("Insecure 应随 mesh.node.insecure")
	}
	if len(nc.DialAllowCIDRs) != 1 || nc.DialAllowCIDRs[0] != "192.168.0.0/16" {
		t.Fatalf("DialAllowCIDRs 不符: %v", nc.DialAllowCIDRs)
	}
	// 服务与精确放行地址必须一致（service 宣告的 loopback 地址要在 addrs 里，否则出口被拒）。
	if len(nc.Services) != 1 || nc.Services[0].Name != "volread" || nc.Services[0].Addr != "127.0.0.1:19000" {
		t.Fatalf("Services 不符: %+v", nc.Services)
	}
	if len(nc.ServiceAddrs) != 1 || nc.ServiceAddrs[0] != "127.0.0.1:19000" {
		t.Fatalf("ServiceAddrs 不符（应精确放行宣告地址）: %v", nc.ServiceAddrs)
	}
}

// TestMeshNodeConfig_MeshNodeIDPrefersNodeField 钉住 node_id 优先级：node_id → mesh.node_id → hub.node_id。
func TestMeshNodeConfig_MeshNodeIDPrefersNodeField(t *testing.T) {
	cfg := server.Default()
	cfg.Mesh.Node.NodeID = "from-node"
	cfg.Mesh.NodeID = "from-mesh"
	cfg.Hub.NodeID = "from-hub"
	if got := cfg.MeshNodeID(); got != "from-node" {
		t.Fatalf("MeshNodeID=%q want from-node", got)
	}
}

// TestStartMeshNodeRole_DisabledIsNoop 钉住默认不启用：返回 false 且不启动任何东西（零回归）。
func TestStartMeshNodeRole_DisabledIsNoop(t *testing.T) {
	cfg := server.Default()
	started := startMeshNodeRoleWithCreds(t.Context(), cfg, "127.0.0.1:19000", "", nil, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
	if started {
		t.Fatal("未启用时不应启动 mesh node 角色")
	}
}

// TestStartMeshNodeRole_EnabledStartsAndStopsOnCtx 钉住启用时启动后台 RunNode 且能随 ctx 收敛：
// node_id 非法/缺失时**不启动**并返回 false（fail-closed，不 panic）。
func TestStartMeshNodeRole_EnabledStartsAndStopsOnCtx(t *testing.T) {
	cfg := server.Default()
	cfg.StorageRoot = t.TempDir()
	cfg.RemoteRead.Enabled = true // 先保证「有可宣告的服务」，使失败原因只会是 node_id
	cfg.Mesh.Node.Enabled = true
	cfg.Mesh.Node.NodeID = "" // 故意不给 node_id
	cfg.Hub.NodeID = ""       // 回落链全空
	cfg.SetDefaults()
	creds := &meshHubCreds{AK: "ak-" + strings.Repeat("a", 32), SK: strings.Repeat("b", 64), SkeyID: "skey-0123456789ab"}
	if started := startMeshNodeRoleWithCreds(t.Context(), cfg, "127.0.0.1:19000", "", creds, loggerDiscard()); started {
		t.Fatal("缺 node_id 时不应启动（fail-closed）")
	}

	// 给上 node_id：应启动（后台 goroutine；用可取消 ctx 收敛）。
	cfg.Mesh.Node.NodeID = "node-b-self"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if started := startMeshNodeRoleWithCreds(ctx, cfg, "127.0.0.1:19000", "", creds, loggerDiscard()); !started {
		t.Fatal("前置齐备时应启动")
	}
	cancel()
	// 有意占位：RunNode 的 ctx 取消是异步的，此处只需「不 panic 地退出」；
	// 无终态事件可观测（残余清单登记项）。
	time.Sleep(50 * time.Millisecond)
}

// discardWriter 是丢弃输出（避免测试日志噪音）。
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// loggerDiscard 返回丢弃日志器。
func loggerDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(discardWriter{}, nil)) }

// 编译期：服务宣告类型是 hub.Service（与 RunNode 的入参一致）。
var _ = []hub.Service{}
var _ = strings.TrimSpace
