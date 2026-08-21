// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// hubNodesHandler 返回在线节点列表。
func (h *Handlers) hubNodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	nodes := h.routeTable.List()
	type nodeResp struct {
		ID        string    `json:"id"`
		Addr      string    `json:"addr,omitempty"`
		Connected time.Time `json:"connected"`
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
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": string(id)}); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubRemoveNodeHandler", "error", err)
	}
}

// hubStatsHandler 返回中继统计。
func (h *Handlers) hubStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	count := h.routeTable.NodeCount()
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(map[string]any{
		"nodes_connected": count,
	}); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubStatsHandler", "error", err)
	}
}

// hubServicesHandler 返回所有节点宣告的服务（mesh 选路用）。
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
	for _, n := range h.routeTable.List() {
		for _, s := range h.routeTable.ServicesOf(n.ID) {
			resp = append(resp, svcResp{Name: s.Name, Node: string(n.ID), Addr: s.Addr})
		}
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "hubServicesHandler", "error", err)
	}
}
