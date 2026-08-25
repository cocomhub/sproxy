// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// newTestMux 创建测试用 mux（管道传输对），t.Cleanup 关闭。
func newTestMux(t *testing.T) *mux.Mux {
	t.Helper()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// TestMeshRouteTable_CrossMeshIsolation（M-9 核心验收）：两个 mesh 注册节点，
// 列表/转发/服务发现/计数全部按 mesh 隔离，跨 mesh 不可见。
func TestMeshRouteTable_CrossMeshIsolation(t *testing.T) {
	mrt := NewMeshRouteTable()
	muxA := newTestMux(t)
	muxB := newTestMux(t)
	mrt.Add("mesh-a", NodeInfo{ID: "node-a", Mux: muxA, Connected: time.Now()}, []Service{{Name: "svc-a", Addr: "a:22"}})
	mrt.Add("mesh-b", NodeInfo{ID: "node-b", Mux: muxB, Connected: time.Now()}, []Service{{Name: "svc-b", Addr: "b:22"}})

	// 各自 List(mesh) 只见本 mesh。
	meshA := mrt.List("mesh-a")
	if len(meshA) != 1 || meshA[0].ID != "node-a" {
		t.Fatalf("List(mesh-a) = %+v, want 仅 node-a", meshA)
	}
	meshB := mrt.List("mesh-b")
	if len(meshB) != 1 || meshB[0].ID != "node-b" {
		t.Fatalf("List(mesh-b) = %+v, want 仅 node-b", meshB)
	}
	// NodeInfo.Mesh 已写入。
	if meshA[0].Mesh != "mesh-a" || meshB[0].Mesh != "mesh-b" {
		t.Fatalf("NodeInfo.Mesh 未写入: mesh-a=%q mesh-b=%q", meshA[0].Mesh, meshB[0].Mesh)
	}

	// 路由面隔离：各 mesh 的独立 RouteTable 不含他 mesh 节点（跨 mesh Lookup/Has 为 nil/false）。
	if got := mrt.Table("mesh-a").Lookup("node-b"); got != nil {
		t.Fatal("mesh-a 的路由表不应查到 mesh-b 的 node-b（路由面隔离）")
	}
	if got := mrt.Table("mesh-b").Lookup("node-a"); got != nil {
		t.Fatal("mesh-b 的路由表不应查到 mesh-a 的 node-a（路由面隔离）")
	}
	if mrt.Table("mesh-a").Has("node-b") || mrt.Table("mesh-b").Has("node-a") {
		t.Fatal("各 mesh 表不应互相 Has 对方节点")
	}

	// 聚合 Lookup 按 nodeMesh 定位到节点所属 mesh（转发面：nodeID 只属于一个 mesh）。
	if mrt.Lookup("node-a") != muxA || mrt.Lookup("node-b") != muxB {
		t.Fatal("聚合 Lookup 应查到各自所属 mesh 的 mux")
	}
	if mrt.Lookup("ghost") != nil {
		t.Fatal("未知节点 Lookup 应返回 nil")
	}
	if mrt.MeshOf("node-a") != "mesh-a" || mrt.MeshOf("node-b") != "mesh-b" {
		t.Fatalf("MeshOf 错误: %q/%q", mrt.MeshOf("node-a"), mrt.MeshOf("node-b"))
	}

	// 服务发现按 mesh 隔离。
	svcA := mrt.ListServices("mesh-a")
	if len(svcA) != 1 || svcA[0].Node != "node-a" || svcA[0].Service.Name != "svc-a" {
		t.Fatalf("ListServices(mesh-a) = %+v, want 仅 svc-a", svcA)
	}
	if got := mrt.ListServices("mesh-b"); len(got) != 1 || got[0].Service.Name != "svc-b" {
		t.Fatalf("ListServices(mesh-b) = %+v, want 仅 svc-b", got)
	}

	// 节点计数按 mesh 隔离。
	if mrt.NodeCount("mesh-a") != 1 || mrt.NodeCount("mesh-b") != 1 {
		t.Fatalf("NodeCount mesh-a=%d mesh-b=%d, want 1/1", mrt.NodeCount("mesh-a"), mrt.NodeCount("mesh-b"))
	}
	if mrt.NodeCount("ghost-mesh") != 0 {
		t.Fatal("不存在 mesh 的 NodeCount 应为 0")
	}
}

// TestMeshRouteTable_DefaultMeshBehavesLikeSingleMesh：默认 mesh "" 与单 mesh 行为等价。
func TestMeshRouteTable_DefaultMeshBehavesLikeSingleMesh(t *testing.T) {
	mrt := NewMeshRouteTable()
	mux1 := newTestMux(t)
	mrt.AddNode("", "node-1", mux1)

	if !mrt.Has("node-1") {
		t.Fatal("默认 mesh 节点应可寻址（Has）")
	}
	if mrt.Lookup("node-1") != mux1 {
		t.Fatal("默认 mesh 节点应可转发（Lookup）")
	}
	if got := mrt.List(""); len(got) != 1 || got[0].ID != "node-1" {
		t.Fatalf("List(\"\") = %+v, want 仅 node-1", got)
	}
	if mrt.NodeCount("") != 1 {
		t.Fatalf("NodeCount(\"\") = %d, want 1", mrt.NodeCount(""))
	}
}

// TestMeshRouteTable_RemoveCleansNodeMeshAndEmptyTable：Remove 成功后 nodeMesh 清理、
// 空 mesh 表删除；RemoveIfOwned 所有权不匹配不删除。
func TestMeshRouteTable_RemoveCleansNodeMeshAndEmptyTable(t *testing.T) {
	mrt := NewMeshRouteTable()
	muxA := newTestMux(t)
	mrt.AddNode("mesh-a", "node-a", muxA)

	if !mrt.Remove("node-a") {
		t.Fatal("Remove 应成功")
	}
	if mrt.Has("node-a") {
		t.Fatal("Remove 后节点不应存在")
	}
	if mrt.MeshOf("node-a") != "" {
		t.Fatalf("Remove 后 MeshOf 应为空, got %q", mrt.MeshOf("node-a"))
	}
	// 空 mesh 表被清理：AllMeshes 不应包含 mesh-a。
	if got := mrt.AllMeshes(); slices.Contains(got, "mesh-a") {
		t.Fatalf("空 mesh 表应被清理, AllMeshes=%v", got)
	}

	// RemoveIfOwned：错误 mux 不删，正确 mux 删。
	muxB := newTestMux(t)
	muxC := newTestMux(t)
	mrt.AddNode("mesh-b", "node-b", muxB)
	if mrt.RemoveIfOwned("node-b", muxC) {
		t.Fatal("RemoveIfOwned 错误 mux 不应删除")
	}
	if !mrt.Has("node-b") {
		t.Fatal("RemoveIfOwned 失败后节点应保留")
	}
	if !mrt.RemoveIfOwned("node-b", muxB) {
		t.Fatal("RemoveIfOwned 正确 mux 应删除")
	}
	if mrt.Has("node-b") {
		t.Fatal("RemoveIfOwned 后节点不应存在")
	}
}

// TestMeshRouteTable_SameNodeID_MovesMesh：同一节点 ID 跨 mesh 重注册时，
// 从旧 mesh 表移除（节点名不跨 mesh 共享），nodeMesh 指向新 mesh。
func TestMeshRouteTable_SameNodeID_MovesMesh(t *testing.T) {
	mrt := NewMeshRouteTable()
	muxA := newTestMux(t)
	muxB := newTestMux(t)
	mrt.AddNode("mesh-a", "node-x", muxA)

	// 同 ID 重注册到 mesh-b。
	mrt.AddNode("mesh-b", "node-x", muxB)

	if got := mrt.List("mesh-a"); len(got) != 0 {
		t.Fatalf("node-x 迁走后 mesh-a 应无节点, got %+v", got)
	}
	if mrt.Lookup("node-x") != muxB {
		t.Fatal("node-x 应指向 mesh-b 的 mux")
	}
	if mrt.MeshOf("node-x") != "mesh-b" {
		t.Fatalf("MeshOf = %q, want mesh-b", mrt.MeshOf("node-x"))
	}
	if mrt.Has("node-x") == false {
		t.Fatal("node-x 应在 mesh-b 中")
	}
}

// TestMeshRouteTable_RemoveHookFiresPerMesh：SetRemoveHook 为所有内部表挂回调
// （含之后惰性新建的表），节点移除时按 nodeID 触发。
func TestMeshRouteTable_RemoveHookFiresPerMesh(t *testing.T) {
	mrt := NewMeshRouteTable()
	var mu sync.Mutex
	var removed []NodeID
	mrt.SetRemoveHook(func(id NodeID) {
		mu.Lock()
		removed = append(removed, id)
		mu.Unlock()
	})

	// 已存在与新建的表都挂上回调。
	mrt.AddNode("mesh-a", "node-a", newTestMux(t))
	mrt.AddNode("mesh-b", "node-b", newTestMux(t))
	if !mrt.Remove("node-a") {
		t.Fatal("Remove node-a 应成功")
	}
	if !mrt.Remove("node-b") {
		t.Fatal("Remove node-b 应成功")
	}
	mu.Lock()
	got := slices.Clone(removed)
	mu.Unlock()
	if len(got) != 2 || !slices.Contains(got, "node-a") || !slices.Contains(got, "node-b") {
		t.Fatalf("remove hook 应按 nodeID 触发: %v", got)
	}

	// SetRemoveHook(nil) 清除后不再触发。
	mrt.SetRemoveHook(nil)
	mrt.AddNode("mesh-c", "node-c", newTestMux(t))
	_ = mrt.Remove("node-c")
	mu.Lock()
	after := slices.Clone(removed)
	mu.Unlock()
	if len(after) != 2 {
		t.Fatalf("清除回调后不应触发, got %v", after)
	}
}

// TestMeshRouteTable_AllMeshes：AllMeshes 返回所有 mesh（含默认 ""），无节点时为空。
func TestMeshRouteTable_AllMeshes(t *testing.T) {
	mrt := NewMeshRouteTable()
	if got := mrt.AllMeshes(); len(got) != 0 {
		t.Fatalf("初始 AllMeshes 应为空, got %v", got)
	}
	mrt.AddNode("mesh-a", "node-a", newTestMux(t))
	mrt.AddNode("", "node-0", newTestMux(t))
	got := mrt.AllMeshes()
	if !slices.Contains(got, "mesh-a") || !slices.Contains(got, "") {
		t.Fatalf("AllMeshes 应包含 mesh-a 与默认 \"\", got %v", got)
	}
}
