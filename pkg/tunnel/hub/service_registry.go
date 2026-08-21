// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

// 本文件承载 mesh 选路所需的服务可达性模型。
// 期3 起使用；期1 的 Service 宣告已就绪（RouteTable.SetServices 等见 router.go）。

// Path 描述从调用方到目标服务的一条数据路径。
type Path struct {
	// Via 是路径上的中转节点（hub 中继时的叶子/出口节点；
	// 直连时为调用方自身）。
	Via []NodeID
	// Kind 是路径类型：direct | webrtc | relay | exit。
	Kind string
	// Addr 是最终数据面地址（出口节点内网可达地址或目标节点本身）。
	Addr string
}

// Reachability 描述调用方视角的可达性摘要。
type Reachability struct {
	// Nodes 是当前在线节点列表。
	Nodes []NodeInfo
}

// PlanKind 反映选路优先级。越靠前的 Kind 越优。
var PlanKindRank = map[string]int{
	"direct": 0,
	"webrtc": 1,
	"relay":  2,
	"exit":   3,
	"none":   4,
}

// BetterPath 比较两条候选路径在当前调用方视图下的优劣。
func BetterPath(a, b Path) bool {
	if PlanKindRank[a.Kind] != PlanKindRank[b.Kind] {
		return PlanKindRank[a.Kind] < PlanKindRank[b.Kind]
	}
	return len(a.Via) < len(b.Via)
}
