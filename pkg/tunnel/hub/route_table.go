// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hub 提供星型中继网络的 Hub 端实现。
//
// Hub 维护节点路由表（NodeID → mux.Mux），
// 为中继请求提供目标节点查找和转发能力。
package hub

import (
	"sort"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// NodeID 是节点唯一标识符。
type NodeID string

// NodeInfo 包含已注册节点的信息。
type NodeInfo struct {
	ID        NodeID
	Mux       *mux.Mux
	Connected time.Time // 连接时间
	Addr      string    // 远端地址
	Secret    string    // per-node 独立 secret（仅节点声明 per-node-secret 能力时下发；不落日志）
}

// RouteTable 是线程安全的节点路由表。
type RouteTable struct {
	mu       sync.RWMutex
	nodes    map[NodeID]*mux.Mux
	info     map[NodeID]NodeInfo  // 扩展信息
	services map[NodeID][]Service // 节点宣告的服务（mesh 选路）
	// onRemove 是节点真正从路由表移除时的回调（Remove / RemoveIfOwned 成功路径）。
	// 供上层（如 server 的 SignalBroker）在节点下线时清理 per-node 状态（I6 收件箱）。
	// 在锁外同步调用，必须快速返回；nil 表示未注册。
	onRemove func(NodeID)
}

// SetRemoveHook 注册节点移除回调：节点真正从路由表移除（Remove / RemoveIfOwned
// 成功）后、在锁外调用一次 fn(id)。传 nil 清除回调。回调应快速返回（不做阻塞
// I/O），需异步自行 go。
func (rt *RouteTable) SetRemoveHook(fn func(NodeID)) {
	rt.mu.Lock()
	rt.onRemove = fn
	rt.mu.Unlock()
}

// NewRouteTable 创建路由表。
func NewRouteTable() *RouteTable {
	return &RouteTable{
		nodes:    make(map[NodeID]*mux.Mux),
		info:     make(map[NodeID]NodeInfo),
		services: make(map[NodeID][]Service),
	}
}

// Add 注册一个节点。如果节点 ID 已存在，先关闭旧连接再替换。
func (rt *RouteTable) Add(id NodeID, m *mux.Mux) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if old, ok := rt.nodes[id]; ok {
		go old.Close() // 异步关闭旧连接
	}
	rt.nodes[id] = m
}

// AddWithInfo 注册节点并保存扩展信息。
func (rt *RouteTable) AddWithInfo(info NodeInfo) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if old, ok := rt.nodes[info.ID]; ok {
		go old.Close()
	}
	rt.nodes[info.ID] = info.Mux
	rt.info[info.ID] = info
}

// AddWithInfoAndServices 原子地注册节点并写入服务宣告。
// 与分两次调用 AddWithInfo + SetServices 相比，消除了重连时短暂残留
// 旧服务宣告的非原子窗口（S4）。空/ nil svcs 等价于清除该节点的旧宣告。
func (rt *RouteTable) AddWithInfoAndServices(info NodeInfo, svcs []Service) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if old, ok := rt.nodes[info.ID]; ok {
		go old.Close()
	}
	rt.nodes[info.ID] = info.Mux
	rt.info[info.ID] = info
	if rt.services == nil {
		rt.services = make(map[NodeID][]Service)
	}
	if len(svcs) == 0 {
		delete(rt.services, info.ID)
		return
	}
	rt.services[info.ID] = svcs
}

// Remove 移除一个节点。返回是否真正移除（节点存在）。
// 移除成功后在锁外触发 onRemove 回调（若注册）。
func (rt *RouteTable) Remove(id NodeID) bool {
	rt.mu.Lock()
	m, ok := rt.nodes[id]
	if !ok {
		rt.mu.Unlock()
		return false
	}
	delete(rt.nodes, id)
	delete(rt.info, id)
	delete(rt.services, id)
	fn := rt.onRemove
	rt.mu.Unlock()
	if m != nil {
		go func() { _ = m.Close() }()
	}
	if fn != nil {
		fn(id)
	}
	return true
}

// RemoveIfOwned 仅当该节点 ID 当前绑定到给定 mux（即本连接）时才移除。
// 防止旧连接断开时误删新注册的同名节点（stale identity 防护）。
// 返回是否真正移除。移除成功后在锁外触发 onRemove 回调（若注册）。
func (rt *RouteTable) RemoveIfOwned(id NodeID, m *mux.Mux) bool {
	rt.mu.Lock()
	cur, ok := rt.nodes[id]
	if !ok || cur != m {
		rt.mu.Unlock()
		return false
	}
	delete(rt.nodes, id)
	delete(rt.info, id)
	delete(rt.services, id)
	fn := rt.onRemove
	rt.mu.Unlock()
	if fn != nil {
		fn(id)
	}
	return true
}

// Has 检查节点是否存在。
func (rt *RouteTable) Has(id NodeID) bool {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	_, ok := rt.nodes[id]
	return ok
}

// Lookup 按 ID 查找节点的 Mux 连接。
// 未找到时返回 nil。
func (rt *RouteTable) Lookup(id NodeID) *mux.Mux {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return rt.nodes[id]
}

// LookupInfo 按 ID 查找节点的扩展信息。
// 同时确认 nodes 与 info 两表均存在该节点；节点不存在时返回 false。
func (rt *RouteTable) LookupInfo(id NodeID) (NodeInfo, bool) {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	nfo, ok := rt.info[id]
	if !ok {
		return NodeInfo{}, false
	}
	if _, ok := rt.nodes[id]; !ok {
		return NodeInfo{}, false
	}
	return nfo, true
}

// List 返回所有已注册节点的列表。
func (rt *RouteTable) List() []NodeInfo {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	result := make([]NodeInfo, 0, len(rt.nodes))
	for id, m := range rt.nodes {
		nfo := rt.info[id]
		nfo.ID = id
		nfo.Mux = m
		result = append(result, nfo)
	}
	return result
}

// NodeService 是 ListServices 返回的一个服务条目（节点 + 服务）。
type NodeService struct {
	Node    NodeID
	Service Service
}

// ListServices 返回所有节点宣告的服务，按 (node, name) 稳定排序（I3）。
// 客户端据此确定性选路；多节点同名服务保持多候选（failover 语义不破坏）。
func (rt *RouteTable) ListServices() []NodeService {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	var out []NodeService
	for id, svcs := range rt.services {
		for _, s := range svcs {
			out = append(out, NodeService{Node: id, Service: s})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Node != out[j].Node {
			return out[i].Node < out[j].Node
		}
		return out[i].Service.Name < out[j].Service.Name
	})
	return out
}

// NodeCount 返回当前注册的节点数量。
func (rt *RouteTable) NodeCount() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.nodes)
}
