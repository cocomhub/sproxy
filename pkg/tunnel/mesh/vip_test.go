// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"fmt"
	"net/netip"
	"sync"
	"testing"
)

var testVIPSubnet = netip.MustParsePrefix("100.64.0.0/10")

// TestVipTable_AddQuery 校验 vipTable 的 Add/NodeByAddr/AddrByNode 与 Subnet。
func TestVipTable_AddQuery(t *testing.T) {
	vt := NewVipTable(testVIPSubnet)
	vt.Add(netip.MustParseAddr("100.64.0.2"), "node-a")
	vt.Add(netip.MustParseAddr("100.64.0.3"), "node-b")

	if got, ok := vt.NodeByAddr(netip.MustParseAddr("100.64.0.2")); !ok || got != "node-a" {
		t.Fatalf("NodeByAddr(.2) = %q, %v", got, ok)
	}
	if got, ok := vt.AddrByNode("node-b"); !ok || got != netip.MustParseAddr("100.64.0.3") {
		t.Fatalf("AddrByNode(node-b) = %v, %v", got, ok)
	}
	if _, ok := vt.NodeByAddr(netip.MustParseAddr("100.64.0.99")); ok {
		t.Fatal("未注册的虚拟 IP 不应命中")
	}
	if _, ok := vt.AddrByNode("node-unknown"); ok {
		t.Fatal("未注册的节点不应命中")
	}
	if vt.Subnet() != testVIPSubnet {
		t.Fatalf("Subnet() = %v, want %v", vt.Subnet(), testVIPSubnet)
	}
}

// TestVipTable_AddConflictRejects 校验同 addr 被不同 nodeID 声明时**拒绝**（保留
// 既有映射，防 VIP 劫持）；同 nodeID 幂等覆盖；新 addr 正常加入。
func TestVipTable_AddConflictRejects(t *testing.T) {
	vt := NewVipTable(testVIPSubnet)
	addr := netip.MustParseAddr("100.64.0.2")
	if !vt.Add(addr, "node-a") {
		t.Fatal("首个 Add 应成功")
	}
	// 同 nodeID 幂等。
	if !vt.Add(addr, "node-a") {
		t.Fatal("同 nodeID 重复 Add 应幂等成功")
	}
	// 不同 nodeID → 冲突拒绝，保留 node-a。
	if vt.Add(addr, "node-b") {
		t.Fatal("同 addr 不同 nodeID 应拒绝（防 VIP 劫持）")
	}
	if got, _ := vt.NodeByAddr(addr); got != "node-a" {
		t.Fatalf("冲突后映射应保留 node-a, got %q", got)
	}
	// 不同 addr 正常加入。
	if !vt.Add(netip.MustParseAddr("100.64.0.3"), "node-b") {
		t.Fatal("不同 addr 应正常加入")
	}
}

// TestDeterministicAllocator_Properties 校验 mDNS 无 hub 回落分配器：
// 稳定（同输入同输出）、在子网内、非保留地址（偏移 ≥2）、Subnet 正确。
func TestDeterministicAllocator_Properties(t *testing.T) {
	a := newDeterministicAllocator(testVIPSubnet)
	seen := make(map[netip.Addr]string)
	for i := 0; i < 20; i++ {
		nodeID := "node-" + string(rune('a'+i))
		vip, err := a.Alloc("mesh-a", nodeID)
		if err != nil {
			t.Fatalf("Alloc(%s): %v", nodeID, err)
		}
		if !vip.IsValid() || !vip.Is4() || !testVIPSubnet.Contains(vip) {
			t.Fatalf("Alloc(%s) = %v 不在子网 %s 内", nodeID, vip, testVIPSubnet)
		}
		if off := ipv4Offset(testVIPSubnet, vip); off < 2 {
			t.Fatalf("Alloc(%s) = %v 偏移 %d < 2（保留地址）", nodeID, vip, off)
		}
		// 稳定：重复 Alloc 返回相同值。
		vip2, err := a.Alloc("mesh-a", nodeID)
		if err != nil || vip2 != vip {
			t.Fatalf("Alloc(%s) 不稳定: %v → %v", nodeID, vip, vip2)
		}
		seen[vip] = nodeID
	}
	// 20 个节点在 /10 子网（4M 地址）哈希碰撞概率极低，但存在理论可能；
	// 不因碰撞失败（确定性分配不保证唯一，仅 hub 权威分配保证）。
	_ = seen
	if a.Subnet() != testVIPSubnet {
		t.Fatalf("Subnet() = %v, want %v", a.Subnet(), testVIPSubnet)
	}
}

// TestDeterministicAllocator_ReleaseNoop 校验 Release 是 no-op（无状态分配器）。
func TestDeterministicAllocator_ReleaseNoop(t *testing.T) {
	a := newDeterministicAllocator(testVIPSubnet)
	v1, _ := a.Alloc("m", "node-x")
	a.Release("m", "node-x")
	v2, _ := a.Alloc("m", "node-x")
	if v1 != v2 {
		t.Fatalf("Release 后同 node 分配漂移: %v → %v", v1, v2)
	}
}

// TestDeterministicAllocator_RejectsIPv6 校验 IPv6 子网被拒绝（虚拟 IP 仅支持 IPv4）。
func TestDeterministicAllocator_RejectsIPv6(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("IPv6 子网应 panic（虚拟 IP 分配仅支持 IPv4）")
		}
	}()
	newDeterministicAllocator(netip.MustParsePrefix("fd00::/8"))
}

// TestIsVirtualAddr 校验虚拟子网归属判断。
func TestIsVirtualAddr(t *testing.T) {
	if !IsVirtualAddr(netip.MustParseAddr("100.64.0.5"), testVIPSubnet) {
		t.Fatal("100.64.0.5 应在虚拟子网内")
	}
	if IsVirtualAddr(netip.MustParseAddr("8.8.8.8"), testVIPSubnet) {
		t.Fatal("8.8.8.8 不应在虚拟子网内")
	}
	if IsVirtualAddr(netip.Addr{}, testVIPSubnet) {
		t.Fatal("无效地址不应在虚拟子网内")
	}
}

// TestParseVirtualAddr 校验 host 解析为虚拟 IP。
func TestParseVirtualAddr(t *testing.T) {
	if got, ok := ParseVirtualAddr("100.64.0.5"); !ok || got != netip.MustParseAddr("100.64.0.5") {
		t.Fatalf("ParseVirtualAddr(100.64.0.5) = %v, %v", got, ok)
	}
	if _, ok := ParseVirtualAddr("not-an-ip"); ok {
		t.Fatal("非 IP 不应解析成功")
	}
	if _, ok := ParseVirtualAddr("100.64.0.5:22"); ok {
		t.Fatal("带端口的不应解析为纯 IP（调用方先 SplitHostPort）")
	}
}

// TestDeterministicAllocator_ConflictRejectedByTable（安全审查 MEDIUM 回归）校验：
// 确定性分配碰撞时，vipTable 学习侧**拒绝**第二个 nodeID 的映射（保留既有映射，
// 防 VIP 劫持）。用 /24 子网（253 可用地址）+ 300 样本保证鸽巢碰撞。
func TestDeterministicAllocator_ConflictRejectedByTable(t *testing.T) {
	subnet := netip.MustParsePrefix("100.64.0.0/24")
	a := newDeterministicAllocator(subnet)
	vt := NewVipTable(subnet)
	seen := make(map[netip.Addr]string, 300)
	conflictDetected := false
	for i := 0; i < 300; i++ {
		nodeID := fmt.Sprintf("cnode-%d", i)
		vip, err := a.Alloc("m", nodeID)
		if err != nil {
			t.Fatalf("Alloc(%s): %v", nodeID, err)
		}
		if prev, dup := seen[vip]; dup {
			// 碰撞：学习侧应拒绝第二个不同 nodeID 的映射。
			conflictDetected = true
			if vt.Add(vip, prev) && vt.Add(vip, nodeID) {
				t.Fatalf("碰撞 VIP %v 被 %s 与 %s 同时接受（应拒绝，防 VIP 劫持）", vip, prev, nodeID)
			}
			if got, _ := vt.NodeByAddr(vip); got != prev {
				t.Fatalf("冲突后映射应保留 %s, got %q", prev, got)
			}
			break
		}
		seen[vip] = nodeID
		_ = vt.Add(vip, nodeID)
	}
	if !conflictDetected {
		t.Fatal("/24 子网 253 可用地址 + 300 样本应鸽巢碰撞（确定性哈希必然重复）")
	}
}

// TestVipTable_Reconcile（安全审查 MEDIUM 精化）校验 hub 权威模式原子重建：
// 每次刷新从签名 hub 节点列表清空陈旧条目、重新填入权威映射（不依赖谁先声明）。
func TestVipTable_Reconcile(t *testing.T) {
	vt := NewVipTable(testVIPSubnet)
	vt.Add(netip.MustParseAddr("100.64.0.2"), "node-a")
	vt.Add(netip.MustParseAddr("100.64.0.3"), "node-stale") // 陈旧节点

	// 刷新：node-a 保留，node-stale 清除，node-b 加入。
	ok := vt.Reconcile([]VipEntry{
		{Addr: netip.MustParseAddr("100.64.0.2"), NodeID: "node-a"},
		{Addr: netip.MustParseAddr("100.64.0.4"), NodeID: "node-b"},
	})
	if !ok {
		t.Fatal("合法重建应成功")
	}
	if got, _ := vt.NodeByAddr(netip.MustParseAddr("100.64.0.3")); got != "" {
		t.Fatalf("陈旧节点 node-stale 应被清除, got %q", got)
	}
	if got, _ := vt.NodeByAddr(netip.MustParseAddr("100.64.0.4")); got != "node-b" {
		t.Fatalf("node-b 应加入, got %q", got)
	}
	if got, _ := vt.NodeByAddr(netip.MustParseAddr("100.64.0.2")); got != "node-a" {
		t.Fatalf("node-a 应保留, got %q", got)
	}

	// 权威源内冲突（同一 addr 两个 nodeID）→ 拒绝重建，保持原表。
	if vt.Reconcile([]VipEntry{
		{Addr: netip.MustParseAddr("100.64.0.2"), NodeID: "node-x"},
		{Addr: netip.MustParseAddr("100.64.0.2"), NodeID: "node-y"},
	}) {
		t.Fatal("权威源内冲突应拒绝重建")
	}
	if got, _ := vt.NodeByAddr(netip.MustParseAddr("100.64.0.2")); got != "node-a" {
		t.Fatalf("冲突拒绝后应保留原表, got %q", got)
	}

	// 子网外地址 → 拒绝重建。
	if vt.Reconcile([]VipEntry{{Addr: netip.MustParseAddr("8.8.8.8"), NodeID: "node-x"}}) {
		t.Fatal("子网外地址应拒绝重建")
	}
}

// TestVipTable_AddVerified（安全审查 MEDIUM 精化）校验 mDNS 模式只接受与确定性分配
// 结果一致的虚拟 IP——对端自声明任意 VIP 被拒，无法抢占。
func TestVipTable_AddVerified(t *testing.T) {
	vt := NewVipTable(testVIPSubnet)
	alloc := newDeterministicAllocator(testVIPSubnet)

	// 一致 → 接受。
	expected, _ := alloc.Alloc("", "node-a")
	if !vt.AddVerified(expected, "node-a", "", alloc) {
		t.Fatalf("与确定性分配一致的 VIP %v 应接受", expected)
	}
	if got, _ := vt.NodeByAddr(expected); got != "node-a" {
		t.Fatalf("接受后映射 = %q, want node-a", got)
	}
	// 不一致（攻击者自声明任意 VIP）→ 拒绝。
	other := netip.MustParseAddr("100.64.0.9")
	if other == expected {
		other = netip.MustParseAddr("100.64.0.10")
	}
	if vt.AddVerified(other, "node-a", "", alloc) {
		t.Fatal("与确定性分配不一致的自声明 VIP 应拒绝（防抢占）")
	}
	// 子网外 → 拒绝。
	if vt.AddVerified(netip.MustParseAddr("8.8.8.8"), "node-a", "", alloc) {
		t.Fatal("子网外地址应拒绝")
	}
	// 已映射不同 nodeID → 冲突拒绝（与 Add 一致）。
	expectedB, _ := alloc.Alloc("", "node-b")
	_ = vt.AddVerified(expectedB, "node-b", "", alloc)
	if vt.AddVerified(expectedB, "node-c", "", alloc) {
		t.Fatal("同 addr 不同 nodeID 应拒绝")
	}
}

// TestVipTable_ReconcileConcurrent（S-3 回归）校验并发 Reconcile 重建 + 读表在
// -race 下稳定（原子换表，读侧不半读）。
func TestVipTable_ReconcileConcurrent(t *testing.T) {
	vt := NewVipTable(testVIPSubnet)
	vt.Add(netip.MustParseAddr("100.64.0.2"), "node-a")

	var wg sync.WaitGroup
	// 并发重建（交替不同条目集）。
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			entries := []VipEntry{
				{Addr: netip.MustParseAddr("100.64.0.2"), NodeID: "node-a"},
				{Addr: netip.MustParseAddr("100.64.0.3"), NodeID: fmt.Sprintf("node-%d", i)},
			}
			vt.Reconcile(entries)
		}(i)
		go func() {
			defer wg.Done()
			_, _ = vt.NodeByAddr(netip.MustParseAddr("100.64.0.2"))
			_ = vt.Nodes()
		}()
	}
	wg.Wait()
}

// TestDeterministicAllocator_TinySubnet（S-3 回归）校验 /31、/32 无可分配地址报错，
// /30 单宿主（偏移 2）正确分配。
func TestDeterministicAllocator_TinySubnet(t *testing.T) {
	for _, cidr := range []string{"100.64.0.0/31", "100.64.0.0/32"} {
		a := newDeterministicAllocator(netip.MustParsePrefix(cidr))
		if _, err := a.Alloc("m", "node-a"); err == nil {
			t.Fatalf("%s 应地址耗尽（无可分配主机）", cidr)
		}
	}
	// /30：size=4，可分配偏移 [2,2]（.2 唯一宿主；.1 网关、.3 广播保留）。
	a := newDeterministicAllocator(netip.MustParsePrefix("100.64.0.0/30"))
	vip, err := a.Alloc("m", "node-a")
	if err != nil {
		t.Fatalf("/30 Alloc: %v", err)
	}
	if vip != netip.MustParseAddr("100.64.0.2") {
		t.Fatalf("/30 Alloc = %v, want 100.64.0.2", vip)
	}
}
