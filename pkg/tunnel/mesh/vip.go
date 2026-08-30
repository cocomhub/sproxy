// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"fmt"
	"hash/fnv"
	"net/netip"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// vipEntry 是 VipTable 中一个虚拟 IP 的条目。
type vipEntry struct {
	// NodeID 是该虚拟 IP 归属的节点。
	NodeID string
	// Name 是预留的 DNS 名（R-1：虚拟 IP 的域名解析，第一版不消费）。
	Name string
}

// VipTable 维护虚拟 IP → 节点映射（本地视图）。
//
// 数据源防注入（设计 §5.1）：只接受**认证数据源**填充——
//   - hub 节点列表（SproxySig 签名 /api/hub/nodes，带 virtual_ip）；
//   - mDNS 签名 TXT（--mdns-secret HMAC）。
//
// 一次性 CLI（mesh connect <vip>:<port>）与常驻 mesh node 各持一份，无跨进程
// 一致性要求。线程安全。
type VipTable struct {
	mu     sync.RWMutex
	subnet netip.Prefix
	byAddr map[netip.Addr]vipEntry
}

// NewVipTable 创建虚拟 IP 表（锁定子网，仅 IPv4）。
func NewVipTable(subnet netip.Prefix) *VipTable {
	if !subnet.Addr().Is4() {
		panic(fmt.Sprintf("mesh: NewVipTable 需要 IPv4 虚拟子网，got %s", subnet))
	}
	return &VipTable{subnet: subnet.Masked(), byAddr: make(map[netip.Addr]vipEntry)}
}

// Subnet 返回本表锁定的虚拟子网。
func (t *VipTable) Subnet() netip.Prefix {
	return t.subnet
}

// Add 记录虚拟 IP → 节点映射（增量语义，供 mDNS 等非权威源的逐条学习）。
//
// 安全（VIP 注入面）：addr 已映射到**不同** nodeID 时**拒绝**（返回 false），保留
// 既有映射——防恶意/错误数据源劫持 VIP 流量。同一 nodeID 重复 Add 幂等覆盖（返回
// true）。注意：纯 Add 的 first-writer-wins 在 mDNS LAN 信任模型下仍可被抢占（攻击者
// 先声明任意 VIP），故 mDNS 模式应使用 AddVerified（校验与确定性分配一致）；hub 模式
// 应使用 Reconcile（从签名 hub 列表原子重建）。
func (t *VipTable) Add(addr netip.Addr, nodeID string) bool {
	if !IsVirtualAddr(addr, t.subnet) {
		return false // 子网外地址拒绝。
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if e, ok := t.byAddr[addr]; ok {
		if e.NodeID != nodeID {
			return false // 冲突：拒绝覆盖，防 VIP 劫持。
		}
		return true // 幂等：同一节点刷新。
	}
	t.byAddr[addr] = vipEntry{NodeID: nodeID}
	return true
}

// VipEntry 是 VipTable 的一条权威映射（Reconcile 重建用）。
type VipEntry struct {
	Addr   netip.Addr
	NodeID string
}

// Reconcile 从**权威数据源**（hub 节点列表，SproxySig 签名）原子重建整表：
// 清空全部条目后填入 entries。hub 权威分配在 mesh 隔离内唯一，不依赖"谁先声明"；
// 每次刷新重建同时清除陈旧/离线节点的残留映射。
//
// 防御性校验：entries 含子网外地址或同一 addr 映射到不同 nodeID 时拒绝（返回 false，
// 保持原表不变）——hub 异常/恶意源不破坏既有表。
func (t *VipTable) Reconcile(entries []VipEntry) bool {
	seen := make(map[netip.Addr]string, len(entries))
	for _, e := range entries {
		if !IsVirtualAddr(e.Addr, t.subnet) {
			return false // 子网外地址拒绝。
		}
		if prev, dup := seen[e.Addr]; dup && prev != e.NodeID {
			return false // 权威源内冲突（异常），拒绝重建。
		}
		seen[e.Addr] = e.NodeID
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(entries) == 0 {
		t.byAddr = make(map[netip.Addr]vipEntry)
		return true
	}
	rebuilt := make(map[netip.Addr]vipEntry, len(entries))
	for _, e := range entries {
		rebuilt[e.Addr] = vipEntry{NodeID: e.NodeID}
	}
	t.byAddr = rebuilt
	return true
}

// AddVerified 校验声明虚拟 IP 与**确定性分配结果**一致后才接受（mDNS 无 hub 模式）。
// 不接受对端自声明的绑定值——只接受与 deterministicAllocator(mesh, nodeID) 公式一致
// 者，攻击者无法声明任意 VIP 抢占他人（公式可预测性在 LAN 信任模型内是设计边界）。
// 子网外地址 / 与确定性不一致 / 与既有映射冲突均拒绝。
func (t *VipTable) AddVerified(addr netip.Addr, nodeID, mesh string, alloc hub.Allocator) bool {
	if !IsVirtualAddr(addr, t.subnet) {
		return false // 子网外地址拒绝。
	}
	expected, err := alloc.Alloc(mesh, nodeID)
	if err != nil || expected != addr {
		return false // 与确定性分配不一致：拒绝自声明抢占。
	}
	return t.Add(addr, nodeID)
}

// NodeByAddr 反查虚拟 IP → 节点 ID。
func (t *VipTable) NodeByAddr(addr netip.Addr) (string, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	e, ok := t.byAddr[addr]
	return e.NodeID, ok
}

// AddrByNode 正查节点 ID → 虚拟 IP。
func (t *VipTable) AddrByNode(nodeID string) (netip.Addr, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	for addr, e := range t.byAddr {
		if e.NodeID == nodeID {
			return addr, true
		}
	}
	return netip.Addr{}, false
}

// Len 返回表内条目数（调试/测试用）。
func (t *VipTable) Len() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return len(t.byAddr)
}

// deterministicAllocator 是 mDNS 无 hub 模式的本地确定性虚拟 IP 分配器
// （实现 hub.Allocator）。vip = 子网基址 + hash(mesh, nodeID) % (宿主-3) + 2，
// 无状态、无 hub 可用。仅 IPv4（虚拟 IP 分配做 IPv4 算术）。
//
// 冲突策略（安全审查 MEDIUM 闭环）：
//   - **hub 权威优先**：有 hub 时虚拟 IP 由 hub 递增分配（REG_OK 下发），确定性
//     分配器仅作 mDNS 无 hub 回落——两个实现互斥，不共存。
//   - **确定性分配本身无状态，不做全局冲突检测**；冲突由**学习侧**处理：对端宣告
//     /hub 列表的虚拟 IP 经 VipTable.Add 学习，同一 addr 被不同 nodeID 声明时**拒绝**
//     （保留既有映射 + 告警，见 discovery.go/mdns_node.go），被拒对端无法抢占/劫持
//     VIP 流量。
//   - **LAN 信任模型边界**：mDNS 无 hub 场景下，同网段可组播者本就可自选 node-id
//     （LAN 信任模型），可预测/抢占确定性 VIP（哈希公式公开）。该边界已文档化；
//     有 hub 时权威分配 + 冲突拒绝消除该风险（注册准入 SproxySig + HMAC proof）。
//   - 退避：碰撞不触发重算（无状态），被拒方仅在本表不可达该 VIP，不影响其既有
//     服务寻址（服务名/--node 拨号路径独立可用）。
type deterministicAllocator struct {
	subnet netip.Prefix
}

// newDeterministicAllocator 创建确定性分配器（锁定子网，仅 IPv4）。
func newDeterministicAllocator(subnet netip.Prefix) *deterministicAllocator {
	if !subnet.Addr().Is4() {
		panic(fmt.Sprintf("mesh: deterministicAllocator 需要 IPv4 虚拟子网，got %s", subnet))
	}
	return &deterministicAllocator{subnet: subnet.Masked()}
}

// Alloc 返回 (mesh, nodeID) 的确定性虚拟 IP（稳定：同输入同输出）。
func (a *deterministicAllocator) Alloc(mesh, nodeID string) (netip.Addr, error) {
	size := uint64(1) << (32 - a.subnet.Bits())
	if size < 3 {
		return netip.Addr{}, fmt.Errorf("虚拟子网 %s 无可分配主机", a.subnet)
	}
	h := hashMeshNode(mesh, nodeID)
	// 可分配偏移 [2, size-2]（.0 网络/.1 网关/.尾 广播保留），共 size-3 个。
	off := 2 + h%(size-3)
	base := a.subnet.Masked().Addr()
	return u64ToIPv4(ipv4ToU64(base) + off), nil
}

// Release 是 no-op（无状态分配器，无需回收）。
func (a *deterministicAllocator) Release(mesh, nodeID string) {}

// Subnet 返回分配器使用的虚拟子网。
func (a *deterministicAllocator) Subnet() netip.Prefix {
	return a.subnet
}

// hashMeshNode 计算 (mesh, nodeID) 的 FNV-1a 64 位哈希（确定性分配用）。
func hashMeshNode(mesh, nodeID string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(mesh))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(nodeID))
	return h.Sum64()
}

// IsVirtualAddr 判断 addr 是否为指定虚拟子网内的 IPv4 地址。
func IsVirtualAddr(addr netip.Addr, subnet netip.Prefix) bool {
	return addr.IsValid() && addr.Is4() && subnet.Contains(addr)
}

// ParseVirtualAddr 把 host 解析为 IP 地址（供 mesh connect <vip>:<port> 识别虚拟 IP
// host 段）。带端口或非法输入返回 false。
func ParseVirtualAddr(host string) (netip.Addr, bool) {
	if host == "" {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return addr, true
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

// ipv4Offset 返回 addr 相对 subnet 基址的主机偏移。
func ipv4Offset(subnet netip.Prefix, addr netip.Addr) uint64 {
	return ipv4ToU64(addr) - ipv4ToU64(subnet.Masked().Addr())
}

// compile-time check：deterministicAllocator 实现 hub.Allocator。
var _ hub.Allocator = (*deterministicAllocator)(nil)
