// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// SetDHT 注入节点发现表（DHT）。nil 清除（恢复不合并 DHT 候选）。
// 由 cmd/sproxy 装配 Kademlia 时调用（hub.dht: kad）。
func (h *Handlers) SetDHT(dht hub.DHT) {
	h.dht = dht
}

// hubNodesHandler 返回在线节点列表（按调用方 mesh 过滤，M-9）。
// 发现源 = 路由表（hub 权威）+ DHT 候选节点（SetDHT 注入时合并）+ 联邦候选节点
// （SetFederationClient 注入时合并），逐层去重（路由表优先），均只提供发现/可达性。
// 满足"路由表仍 hub 权威；DHT/联邦只提供候选节点/发现，不改状态"。
func (h *Handlers) hubNodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	mesh := meshFromRequest(r)
	nodes := h.routeTable.List(mesh)
	if h.dht != nil {
		nodes = h.mergeDHTNodes(nodes, mesh)
	}
	if h.fedClient != nil {
		nodes = h.mergeFederationNodes(nodes, mesh)
	}
	type nodeResp struct {
		ID   string `json:"id"`
		Addr string `json:"addr,omitempty"`
		// Quality 是节点链路质量分档（roadmap P2 节点级状态仪表）：
		// healthy|degraded|stale（复用 #501 判据）。
		Quality string `json:"quality,omitempty"`
		// VirtualIP 是节点虚拟 IP（hub 权威分配；DHT/联邦候选节点无虚拟 IP，省略）。
		VirtualIP string `json:"virtual_ip,omitempty"`
		// omitzero（Go 1.24+）：time.Time 的零值经 omitempty 仍会序列化为
		// "0001-01-01T00:00:00Z"，DHT 候选无连接时间需用 omitzero 才真正省略。
		Connected time.Time `json:"connected,omitzero"`
		// Capabilities 是节点声明的能力标志（如 "outbound-dial"：可作中转出口）。
		// via-node 多跳据此发现候选中间节点（fail-closed：无此标记不选）。
		Capabilities []string `json:"capabilities,omitempty"`
	}
	resp := make([]nodeResp, 0, len(nodes))
	for _, n := range nodes {
		var vipStr string
		if n.VirtualIP.IsValid() {
			vipStr = n.VirtualIP.String()
		}
		resp = append(resp, nodeResp{
			ID:           string(n.ID),
			Addr:         n.Addr,
			Quality:      h.nodeQuality(string(n.ID), mesh),
			VirtualIP:    vipStr,
			Connected:    n.Connected,
			Capabilities: n.Capabilities,
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubNodesHandler", "error", err)
	}
}

// mergeDHTNodes 把 DHT 候选节点合并进发现列表：按调用方 mesh 过滤（PeerInfo.Meta
// ["mesh"]），按 node-id 去重（路由表权威优先）。DHT 查询失败/为空时原样返回。
// 对端候选节点的 Connected 时间未知，用零值（客户端仅按 id 寻址，不依赖时间）。
func (h *Handlers) mergeDHTNodes(nodes []hub.NodeInfo, mesh string) []hub.NodeInfo {
	candidates, err := h.dht.GetClosestNodes(context.Background(), "hub-discovery", 1000)
	if err != nil {
		return nodes
	}
	seen := make(map[hub.NodeID]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
	}
	for _, c := range candidates {
		// DHT 候选按 mesh 严格隔离：cm=="" 即默认 mesh，只对默认 mesh 请求者放行。
		// 不能用"cm=="" 放行所有"——否则默认 mesh 节点泄漏给命名 mesh 调用方
		// （破坏 M-9 列表隔离，且信令按 node-id 存转可被利用跨 mesh 拨号）。
		if cm := c.Meta["mesh"]; cm != mesh {
			continue
		}
		id := hub.NodeID(c.ID)
		if id == "" || seen[id] {
			continue // 去重：路由表权威优先
		}
		seen[id] = true
		addr := c.Meta["addr"]
		if addr == "" && len(c.Addrs) > 0 {
			addr = c.Addrs[0]
		}
		nodes = append(nodes, hub.NodeInfo{ID: id, Addr: addr})
	}
	return nodes
}

// federationNodesHandler 返回本 hub 路由表节点 + 联邦候选（带 mesh），供联邦对端同步。
// 按调用方 mesh 过滤（M-9）：拉取方用哪个 mesh 的凭据，只能拿到该 mesh 的节点，
// 联邦同步不破坏 mesh 隔离。
//
// 多跳发现（2026-09-19 方案 B）：除路由表外**合并本 hub 的联邦候选**（fc.Candidates()），
// 使对端 A 能看到 2 级节点（A→hub-B→hub-C→B 链式发现）。防环理由：
//   - 同步是**单次拉取、不递归**（hub-B 只返回「路由表 + 自己的直接候选」，不再次
//     拉取 hub-C 的候选合并）⇒ A 最多看到 2 级节点，无无限回声；
//   - 候选的**转发**链路由 X-Relay-Hop/Path 防环（上限 4 + 回源拒绝）保护；
//   - A↔B 互配时 hub-B 可能返回 A 的节点（来自 A 的候选）——无实际危害（转发时
//     本 hub 路由表命中优先），且候选仅作发现/可达性，不进入路由表。
func (h *Handlers) federationNodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	mesh := meshFromRequest(r)
	type fedNodeResp struct {
		ID        string    `json:"id"`
		Addr      string    `json:"addr,omitempty"`
		Mesh      string    `json:"mesh,omitempty"`
		Connected time.Time `json:"connected,omitzero"`
	}
	nodes := h.routeTable.List(mesh)
	if h.fedClient != nil {
		nodes = h.mergeFederationNodes(nodes, mesh)
	}
	resp := make([]fedNodeResp, 0, len(nodes))
	for _, n := range nodes {
		resp = append(resp, fedNodeResp{
			ID:        string(n.ID),
			Addr:      n.Addr,
			Mesh:      n.Mesh,
			Connected: n.Connected,
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "federationNodesHandler", "error", err)
	}
}

// mergeFederationNodes 把联邦候选节点合并进发现列表：按调用方 mesh 严格过滤
// （FederationNode.Mesh，空 mesh 只对默认 mesh 请求者放行），按 node-id 去重
// （路由表/DHT 已占用优先，联邦候选后置）。联邦候选是远程 hub 的节点，不进入
// 本 hub 路由表（本 hub 无法转发到远程节点），仅提供发现/可达性。
// 注意：候选 Addr 来自对端上报（信息面，与 mergeDHTNodes 一致，客户端自行决定
// 连接）；若未来联邦候选用于自动拨号，需在此处增加地址合法性校验。
func (h *Handlers) mergeFederationNodes(nodes []hub.NodeInfo, mesh string) []hub.NodeInfo {
	candidates := h.fedClient.Candidates()
	if len(candidates) == 0 {
		return nodes
	}
	seen := make(map[hub.NodeID]bool, len(nodes))
	for _, n := range nodes {
		seen[n.ID] = true
	}
	for _, c := range candidates {
		// 联邦候选按 mesh 严格隔离：cm=="" 即默认 mesh，只对默认 mesh 请求者放行。
		// 不能用"cm=="" 放行所有"——否则默认 mesh 节点泄漏给命名 mesh 调用方
		// （破坏 M-9 列表隔离，且信令按 node-id 存转可被利用跨 mesh 拨号）。
		// 与 mergeDHTNodes 的隔离语义严格一致（阶段 2 DHT 曾踩过默认 mesh 泄漏）。
		if c.Mesh != mesh {
			continue
		}
		id := c.ID
		if id == "" || seen[id] {
			continue // 去重：路由表/DHT 优先
		}
		seen[id] = true
		nodes = append(nodes, hub.NodeInfo{ID: id, Addr: c.Addr})
	}
	return nodes
}

// hubRemoveNodeHandler 踢出指定节点。
func (h *Handlers) hubRemoveNodeHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	id := hub.NodeID(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing node id", http.StatusBadRequest)
		return
	}
	// 先检查节点是否存在
	if !h.routeTable.Has(id) {
		http.Error(w, fmt.Sprintf("节点 %s 不存在", id), http.StatusNotFound)
		return
	}
	h.routeTable.Remove(id)
	// 同步从发现表（DHT）移除，防管理端踢出后节点仍出现在 /api/hub/nodes。
	if h.dht != nil {
		if rerr := h.dht.Remove(context.Background(), string(id)); rerr != nil {
			h.logger.Debug("DHT 节点移除失败（忽略）", "node", id, "error", rerr)
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": string(id)}); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubRemoveNodeHandler", "error", err)
	}
}

// hubStatsHandler 返回中继统计（按调用方 mesh 统计节点数，M-9）。
func (h *Handlers) hubStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	count := h.routeTable.NodeCount(meshFromRequest(r))
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"nodes_connected": count,
	}); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubStatsHandler", "error", err)
	}
}

// hubServicesHandler 返回调用方 mesh 内节点宣告的服务（mesh 选路用）。
// 使用 RouteTable.ListServices 按 (node, name) 稳定排序，客户端可确定性选路（I3）。
func (h *Handlers) hubServicesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	type svcResp struct {
		Name    string `json:"name"`
		Node    string `json:"node"`
		Addr    string `json:"addr,omitempty"`
		Quality string `json:"quality,omitempty"` // healthy|degraded|stale（链路质量分档）
	}
	mesh := meshFromRequest(r)
	var resp []svcResp
	for _, ns := range h.routeTable.ListServices(mesh) {
		resp = append(resp, svcResp{
			Name: ns.Service.Name, Node: string(ns.Node), Addr: ns.Service.Addr,
			Quality: h.nodeQuality(string(ns.Node), mesh),
		})
	}
	// 跨 hub 服务发现（roadmap P2 mesh 集群化 F1）：聚合联邦候选服务。
	if h.fedClient != nil {
		seen := make(map[string]bool, len(resp))
		for _, r := range resp {
			seen[r.Node+string([]byte{0})+r.Name] = true
		}
		for _, fs := range h.fedClient.CandidateServices() {
			if fs.Mesh != mesh || seen[string(fs.Node)+string([]byte{0})+fs.Name] {
				continue
			}
			seen[string(fs.Node)+string([]byte{0})+fs.Name] = true
			resp = append(resp, svcResp{Name: fs.Name, Node: string(fs.Node), Addr: fs.Addr, Quality: "stale"})
		}
	}
	// 按质量排序（healthy < degraded < stale 升序 = 质量优者在前）；同档按 node 名稳定。
	sort.SliceStable(resp, func(i, j int) bool {
		if q := qualityRank(resp[i].Quality) - qualityRank(resp[j].Quality); q != 0 {
			return q < 0
		}
		return resp[i].Node < resp[j].Node
	})
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubServicesHandler", "error", err)
	}
}

// nodeQuality 返回节点链路质量分档（基于 mux Metrics + 连接时间）。
//   - stale：节点连接超过 staleThreshold（心跳 30s 的 3 倍 = 90s 未活跃）；
//   - degraded：链路质量劣化（重传率高 / 错误多）；
//   - healthy：默认。
func (h *Handlers) nodeQuality(nodeID, mesh string) string {
	if h.routeTable == nil {
		return "stale"
	}
	info, ok := h.routeTable.LookupInfo(hub.NodeID(nodeID))
	if !ok {
		return "stale"
	}
	if time.Since(info.Connected) > staleThreshold {
		return "stale"
	}
	if info.Mux == nil {
		return "healthy"
	}
	mm := info.Mux.Metrics()
	// 劣化判定：重传累计 > 0（链路质量降级信号）或错误累计 > 0。
	if mm.Retransmits.Load() > 0 || mm.Errors.Load() > 0 {
		return "degraded"
	}
	return "healthy"
}

// staleThreshold 是节点 stale 判定阈值（mux 心跳 30s 的 3 倍）。
const staleThreshold = 90 * time.Second

// qualityRank 返回质量分档的排序权重（越小越优）。
func qualityRank(q string) int {
	switch q {
	case "healthy":
		return 0
	case "degraded":
		return 1
	default: // stale / unknown
		return 2
	}
}
