// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestRouteTableAddAndRemove(t *testing.T) {
	rt := hub.NewRouteTable()
	a, b := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	_ = b

	rt.Add("node-1", m)
	if rt.Lookup("node-1") == nil {
		t.Fatal("expected to find node-1")
	}
	if rt.Lookup("unknown") != nil {
		t.Fatal("expected nil for unknown node")
	}

	rt.Remove("node-1")
	if rt.Lookup("node-1") != nil {
		t.Fatal("expected nil after remove")
	}
}

// TestRouteTableRemoveIfOwned 验证 ownership 防护：
// 仅当节点当前绑定到给定 mux 时才移除，防止旧连接误删同名新注册。
func TestRouteTableRemoveIfOwned(t *testing.T) {
	rt := hub.NewRouteTable()

	// 两个不同的 mux
	a1, _ := xfertest.Pipe()
	m1 := mux.New(a1, mux.RoleDialer)
	defer m1.Close()
	a2, _ := xfertest.Pipe()
	m2 := mux.New(a2, mux.RoleDialer)
	defer m2.Close()

	rt.Add("node-1", m1)

	// 用错误的 mux（m2）尝试移除：不应成功
	if rt.RemoveIfOwned("node-1", m2) {
		t.Fatal("RemoveIfOwned with wrong mux should return false")
	}
	if rt.Lookup("node-1") == nil {
		t.Fatal("node-1 should still exist after failed RemoveIfOwned")
	}

	// 用正确的 mux（m1）移除：应成功
	if !rt.RemoveIfOwned("node-1", m1) {
		t.Fatal("RemoveIfOwned with owning mux should return true")
	}
	if rt.Lookup("node-1") != nil {
		t.Fatal("node-1 should be gone after owning RemoveIfOwned")
	}

	// 再次移除（已不存在）：返回 false
	if rt.RemoveIfOwned("node-1", m1) {
		t.Fatal("RemoveIfOwned of absent node should return false")
	}
}

// TestRouteTable_RemoveHook 验证节点移除回调（I6 收件箱清理钩子）：
// Remove / RemoveIfOwned 真正移除节点时触发；失败路径（节点不存在 / 所有权
// 不匹配的 stale identity）不触发；SetRemoveHook(nil) 清除后不再触发。
func TestRouteTable_RemoveHook(t *testing.T) {
	rt := hub.NewRouteTable()
	newTestMux := func() *mux.Mux {
		a, _ := xfertest.Pipe()
		m := mux.New(a, mux.RoleDialer)
		t.Cleanup(func() { _ = m.Close() })
		return m
	}

	var mu sync.Mutex
	var removed []hub.NodeID
	rt.SetRemoveHook(func(id hub.NodeID) {
		mu.Lock()
		removed = append(removed, id)
		mu.Unlock()
	})
	hasRemoved := func(id string) bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(removed, hub.NodeID(id))
	}

	// Remove 成功路径触发
	rt.Add("node-a", newTestMux())
	if !rt.Remove("node-a") {
		t.Fatal("Remove should succeed")
	}
	if !hasRemoved("node-a") {
		t.Fatalf("Remove hook should fire for node-a, got %v", removed)
	}

	// Remove 不存在的节点不触发
	_ = rt.Remove("ghost")
	if len(removed) != 1 {
		t.Fatalf("removing absent node should not fire hook, got %v", removed)
	}

	// RemoveIfOwned 所有权匹配触发
	m := newTestMux()
	rt.AddWithInfoAndServices(hub.NodeInfo{ID: "node-b", Mux: m}, nil)
	if !rt.RemoveIfOwned("node-b", m) {
		t.Fatal("RemoveIfOwned should succeed")
	}
	if !hasRemoved("node-b") {
		t.Fatalf("RemoveIfOwned hook should fire for node-b, got %v", removed)
	}

	// RemoveIfOwned 所有权不匹配不触发（stale identity 防护：同名节点被新连接
	// 重新注册后，旧连接断开不得触发清理——在线节点收件箱保留）
	rt.Add("node-c", newTestMux())
	if rt.RemoveIfOwned("node-c", newTestMux()) {
		t.Fatal("RemoveIfOwned with wrong mux should return false")
	}
	if len(removed) != 2 {
		t.Fatalf("RemoveIfOwned mismatch should not fire hook, got %v", removed)
	}

	// SetRemoveHook(nil) 清除后不再触发
	rt.SetRemoveHook(nil)
	_ = rt.Remove("node-c")
	if len(removed) != 2 {
		t.Fatalf("cleared hook should not fire, got %v", removed)
	}
}

func TestRouteTableConcurrent(t *testing.T) {
	rt := hub.NewRouteTable()
	var wg sync.WaitGroup

	for i := range 10 {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			a, _ := xfertest.Pipe()
			m := mux.New(a, mux.RoleDialer)
			id := hub.NodeID(rune('a' + i))
			rt.Add(id, m)
		}()
	}
	wg.Wait()

	nodes := rt.List()
	if len(nodes) != 10 {
		t.Fatalf("expected 10 nodes, got %d", len(nodes))
	}
}

func TestRouteTableEmptyList(t *testing.T) {
	rt := hub.NewRouteTable()
	nodes := rt.List()
	if len(nodes) != 0 {
		t.Fatalf("expected empty list, got %d", len(nodes))
	}
}

func TestRouteTableAddWithInfo(t *testing.T) {
	rt := hub.NewRouteTable()
	a, b := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()
	_ = b

	rt.AddWithInfo(hub.NodeInfo{
		ID:        "node-with-info",
		Mux:       m,
		Connected: time.Now(),
		Addr:      "127.0.0.1:8080",
		Secret:    "sec-***",
	})

	// Lookup should work
	if rt.Lookup("node-with-info") == nil {
		t.Fatal("expected to find node-with-info")
	}

	// LookupInfo should return the stored info
	if info, ok := rt.LookupInfo("node-with-info"); !ok {
		t.Fatal("expected LookupInfo to find node-with-info")
	} else if info.Addr != "127.0.0.1:8080" {
		t.Fatalf("expected addr 127.0.0.1:8080, got %s", info.Addr)
	} else if info.Secret != "sec-***" {
		t.Fatalf("expected secret sec-***, got %s", info.Secret)
	}

	// List should include info
	nodes := rt.List()
	if len(nodes) != 1 {
		t.Fatalf("expected 1 node, got %d", len(nodes))
	}
	if nodes[0].Addr != "127.0.0.1:8080" {
		t.Fatalf("expected addr 127.0.0.1:8080, got %s", nodes[0].Addr)
	}
	if nodes[0].Secret != "sec-***" {
		t.Fatalf("expected secret sec-***, got %s", nodes[0].Secret)
	}
	if nodes[0].Connected.IsZero() {
		t.Fatal("expected non-zero Connected time")
	}
}

func TestRouteTableDuplicateReplace(t *testing.T) {
	rt := hub.NewRouteTable()
	a1, b1 := xfertest.Pipe()
	m1 := mux.New(a1, mux.RoleDialer)
	_ = b1

	a2, b2 := xfertest.Pipe()
	m2 := mux.New(a2, mux.RoleDialer)
	_ = b2

	rt.Add("same-node", m1)
	rt.Add("same-node", m2)

	// Should point to m2 now
	if rt.Lookup("same-node") != m2 {
		t.Fatal("expected lookup to return new mux after replace")
	}
}

func TestRouteTableNodeCount(t *testing.T) {
	rt := hub.NewRouteTable()
	if c := rt.NodeCount(); c != 0 {
		t.Fatalf("expected 0, got %d", c)
	}
	rt.Add("a", nil)
	rt.Add("b", nil)
	if c := rt.NodeCount(); c != 2 {
		t.Fatalf("expected 2, got %d", c)
	}
	rt.Remove("a")
	if c := rt.NodeCount(); c != 1 {
		t.Fatalf("expected 1, got %d", c)
	}
}

// TestRouteTableRemoveReturnsBool 验证 Remove 返回是否真正移除。
func TestRouteTableRemoveReturnsBool(t *testing.T) {
	rt := hub.NewRouteTable()
	rt.Add("node-1", nil)
	if !rt.Remove("node-1") {
		t.Fatal("expected Remove to return true for existing node")
	}
	if rt.Remove("node-1") {
		t.Fatal("expected Remove to return false for absent node")
	}
}

// TestRouteTableLookupInfoAbsent 验证 LookupInfo 对不存在的节点返回 false。
func TestRouteTableLookupInfoAbsent(t *testing.T) {
	rt := hub.NewRouteTable()
	if _, ok := rt.LookupInfo("nope"); ok {
		t.Fatal("expected LookupInfo false for unknown node")
	}

	// AddWithInfo 但 nodes 表不同步（异常状态）也应返回 false
	rt.Add("mux-only", nil)
	if _, ok := rt.LookupInfo("mux-only"); ok {
		t.Fatal("expected LookupInfo false when info entry absent")
	}
}

// TestRouteTableAddWithInfoAndServices 验证原子写入节点与服务宣告，
// 以及空 svcs 清除旧宣告的语义。
func TestRouteTableAddWithInfoAndServices(t *testing.T) {
	rt := hub.NewRouteTable()

	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	defer m.Close()

	info := hub.NodeInfo{ID: "node-svc", Mux: m, Connected: time.Now()}
	svcs := []hub.Service{
		{Name: "ssh", Addr: "127.0.0.1:22"},
		{Name: "web", Addr: "10.0.0.1:8080"},
	}
	rt.AddWithInfoAndServices(info, svcs)

	if got := rt.ServicesOf("node-svc"); len(got) != 2 {
		t.Fatalf("expected 2 services, got %d: %+v", len(got), got)
	}
	if got, ok := rt.LookupInfo("node-svc"); !ok || got.Mux != m {
		t.Fatal("expected node to be registered with mux")
	}

	// 空 svcs：等价清除旧宣告，但节点本身保留
	rt.AddWithInfoAndServices(info, nil)
	if got := rt.ServicesOf("node-svc"); len(got) != 0 {
		t.Fatalf("expected 0 services after clear, got %d: %+v", len(got), got)
	}
	if rt.Lookup("node-svc") == nil {
		t.Fatal("node should still be registered after clearing services")
	}
}

// TestRouteTableListServicesStableOrder 验证 ListServices 按 (node, name)
// 稳定排序（I3），多节点同名服务保持多候选（failover 不破坏）。
func TestRouteTableListServicesStableOrder(t *testing.T) {
	rt := hub.NewRouteTable()

	a1, _ := xfertest.Pipe()
	m1 := mux.New(a1, mux.RoleDialer)
	defer m1.Close()
	rt.AddWithInfoAndServices(hub.NodeInfo{ID: "node-b", Mux: m1, Connected: time.Now()}, []hub.Service{
		{Name: "ssh", Addr: "b-ip:22"},
		{Name: "web", Addr: "b-ip:80"},
	})

	a2, _ := xfertest.Pipe()
	m2 := mux.New(a2, mux.RoleDialer)
	defer m2.Close()
	rt.AddWithInfoAndServices(hub.NodeInfo{ID: "node-a", Mux: m2, Connected: time.Now()}, []hub.Service{
		{Name: "web", Addr: "a-ip:80"},
		{Name: "ssh", Addr: "a-ip:22"},
	})

	got := rt.ListServices()
	want := []hub.NodeService{
		{Node: "node-a", Service: hub.Service{Name: "ssh", Addr: "a-ip:22"}},
		{Node: "node-a", Service: hub.Service{Name: "web", Addr: "a-ip:80"}},
		{Node: "node-b", Service: hub.Service{Name: "ssh", Addr: "b-ip:22"}},
		{Node: "node-b", Service: hub.Service{Name: "web", Addr: "b-ip:80"}},
	}
	if len(got) != len(want) {
		t.Fatalf("expected %d services, got %d: %+v", len(want), len(got), got)
	}
	for i := range want {
		if got[i].Node != want[i].Node || got[i].Service.Name != want[i].Service.Name || got[i].Service.Addr != want[i].Service.Addr {
			t.Fatalf("entry %d mismatch: got %+v, want %+v", i, got[i], want[i])
		}
	}
}
