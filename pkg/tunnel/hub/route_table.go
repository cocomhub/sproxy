// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hub 提供星型中继网络的 Hub 端实现。
//
// Hub 维护节点路由表（NodeID → mux.Mux），
// 为中继请求提供目标节点查找和转发能力。
package hub

import (
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
	Token     string    // 使用的 token（脱敏）
}

// RouteTable 是线程安全的节点路由表。
type RouteTable struct {
	mu       sync.RWMutex
	nodes    map[NodeID]*mux.Mux
	info     map[NodeID]NodeInfo  // 扩展信息
	services map[NodeID][]Service // 节点宣告的服务（mesh 选路）
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

// Remove 移除一个节点。
func (rt *RouteTable) Remove(id NodeID) {
	rt.mu.Lock()
	if m, ok := rt.nodes[id]; ok {
		delete(rt.nodes, id)
		delete(rt.info, id)
		delete(rt.services, id)
		if m != nil {
			go func() { _ = m.Close() }()
		}
	}
	rt.mu.Unlock()
}

// RemoveIfOwned 仅当该节点 ID 当前绑定到给定 mux（即本连接）时才移除。
// 防止旧连接断开时误删新注册的同名节点（stale identity 防护）。
// 返回是否真正移除。
func (rt *RouteTable) RemoveIfOwned(id NodeID, m *mux.Mux) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	cur, ok := rt.nodes[id]
	if !ok || cur != m {
		return false
	}
	delete(rt.nodes, id)
	delete(rt.info, id)
	delete(rt.services, id)
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

// NodeCount 返回当前注册的节点数量。
func (rt *RouteTable) NodeCount() int {
	rt.mu.RLock()
	defer rt.mu.RUnlock()
	return len(rt.nodes)
}
