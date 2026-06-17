// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"unicode/utf8"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// RelayRequest 是中继请求的 JSON 格式。
type RelayRequest struct {
	Target     string            `json:"target"`
	Method     string            `json:"method"`
	Path       string            `json:"path"`
	Headers    map[string]string `json:"headers"`
	BodyBase64 string            `json:"body_base64"`
}

// RelayResponse 是中继响应的 JSON 格式。
type RelayResponse struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers"`
	BodyBase64 string              `json:"body_base64"`
	Error      string              `json:"error,omitempty"`
}

// RelayHandler 通过 hub 路由表转发请求到目标节点。
// 使用 Tunnel 帧协议与目标节点的 Tunnel.Serve 通信。
type RelayHandler struct {
	routeTable *hub.RouteTable
	logger     *slog.Logger
}

// NewRelayHandler 创建中继处理器。
func NewRelayHandler(rt *hub.RouteTable, logger *slog.Logger) *RelayHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &RelayHandler{routeTable: rt, logger: logger}
}

// ServeHTTP 处理中继请求：解析 JSON，查找目标节点，转发 HTTP 请求。
func (h *RelayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req RelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRelayError(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}
	if req.Target == "" {
		writeRelayError(w, "缺少 target 字段", http.StatusBadRequest)
		return
	}

	targetMux := h.routeTable.Lookup(hub.NodeID(req.Target))
	if targetMux == nil {
		h.logger.Warn("中继目标节点未找到", "target", req.Target)
		writeRelayError(w, fmt.Sprintf("目标节点 %s 未找到", req.Target), http.StatusNotFound)
		return
	}

	// 使用目标节点的 mux 创建临时 Tunnel 发送请求
	// 注意：Tunnel.NewTunnel 接受 *mux.Mux 但不拥有其生命周期
	// 这里使用无加密通道（中继传输层自身已加密）
	tun := tunnel.NewTunnel(targetMux, nil)

	// 构建转发请求
	forwardReq, err := http.NewRequest(req.Method, req.Path, nil)
	if err != nil {
		writeRelayError(w, fmt.Sprintf("构建转发请求失败: %v", err), http.StatusInternalServerError)
		return
	}
	for k, v := range req.Headers {
		forwardReq.Header.Set(k, v)
	}

	resp, err := tun.Do(forwardReq)
	if err != nil {
		h.logger.Error("中继转发失败", "target", req.Target, "error", err)
		writeRelayError(w, fmt.Sprintf("转发失败: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	result := RelayResponse{
		Status:     resp.StatusCode,
		Headers:    resp.Header,
		BodyBase64: bodyToString(body),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(result)
}

// bodyToString 将 body 转为 string。
// 有效 UTF-8 直接返回；二进制数据用 base64 编码。
func bodyToString(body []byte) string {
	if utf8.Valid(body) {
		return string(body)
	}
	return base64.StdEncoding.EncodeToString(body)
}

func writeRelayError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(RelayResponse{Status: code, Error: msg})
}
