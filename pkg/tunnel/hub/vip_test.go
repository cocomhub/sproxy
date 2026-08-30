// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
)

// testVIPSubnet 是测试用的 CGNAT 子网。
var testVIPSubnet = netip.MustParsePrefix("100.64.0.0/10")

// TestNewHubAllocator_Subnet 校验分配器返回配置子网。
func TestNewHubAllocator_Subnet(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	if got := a.Subnet(); got != testVIPSubnet {
		t.Fatalf("Subnet() = %v, want %v", got, testVIPSubnet)
	}
}

// TestHubAllocator_AllocUniqueInSubnet 校验连续分配的虚拟 IP 唯一、落在子网内、
// 且从子网基址 +2（.1 保留网关）起递增。
func TestHubAllocator_AllocUniqueInSubnet(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	want := netip.MustParseAddr("100.64.0.2")
	seen := make(map[netip.Addr]bool)
	for i := range 10 {
		vip, err := a.Alloc("mesh-a", fmt.Sprintf("node-%d", i))
		if err != nil {
			t.Fatalf("Alloc(%d): %v", i, err)
		}
		if !vip.IsValid() || !vip.Is4() || !testVIPSubnet.Contains(vip) {
			t.Fatalf("Alloc(%d) = %v 不在子网 %s 内", i, vip, testVIPSubnet)
		}
		if seen[vip] {
			t.Fatalf("Alloc(%d) = %v 重复分配", i, vip)
		}
		seen[vip] = true
		if i == 0 && vip != want {
			t.Fatalf("首个分配 = %v, want %v（首地址 .1 保留网关，从 .2 起）", vip, want)
		}
	}
}

// TestHubAllocator_AllocStable 校验同一 (mesh, nodeID) 重复 Alloc 返回相同虚拟 IP。
func TestHubAllocator_AllocStable(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	v1, err := a.Alloc("mesh-a", "node-a")
	if err != nil {
		t.Fatalf("首次 Alloc: %v", err)
	}
	for range 3 {
		v2, err := a.Alloc("mesh-a", "node-a")
		if err != nil {
			t.Fatalf("重复 Alloc: %v", err)
		}
		if v2 != v1 {
			t.Fatalf("重复 Alloc 漂移: got %v, want %v", v2, v1)
		}
	}
}

// TestHubAllocator_MeshIsolation 校验不同 mesh 的节点各自获得独立虚拟 IP（分配互不干扰）。
func TestHubAllocator_MeshIsolation(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	vA, err := a.Alloc("mesh-a", "node-x")
	if err != nil {
		t.Fatalf("Alloc(mesh-a): %v", err)
	}
	vB, err := a.Alloc("mesh-b", "node-x")
	if err != nil {
		t.Fatalf("Alloc(mesh-b): %v", err)
	}
	if vA == vB {
		t.Fatalf("不同 mesh 的 node-x 应获得不同虚拟 IP，got %v", vA)
	}
}

// TestHubAllocator_ReleaseReuses 校验 Release 后虚拟 IP 可被后续节点复用。
func TestHubAllocator_ReleaseReuses(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	v1, err := a.Alloc("mesh-a", "node-a")
	if err != nil {
		t.Fatalf("Alloc(node-a): %v", err)
	}
	v2, err := a.Alloc("mesh-a", "node-b")
	if err != nil {
		t.Fatalf("Alloc(node-b): %v", err)
	}
	a.Release("mesh-a", "node-a")
	v3, err := a.Alloc("mesh-a", "node-c")
	if err != nil {
		t.Fatalf("Alloc(node-c): %v", err)
	}
	if v3 != v1 {
		t.Fatalf("Release 后 node-a 的 %v 应被复用，got %v", v1, v3)
	}
	if v2 == v3 {
		t.Fatalf("未释放的 %v 不应被复用", v2)
	}
}

// TestHubAllocator_ReleaseOwnKeyOnly 校验 Release 只释放指定 (mesh, nodeID) 的映射，
// 不影响其他节点。
func TestHubAllocator_ReleaseOwnKeyOnly(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	va, _ := a.Alloc("mesh-a", "node-a")
	vb, _ := a.Alloc("mesh-b", "node-b")
	a.Release("mesh-a", "node-a")
	// node-b 的虚拟 IP 保持不变。
	got, _ := a.Alloc("mesh-b", "node-b")
	if got != vb {
		t.Fatalf("node-b 虚拟 IP 漂移: got %v, want %v", got, vb)
	}
	if va == vb {
		t.Fatalf("node-a/node-b 虚拟 IP 应不同: %v", va)
	}
}

// TestHubAllocator_Exhaust 校验极小子网（/30）地址耗尽时 Alloc 返回错误。
func TestHubAllocator_Exhaust(t *testing.T) {
	small := netip.MustParsePrefix("100.64.0.0/30")
	a := NewHubAllocator(small)
	v1, err := a.Alloc("m", "a")
	if err != nil {
		t.Fatalf("首个 Alloc: %v", err)
	}
	if v1 != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("Alloc = %v, want 100.64.0.2", v1)
	}
	if _, err := a.Alloc("m", "b"); err == nil {
		t.Fatal("/30 子网应地址耗尽（仅 .2 可分配，.1 网关/.3 广播保留）")
	}
}

// TestHubAllocator_ReserveFromSnapshot 校验快照重建：已持久化的虚拟 IP 被保留，
// 新节点不会占用已保留地址。
func TestHubAllocator_ReserveFromSnapshot(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	snap := &Snapshot{
		Nodes: []NodeSnap{
			{ID: "node-a", Mesh: "mesh-a", VirtualIP: netip.MustParseAddr("100.64.0.2")},
			{ID: "node-b", Mesh: "mesh-a", VirtualIP: netip.MustParseAddr("100.64.0.3")},
		},
	}
	if err := a.ReserveSnapshot(snap); err != nil {
		t.Fatalf("ReserveSnapshot: %v", err)
	}
	// 已保留的 (mesh,nodeID) 复用旧地址。
	got, err := a.Alloc("mesh-a", "node-a")
	if err != nil {
		t.Fatalf("Alloc(node-a): %v", err)
	}
	if got != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("node-a 虚拟 IP 漂移: got %v, want 100.64.0.2", got)
	}
	// 新节点不占用已保留地址。
	vn, err := a.Alloc("mesh-a", "node-c")
	if err != nil {
		t.Fatalf("Alloc(node-c): %v", err)
	}
	if vn == netip.MustParseAddr("100.64.0.2") || vn == netip.MustParseAddr("100.64.0.3") {
		t.Fatalf("新节点分配到已保留地址 %v", vn)
	}
	// 递增游标应跳过保留地址：node-c 应为 .4（.2/.3 已保留）。
	if vn != netip.MustParseAddr("100.64.0.4") {
		t.Fatalf("node-c = %v, want 100.64.0.4", vn)
	}
}

// TestHubAllocator_ReserveRejectsReservedOffsets（S-1）校验快照重建不能把网络地址
// （偏移 0）或网关（偏移 1）保留给节点——hub 分配从不产出这些地址，防损坏/伪造
// 持久化文件破坏"首地址保留网关、从 .2 起分配"的不变量。
func TestHubAllocator_ReserveRejectsReservedOffsets(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	if err := a.Reserve("m", "a", netip.MustParseAddr("100.64.0.0")); err == nil {
		t.Fatal("网络地址（偏移 0）不应可保留")
	}
	if err := a.Reserve("m", "a", netip.MustParseAddr("100.64.0.1")); err == nil {
		t.Fatal("网关地址（偏移 1）不应可保留")
	}
	// 正常地址（偏移 ≥2）仍可保留。
	if err := a.Reserve("m", "a", netip.MustParseAddr("100.64.0.2")); err != nil {
		t.Fatalf("正常地址应可保留: %v", err)
	}
}

// TestHubAllocator_TinySubnetExhaust 校验 /31、/32 极小子网无可分配地址（偏移 ≥2
// 且 < maxHost 的范围内无主机）。
func TestHubAllocator_TinySubnetExhaust(t *testing.T) {
	for _, cidr := range []string{"100.64.0.0/31", "100.64.0.0/32"} {
		a := NewHubAllocator(netip.MustParsePrefix(cidr))
		if _, err := a.Alloc("m", "a"); err == nil {
			t.Fatalf("%s 应地址耗尽（无可分配主机）", cidr)
		}
	}
}

// TestHubAllocator_ReserveConflict 校验快照重建时的地址冲突处理：
// 同一 VIP 被两个不同 key 保留 → 第二个报错。
func TestHubAllocator_ReserveConflict(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	if err := a.Reserve("m", "a", netip.MustParseAddr("100.64.0.2")); err != nil {
		t.Fatalf("首个 Reserve: %v", err)
	}
	if err := a.Reserve("m", "b", netip.MustParseAddr("100.64.0.2")); err == nil {
		t.Fatal("同一虚拟 IP 被两个节点保留应报错")
	}
}

// TestHubAllocator_ConcurrentAlloc 校验并发分配的虚拟 IP 唯一（-race 下稳定）。
func TestHubAllocator_ConcurrentAlloc(t *testing.T) {
	a := NewHubAllocator(testVIPSubnet)
	const n = 64
	results := make([]netip.Addr, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vip, err := a.Alloc("mesh-a", fmt.Sprintf("cnode-%d", i))
			if err != nil {
				t.Errorf("并发 Alloc(%d): %v", i, err)
				return
			}
			results[i] = vip
		}(i)
	}
	wg.Wait()
	seen := make(map[netip.Addr]bool, n)
	for i, vip := range results {
		if !vip.IsValid() {
			t.Fatalf("结果[%d] 无效虚拟 IP", i)
		}
		if seen[vip] {
			t.Fatalf("并发分配重复: %v", vip)
		}
		seen[vip] = true
	}
}

// TestNewHubAllocator_RejectsIPv6 校验非 IPv4 子网被拒绝（panic，编程错误即暴露）。
func TestNewHubAllocator_RejectsIPv6(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("IPv6 子网应 panic（虚拟 IP 分配仅支持 IPv4）")
		}
	}()
	NewHubAllocator(netip.MustParsePrefix("fd00::/8"))
}

// TestHubAllocator_DefaultSubnet 校验默认虚拟子网为 CGNAT 100.64.0.0/10。
func TestHubAllocator_DefaultSubnet(t *testing.T) {
	want := netip.MustParsePrefix("100.64.0.0/10")
	got, err := netip.ParsePrefix(DefaultVirtualSubnet)
	if err != nil {
		t.Fatalf("DefaultVirtualSubnet 非法: %v", err)
	}
	if got != want {
		t.Fatalf("DefaultVirtualSubnet = %v, want %v", got, want)
	}
}
