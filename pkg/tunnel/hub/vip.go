// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"fmt"
	"net/netip"
	"sync"
)

// DefaultVirtualSubnet 是虚拟 IP 分配的默认子网：RFC 6598 CGNAT 段 100.64.0.0/10。
// 与 Tailscale 同款，不与常见私网（10/8、172.16/12、192.168/16）冲突。
// 子网首地址（网络地址 + .1）保留为网关/默认，实际分配从 .2 起。
const DefaultVirtualSubnet = "100.64.0.0/10"

// Allocator 是虚拟 IP 分配器抽象。双实现：
//   - hubAllocator（本文件，pkg/tunnel/hub）：hub 权威递增分配 + 快照重建；
//   - deterministicAllocator（pkg/tunnel/mesh）：mDNS 无 hub 回落本地确定性哈希。
//
// mesh 包实现本接口（mesh import hub），hub 不 import mesh，接口放本包避免环。
type Allocator interface {
	// Alloc 为指定 mesh 下的 node-id 分配虚拟 IP（稳定：同一 node-id 复用上次分配）。
	Alloc(mesh, nodeID string) (netip.Addr, error)
	// Release 在节点移除时释放虚拟 IP（回收复用）。
	Release(mesh, nodeID string)
	// Subnet 返回本分配器使用的虚拟子网。
	Subnet() netip.Prefix
}

// hubAllocator 是 hub 权威的虚拟 IP 分配器：在配置子网内递增分配，
// 按 (mesh, nodeID) 记录并复用；节点移除时 Release 回收；支持从持久化快照重建。
//
// 分配范围：子网基址 +2 起（.0 网络地址、.1 网关保留），到广播地址前一个结束。
// 第一版不按 mesh 划分子块（全局唯一由 hub 保证，mesh 隔离由路由面保证）。
type hubAllocator struct {
	mu sync.Mutex
	// subnet 是掩码化后的 IPv4 子网。
	subnet netip.Prefix
	// base 是子网基址的 IPv4 整数表示。
	base uint64
	// maxHost 是可分配的最大主机偏移（不含）：可分配范围 [2, maxHost)。
	maxHost uint64
	// assigned 记录 (mesh,nodeID) → 虚拟 IP（复用稳定）。
	assigned map[string]netip.Addr
	// used 是虚拟 IP → key 反向映射（防重复分配）。
	used map[netip.Addr]string
	// next 是下一次递增分配的候选主机偏移（从 2 起；释放的地址经回扫复用）。
	next uint64
}

// allocKey 拼接 mesh 与 nodeID 为分配表主键。
func allocKey(mesh, nodeID string) string {
	return mesh + "\x00" + nodeID
}

// NewHubAllocator 创建 hub 权威虚拟 IP 分配器。subnet 必须是掩码化的 IPv4 前缀；
// 调用方应先用 ParsePrefix 校验（config.Validate 保证 IPv4）。传非 IPv4 前缀会 panic
// （编程错误：虚拟 IP 分配仅支持 IPv4，配置路径已被 config.Validate 拦截，直接
// 构造 IPv6 前缀属误用，立即暴露优于静默移位溢出产生错误地址）。
func NewHubAllocator(subnet netip.Prefix) *hubAllocator {
	if !subnet.Addr().Is4() {
		panic(fmt.Sprintf("hub: NewHubAllocator 需要 IPv4 子网，got %s", subnet))
	}
	subnet = subnet.Masked()
	size := uint64(1) << (32 - subnet.Bits())
	base := ipv4ToU64(subnet.Addr())
	// 可分配 [base+2, base+size-2)：maxHost = size-1，扫描 i<maxHost 即 i<=size-2。
	maxHost := max(size-1,
		// 极小子网（/31 及更小）：无可分配地址，Alloc 必耗尽。
		2)
	return &hubAllocator{
		subnet:   subnet,
		base:     base,
		maxHost:  maxHost,
		assigned: make(map[string]netip.Addr),
		used:     make(map[netip.Addr]string),
		next:     2,
	}
}

// Subnet 返回分配器使用的虚拟子网。
func (a *hubAllocator) Subnet() netip.Prefix {
	return a.subnet
}

// Alloc 为 (mesh, nodeID) 分配虚拟 IP。已分配过则返回旧值（稳定）；否则递增分配，
// 跳过已用地址；耗尽时返回错误。
func (a *hubAllocator) Alloc(mesh, nodeID string) (netip.Addr, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := allocKey(mesh, nodeID)
	if vip, ok := a.assigned[key]; ok {
		return vip, nil
	}
	vip, ok := a.nextFreeLocked()
	if !ok {
		return netip.Addr{}, fmt.Errorf("虚拟子网 %s 地址空间耗尽", a.subnet)
	}
	a.assigned[key] = vip
	a.used[vip] = key
	return vip, nil
}

// nextFreeLocked 在调用方已持有 a.mu 的前提下寻找下一个空闲地址。
// 先回扫 [2, next) 复用 Release 释放的空洞（紧凑复用），再从前向游标 next 递增到
// maxHost-1（首次分配递增）。释放的地址优先复用，避免地址空间碎片化。
func (a *hubAllocator) nextFreeLocked() (netip.Addr, bool) {
	for i := uint64(2); i < a.next; i++ {
		addr := a.addrAt(i)
		if _, used := a.used[addr]; !used {
			a.next = i + 1
			return addr, true
		}
	}
	for i := a.next; i < a.maxHost; i++ {
		addr := a.addrAt(i)
		if _, used := a.used[addr]; !used {
			a.next = i + 1
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// addrAt 返回子网基址偏移 i 处的 IPv4 地址。
func (a *hubAllocator) addrAt(i uint64) netip.Addr {
	return u64ToIPv4(a.base + i)
}

// Release 释放 (mesh, nodeID) 的虚拟 IP，回收复用。next 游标不回退（避免地址抖动），
// 释放的地址经 nextFreeLocked 回扫复用。
func (a *hubAllocator) Release(mesh, nodeID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := allocKey(mesh, nodeID)
	if vip, ok := a.assigned[key]; ok {
		delete(a.assigned, key)
		delete(a.used, vip)
	}
}

// Reserve 把已持久化的 (mesh, nodeID)→虚拟 IP 灌回分配器（重启快照重建）。
// 冲突（同一 key 已绑其他 VIP / 同一 VIP 已被其他 key 占用 / 不在子网内）返回错误。
func (a *hubAllocator) Reserve(mesh, nodeID string, vip netip.Addr) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.reserveLocked(mesh, nodeID, vip)
}

func (a *hubAllocator) reserveLocked(mesh, nodeID string, vip netip.Addr) error {
	if !vip.IsValid() || !vip.Is4() || !a.subnet.Contains(vip) {
		return fmt.Errorf("虚拟 IP %s 不在子网 %s 内", vip, a.subnet)
	}
	key := allocKey(mesh, nodeID)
	if old, ok := a.assigned[key]; ok && old != vip {
		return fmt.Errorf("虚拟 IP 冲突：%s 已分配 %s，尝试保留 %s", key, old, vip)
	}
	if owner, ok := a.used[vip]; ok && owner != key {
		return fmt.Errorf("虚拟 IP %s 已被 %s 占用", vip, owner)
	}
	a.assigned[key] = vip
	a.used[vip] = key
	if off := a.ipv4Offset(vip); off >= a.next {
		a.next = off + 1 // 递增游标跳过已保留地址，避免把持久化的 VIP 再分给新节点。
	}
	return nil
}

// ReserveSnapshot 把快照中全部带虚拟 IP 的节点灌回分配器（重启快照重建）。
// 逐个保留，冲突累积返回首个错误（其余仍尝试保留）。
func (a *hubAllocator) ReserveSnapshot(snap *Snapshot) error {
	if snap == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var firstErr error
	for _, ns := range snap.Nodes {
		if !ns.VirtualIP.IsValid() {
			continue
		}
		if err := a.reserveLocked(ns.Mesh, string(ns.ID), ns.VirtualIP); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// ipv4Offset 返回虚拟 IP 相对子网基址的主机偏移。
func (a *hubAllocator) ipv4Offset(ip netip.Addr) uint64 {
	return ipv4ToU64(ip) - a.base
}

// PreloadAllocator 把快照中带虚拟 IP 的节点灌回分配器（重启快照重建）。
// 仅 hubAllocator 支持保留（ReserveSnapshot）；deterministicAllocator 无状态、
// 确定性哈希无需重建，直接忽略。返回首个保留冲突错误（其余仍尝试保留）。
func PreloadAllocator(a Allocator, snap *Snapshot) error {
	if a == nil || snap == nil {
		return nil
	}
	if ha, ok := a.(*hubAllocator); ok {
		return ha.ReserveSnapshot(snap)
	}
	return nil
}

// ipv4ToU64 把 IPv4 地址转为一个 uint64（便于算术）。
func ipv4ToU64(ip netip.Addr) uint64 {
	b := ip.As4()
	return uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
}

// u64ToIPv4 把一个 uint64 转回 IPv4 地址。
func u64ToIPv4(v uint64) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
