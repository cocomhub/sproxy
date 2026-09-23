// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// federation_services.go 是跨 hub 服务发现（roadmap P2 mesh 集群化 F1）：
//   - GET /api/hub/federation/services：本 hub 节点宣告的服务（对端拉取）。
//   - /api/hub/services 聚合联邦候选服务（mergeFederationServices）。

import (
	"encoding/json"
	"net/http"
)

// federationServicesHandler 返回本 hub 节点宣告的服务（联邦对端拉取）。
func (h *Handlers) federationServicesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	mesh := meshFromRequest(r)
	type fedSvcResp struct {
		Node string `json:"node"`
		Mesh string `json:"mesh,omitempty"`
		Name string `json:"name"`
		Addr string `json:"addr"`
	}
	resp := make([]fedSvcResp, 0)
	for _, ns := range h.routeTable.ListServices(mesh) {
		resp = append(resp, fedSvcResp{
			Node: string(ns.Node), Mesh: mesh, Name: ns.Service.Name, Addr: ns.Service.Addr,
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.Warn("JSON encode error", "handler", "federationServicesHandler", "error", err)
	}
}
