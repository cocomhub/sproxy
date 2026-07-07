// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"

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
		ID        string `json:"id"`
		Addr      string `json:"addr,omitempty"`
		Connected string `json:"connected,omitempty"`
	}
	resp := make([]nodeResp, 0, len(nodes))
	for _, n := range nodes {
		resp = append(resp, nodeResp{
			ID:        string(n.ID),
			Addr:      n.Addr,
			Connected: n.Connected.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(resp)
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
	h.routeTable.Remove(id)
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": string(id)})
}

// hubStatsHandler 返回中继统计。
func (h *Handlers) hubStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	count := h.routeTable.NodeCount()
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodes_connected": count,
	})
}
