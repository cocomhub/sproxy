// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net/netip"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// TestMeshVIPDial_ResolvesVirtualAddr 校验虚拟 IP 目标经 vipTable 解析为节点 ID，
// 并把解析后的 target 传给 base dial。
func TestMeshVIPDial_ResolvesVirtualAddr(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	vt.Add(netip.MustParseAddr("100.64.0.5"), "node-b")

	var gotTarget *client.MeshService
	base := func(_ context.Context, _ *client.FileClient, _ *hub.HubSignaler, target *client.MeshService, _ string) (*mesh.Result, error) {
		gotTarget = target
		return &mesh.Result{Kind: mesh.KindRelay}, nil
	}
	dial := meshVIPDial(vt, subnet, base, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})

	_, err := dial(context.Background(), nil, nil, &client.MeshService{Addr: "100.64.0.5:22"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget == nil || gotTarget.Node != "node-b" || gotTarget.Addr != "100.64.0.5:22" {
		t.Fatalf("base 收到 target = %+v, want Node=node-b Addr=100.64.0.5:22", gotTarget)
	}
}

// TestMeshVIPDial_UnknownVIPFails 校验 vipTable 中不存在的虚拟 IP 拨号报错（防未知
// 地址注入，不猜测 node-id）。
func TestMeshVIPDial_UnknownVIPFails(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	vt.Add(netip.MustParseAddr("100.64.0.5"), "node-b")

	dial := meshVIPDial(vt, subnet, nil, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})
	_, err := dial(context.Background(), nil, nil, &client.MeshService{Addr: "100.64.0.99:22"}, "")
	if err == nil {
		t.Fatal("未知虚拟 IP 应报错（不猜测 node-id）")
	}
}

// TestMeshVIPDial_NonVirtualAddrFallsBack 校验非虚拟子网目标（含已有 Node 的服务
// 寻址）回落 base 原样拨号，不改写。
func TestMeshVIPDial_NonVirtualAddrFallsBack(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)

	var gotTarget *client.MeshService
	base := func(_ context.Context, _ *client.FileClient, _ *hub.HubSignaler, target *client.MeshService, _ string) (*mesh.Result, error) {
		gotTarget = target
		return &mesh.Result{Kind: mesh.KindWebRTC}, nil
	}
	dial := meshVIPDial(vt, subnet, base, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})

	orig := &client.MeshService{Node: "node-x", Addr: "127.0.0.1:22"}
	if _, err := dial(context.Background(), nil, nil, orig, ""); err != nil {
		t.Fatal(err)
	}
	if gotTarget != orig {
		t.Fatalf("非虚拟子网目标应原样回落 base, got %+v", gotTarget)
	}
}

// TestMeshVIPDial_WrapsGatewayDial 校验 isVIP && --gateway 组合（mesh.go 装配顺序）：
// meshVIPDial 为最外层，先解析虚拟 IP → node-id，再把解析后的 target 传给内层网关
// 选路（meshGatewayDial 用 target.Node 走已建链路）。防止反序（gateway 包最外）导致
// meshVIPDial 被覆盖、目标节点 VIP 变化时解析不到最新 node-id（R-5）。
func TestMeshVIPDial_WrapsGatewayDial(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/10")
	vt := mesh.NewVipTable(subnet)
	vt.Add(netip.MustParseAddr("100.64.0.5"), "node-b")

	// 模拟 meshGatewayDial（内层选路）：记录收到的 target，返回 KindPeerLink。
	var gotTarget *client.MeshService
	gatewayBase := func(_ context.Context, _ *client.FileClient, _ *hub.HubSignaler, target *client.MeshService, _ string) (*mesh.Result, error) {
		gotTarget = target
		return &mesh.Result{Kind: mesh.KindPeerLink}, nil
	}
	// 最外层 = meshVIPDial 包装网关选路（与 mesh.go 装配顺序一致）。
	dial := meshVIPDial(vt, subnet, gatewayBase, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard})

	_, err := dial(context.Background(), nil, nil, &client.MeshService{Addr: "100.64.0.5:22"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if gotTarget == nil || gotTarget.Node != "node-b" || gotTarget.Addr != "100.64.0.5:22" {
		t.Fatalf("网关 base 收到 target = %+v, want Node=node-b Addr=100.64.0.5:22", gotTarget)
	}
}
