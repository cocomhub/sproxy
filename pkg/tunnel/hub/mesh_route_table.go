// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// MeshRouteTable 是每 mesh 独立 RouteTable 的聚合：map[mesh]*RouteTable + nodeID→mesh 映射。
// 转发/列表/信令按 mesh 隔离；无 mesh（""）为默认表，行为等价单 mesh。
type MeshRouteTable struct {
	mu       sync.RWMutex
	tables   map[string]*RouteTable // meshID → 单 mesh 路由表
	nodeMesh map[NodeID]string      // nodeID → mesh（转发时查目标 mesh；LookupInfo 需 mesh 校验时用）
	// onRemove 是为每个内部 RouteTable 注册的节点移除回调（SetRemoveHook 设置）。
	// 惰性新建的表（Table）会同步挂上该回调，保证 SignalBroker 收件箱清理全覆盖。
	onRemove func(NodeID)
}

// NewMeshRouteTable 创建每 mesh 独立路由表的聚合。
func NewMeshRouteTable() *MeshRouteTable {
	return &MeshRouteTable{
		tables:   make(map[string]*RouteTable),
		nodeMesh: make(map[NodeID]string),
	}
}

// Table 获取某 mesh 的路由表，不存在时惰性创建。
// 创建时若已注册 SetRemoveHook，新表同步挂上（收件箱清理全覆盖）。
func (mrt *MeshRouteTable) Table(mesh string) *RouteTable {
	mrt.mu.Lock()
	defer mrt.mu.Unlock()
	rt, ok := mrt.tables[mesh]
	if !ok {
		rt = NewRouteTable()
		if mrt.onRemove != nil {
			rt.SetRemoveHook(mrt.onRemove)
		}
		mrt.tables[mesh] = rt
	}
	return rt
}

// tableOf 仅读取某 mesh 的路由表，不存在时不创建（返回 false）。
func (mrt *MeshRouteTable) tableOf(mesh string) (*RouteTable, bool) {
	mrt.mu.RLock()
	rt, ok := mrt.tables[mesh]
	mrt.mu.RUnlock()
	return rt, ok
}

// lookupMesh 读取节点所属 mesh。
func (mrt *MeshRouteTable) lookupMesh(id NodeID) (string, bool) {
	mrt.mu.RLock()
	mesh, ok := mrt.nodeMesh[id]
	mrt.mu.RUnlock()
	return mesh, ok
}

// Add 写入对应表（info.Mesh = mesh）。
// 若同一节点 ID 此前属于另一 mesh（跨 mesh 同名重注册），先从旧 mesh 表移除，
// 维持 nodeMesh 单一归属的隔离不变量（节点名不跨 mesh 共享）。
func (mrt *MeshRouteTable) Add(mesh string, info NodeInfo, svcs []Service) {
	info.Mesh = mesh
	t := mrt.Table(mesh)
	t.AddWithInfoAndServices(info, svcs)
	mrt.mu.Lock()
	if prev, exists := mrt.nodeMesh[info.ID]; exists && prev != mesh {
		if pt, ok := mrt.tables[prev]; ok {
			pt.Remove(info.ID)
		}
	}
	mrt.nodeMesh[info.ID] = mesh
	mrt.mu.Unlock()
}

// AddNode 低层节点注册（Add 的变体，不携带服务宣告）。
func (mrt *MeshRouteTable) AddNode(mesh string, id NodeID, m *mux.Mux) {
	mrt.Add(mesh, NodeInfo{ID: id, Mux: m}, nil)
}

// Lookup 按 ID 查找节点的 Mux 连接（转发用）：查 nodeMesh[id] → 对应表 Lookup。
// 未找到时返回 nil。节点只属于一个 mesh，跨 mesh 的 ID 不可达。
func (mrt *MeshRouteTable) Lookup(id NodeID) *mux.Mux {
	mesh, ok := mrt.lookupMesh(id)
	if !ok {
		return nil
	}
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return nil
	}
	return t.Lookup(id)
}

// LookupInfo 按 ID 查找节点的扩展信息（NodeInfo 含 Mesh，供 mesh 校验）。
func (mrt *MeshRouteTable) LookupInfo(id NodeID) (NodeInfo, bool) {
	mesh, ok := mrt.lookupMesh(id)
	if !ok {
		return NodeInfo{}, false
	}
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return NodeInfo{}, false
	}
	return t.LookupInfo(id)
}

// Has 检查节点是否存在（按 nodeMesh 自动定位所属 mesh）。
func (mrt *MeshRouteTable) Has(id NodeID) bool {
	mesh, ok := mrt.lookupMesh(id)
	if !ok {
		return false
	}
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return false
	}
	return t.Has(id)
}

// cleanupNodeMesh 在节点已从某 mesh 表移除后清理 nodeMesh 映射与空表。
// 仅当 nodeMesh[id] 仍指向该 mesh 且表中已无该节点时才清理——防止与并发的
// Add 重注册竞争：同名节点刚被重新加入（同 mesh 或换 mesh）时不误删其归属。
// 调用方必须已持有 mrt.mu。
func (mrt *MeshRouteTable) cleanupNodeMesh(id NodeID, mesh string, t *RouteTable) {
	if cur, exists := mrt.nodeMesh[id]; !exists || cur != mesh {
		return
	}
	if t.Has(id) {
		// 并发 Add 已把同 ID 重新加入该 mesh 表：保留 nodeMesh 归属。
		return
	}
	delete(mrt.nodeMesh, id)
	if t.NodeCount() == 0 {
		delete(mrt.tables, mesh)
	}
}

// Remove 按 ID 移除节点（自动按 nodeMesh 定位所属 mesh）。
// 移除成功后删除 nodeMesh 映射并清空该 mesh 的空表。
func (mrt *MeshRouteTable) Remove(id NodeID) bool {
	mesh, ok := mrt.lookupMesh(id)
	if !ok {
		return false
	}
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return false
	}
	if !t.Remove(id) {
		return false
	}
	mrt.mu.Lock()
	mrt.cleanupNodeMesh(id, mesh, t)
	mrt.mu.Unlock()
	return true
}

// RemoveIfOwned 仅当节点 ID 当前绑定到给定 mux（即本连接）时才从对应 mesh 移除。
// 防止旧连接断开时误删新注册的同名节点（stale identity 防护）。
func (mrt *MeshRouteTable) RemoveIfOwned(id NodeID, m *mux.Mux) bool {
	mesh, ok := mrt.lookupMesh(id)
	if !ok {
		return false
	}
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return false
	}
	if !t.RemoveIfOwned(id, m) {
		return false
	}
	mrt.mu.Lock()
	mrt.cleanupNodeMesh(id, mesh, t)
	mrt.mu.Unlock()
	return true
}

// List 返回某 mesh 的节点列表（/api/hub/nodes 用）。
func (mrt *MeshRouteTable) List(mesh string) []NodeInfo {
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return nil
	}
	return t.List()
}

// ListServices 返回某 mesh 的服务宣告（mesh 选路用）。
func (mrt *MeshRouteTable) ListServices(mesh string) []NodeService {
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return nil
	}
	return t.ListServices()
}

// NodeCount 返回某 mesh 的节点数（metrics 用）。
func (mrt *MeshRouteTable) NodeCount(mesh string) int {
	t, ok := mrt.tableOf(mesh)
	if !ok {
		return 0
	}
	return t.NodeCount()
}

// MeshOf 返回节点所属 mesh（未注册返回空串）。
func (mrt *MeshRouteTable) MeshOf(id NodeID) string {
	mesh, _ := mrt.lookupMesh(id)
	return mesh
}

// SetRemoveHook 为每个内部 RouteTable 挂 onRemove 回调（SignalBroker 收件箱清理）。
// 已存在的表立即生效；之后惰性新建的表（Table）自动带上该回调。
func (mrt *MeshRouteTable) SetRemoveHook(fn func(NodeID)) {
	mrt.mu.Lock()
	mrt.onRemove = fn
	tables := make([]*RouteTable, 0, len(mrt.tables))
	for _, t := range mrt.tables {
		tables = append(tables, t)
	}
	mrt.mu.Unlock()
	for _, t := range tables {
		t.SetRemoveHook(fn)
	}
}

// AllMeshes 返回所有已存在的 mesh 列表（debug/管理用）。
func (mrt *MeshRouteTable) AllMeshes() []string {
	mrt.mu.RLock()
	defer mrt.mu.RUnlock()
	out := make([]string, 0, len(mrt.tables))
	for mesh := range mrt.tables {
		out = append(out, mesh)
	}
	return out
}
