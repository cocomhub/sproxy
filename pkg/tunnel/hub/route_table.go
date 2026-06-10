// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package hub 提供星型中继网络的 Hub 端实现。
//
// Hub 维护节点路由表（NodeID → mux.Mux），
// 为中继请求提供目标节点查找和转发能力。
package hub

import (
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// NodeID 是节点唯一标识符。
type NodeID string

// NodeInfo 包含已注册节点的信息。
type NodeInfo struct {
	ID  NodeID
	Mux *mux.Mux
}

// RouteTable 是线程安全的节点路由表。
type RouteTable struct {
	mu    sync.RWMutex
	nodes map[NodeID]*mux.Mux
}

// NewRouteTable 创建路由表。
func NewRouteTable() *RouteTable {
	return &RouteTable{
		nodes: make(map[NodeID]*mux.Mux),
	}
}

// Add 注册一个节点。
func (rt *RouteTable) Add(id NodeID, m *mux.Mux) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.nodes[id] = m
}

// Remove 移除一个节点。
func (rt *RouteTable) Remove(id NodeID) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	delete(rt.nodes, id)
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
		result = append(result, NodeInfo{ID: id, Mux: m})
	}
	return result
}
