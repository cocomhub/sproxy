// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"sort"
	"strings"
	"time"
)

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
// M2：跳过 disc-* 临时发现节点——这类临时身份由 mesh 自动对等发现拨号时注册
// （disc-<base>-<unixnano>），拨号完成即由对端以真实 node-id 重注册。若在快照
// 时刻恰好在线被持久化，重启后既不会重连（临时身份已注销）也无人再用，会留下
// 永久 nil-Mux 幽灵条目。故持久化时一律过滤，避免污染恢复后的节点列表。
func makeNodeSnaps(mesh string, nodes []NodeInfo, t *RouteTable) []NodeSnap {
	snaps := make([]NodeSnap, 0, len(nodes))
	for _, n := range nodes {
		if strings.HasPrefix(string(n.ID), DiscPrefix+"-") {
			continue
		}
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
// M3：快照只保留未过期消息——队列的惰性过期（compactExpiredLocked）只在 Push/Peek/
// Pop 时发生，一个无消息的空 poll 不会触发过期，若镜像原样复制会把已过期消息
// 残留在持久化文件里直到下次写盘。这里在生成快照时就按 signalMsgTTL 过滤，
// 保证**任何**持久化镜像（onChange / FlushSignal / 停服 Flush）都不含过期死信。
func SnapshotSignalQueue(q *SignalQueue) []MessageSnap {
	if q == nil {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	peers := make([]string, 0, len(q.inboxes))
	for peer := range q.inboxes {
		peers = append(peers, peer)
	}
	sort.Strings(peers) // 确定性顺序
	out := make([]MessageSnap, 0, len(peers))
	for _, peer := range peers {
		inbox := q.inboxes[peer]
		msgs := make([]SignalMsg, 0, len(inbox))
		for _, m := range inbox {
			if signalMsgExpired(m, now) {
				continue
			}
			msgs = append(msgs, m)
		}
		if len(msgs) > 0 {
			out = append(out, MessageSnap{Peer: peer, Msgs: msgs})
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// RestoreSignalQueue 将快照的信令收件箱恢复到队列。
// 恢复时即过滤已过期消息（M3）：过期死信不重投递（与惰性过期语义一致，重启后
// 不再投递已过期的消息），且不把过期消息计入 q.total——否则 q.total 在下次
// 惰性清理前被高估，白白占用全局配额。
func RestoreSignalQueue(q *SignalQueue, msgs []MessageSnap) {
	if q == nil {
		return
	}
	now := time.Now()
	for _, ms := range msgs {
		if ms.Peer == "" {
			continue
		}
		inbox := make([]SignalMsg, 0, len(ms.Msgs))
		for _, m := range ms.Msgs {
			if signalMsgExpired(m, now) {
				continue
			}
			inbox = append(inbox, m)
		}
		if len(inbox) == 0 {
			continue
		}
		q.mu.Lock()
		q.inboxes[ms.Peer] = inbox
		q.total += len(inbox)
		q.mu.Unlock()
	}
}
