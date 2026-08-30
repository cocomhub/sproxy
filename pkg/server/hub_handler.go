// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// SetDHT 注入节点发现表（DHT）。nil 清除（恢复不合并 DHT 候选）。
// 由 cmd/sproxy 装配 Kademlia 时调用（hub.dht: kad）。
func (h *Handlers) SetDHT(dht hub.DHT) {
	h.dht = dht
}

// hubNodesHandler 返回在线节点列表（按调用方 mesh 过滤，M-9）。
// 发现源 = 路由表（hub 权威）+ DHT 候选节点（SetDHT 注入时合并，去重，路由表优先）。
// 满足"路由表仍 hub 权威；DHT 只提供候选节点/发现，不改状态"。
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
	type nodeResp struct {
		ID   string `json:"id"`
		Addr string `json:"addr,omitempty"`
		// omitzero（Go 1.24+）：time.Time 的零值经 omitempty 仍会序列化为
		// "0001-01-01T00:00:00Z"，DHT 候选无连接时间需用 omitzero 才真正省略。
		Connected time.Time `json:"connected,omitzero"`
	}
	resp := make([]nodeResp, 0, len(nodes))
	for _, n := range nodes {
		resp = append(resp, nodeResp{
			ID:        string(n.ID),
			Addr:      n.Addr,
			Connected: n.Connected,
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
		Name string `json:"name"`
		Node string `json:"node"`
		Addr string `json:"addr,omitempty"`
	}
	var resp []svcResp
	for _, ns := range h.routeTable.ListServices(meshFromRequest(r)) {
		resp = append(resp, svcResp{Name: ns.Service.Name, Node: string(ns.Node), Addr: ns.Service.Addr})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubServicesHandler", "error", err)
	}
}
