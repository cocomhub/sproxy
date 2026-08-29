// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "sort"

// SnapshotRouteTable 捕获 MeshRouteTable 全部节点注册（含 mesh、服务、secret），
// 生成 Snapshot 的 Nodes 部分。Mux（在线连接）不捕获——重启后节点重连建立。
func SnapshotRouteTable(mrt *MeshRouteTable) *Snapshot {
	if mrt == nil {
		return &Snapshot{}
	}
	return &Snapshot{Nodes: snapshotRouteTableNodes(mrt)}
}

// snapshotRouteTableNodes 遍历所有 mesh，收集每节点 NodeSnap。
func snapshotRouteTableNodes(mrt *MeshRouteTable) []NodeSnap {
	var out []NodeSnap
	for _, mesh := range mrt.AllMeshes() {
		t := mrt.Table(mesh)
		nodes := t.List()
		out = append(out, makeNodeSnaps(mesh, nodes, t)...)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// makeNodeSnaps 把某 mesh 节点列表转成 NodeSnap 序列（服务宣告取该表当前值）。
func makeNodeSnaps(mesh string, nodes []NodeInfo, t *RouteTable) []NodeSnap {
	snaps := make([]NodeSnap, 0, len(nodes))
	for _, n := range nodes {
		snaps = append(snaps, NodeSnap{
			ID:         n.ID,
			Mesh:       mesh,
			Addr:       n.Addr,
			Secret:     n.Secret,
			RealNodeID: n.RealNodeID,
			Connected:  n.Connected,
			Services:   t.ServicesOf(n.ID),
		})
	}
	return snaps
}

// RestoreFromSnapshot 将快照恢复到空的 MeshRouteTable（幂等调用会重复写入相似数据，
// 仅应在启动时对空表调用一次）。
// 恢复的节点没有在线 Mux（nil），等待客户端重连后由 HandleConn 重写 info（含新 Mux
// 与新 secret），保证重启后节点身份/服务不丢失而连接面在线。
func RestoreFromSnapshot(mrt *MeshRouteTable, snap *Snapshot) {
	if mrt == nil || snap == nil {
		return
	}
	for _, ns := range snap.Nodes {
		if ns.ID == "" {
			continue
		}
		mrt.Add(ns.Mesh, NodeInfo{
			ID:         ns.ID,
			Addr:       ns.Addr,
			Secret:     ns.Secret,
			RealNodeID: ns.RealNodeID,
			Connected:  ns.Connected,
		}, ns.Services)
	}
}

// SnapshotSignalQueue 捕获 SignalQueue 全部 per-peer 收件箱为 MessageSnap 序列。
func SnapshotSignalQueue(q *SignalQueue) []MessageSnap {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	peers := make([]string, 0, len(q.inboxes))
	for peer := range q.inboxes {
		peers = append(peers, peer)
	}
	sort.Strings(peers) // 确定性顺序
	out := make([]MessageSnap, 0, len(peers))
	for _, peer := range peers {
		inbox := q.inboxes[peer]
		msgs := make([]SignalMsg, len(inbox))
		copy(msgs, inbox)
		out = append(out, MessageSnap{Peer: peer, Msgs: msgs})
	}
	return out
}

// RestoreSignalQueue 将快照的信令收件箱恢复到队列。
func RestoreSignalQueue(q *SignalQueue, msgs []MessageSnap) {
	if q == nil {
		return
	}
	for _, ms := range msgs {
		if ms.Peer == "" {
			continue
		}
		inbox := make([]SignalMsg, 0, len(ms.Msgs))
		inbox = append(inbox, ms.Msgs...)
		q.mu.Lock()
		q.inboxes[ms.Peer] = inbox
		q.total += len(inbox)
		q.mu.Unlock()
	}
}
