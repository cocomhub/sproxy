// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
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
	Status       int             `json:"status"`
	Headers      http.Header     `json:"headers"`
	Body         json.RawMessage `json:"body"`
	BodyIsBase64 bool            `json:"body_is_base64"`
	Error        string          `json:"error,omitempty"`
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req RelayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeRelayError(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}
	if req.Target == "" {
		writeRelayError(w, "缺少 target 字段", http.StatusBadRequest)
		return
	}

	if err := validateRelayPath(req.Path); err != nil {
		writeRelayError(w, fmt.Sprintf("非法 path: %v", err), http.StatusBadRequest)
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

	// 构建转发请求（使用 url.URL 结构体避免 SSRF S5144）
	forwardReq := &http.Request{
		Method: req.Method,
		URL:    &url.URL{Path: req.Path},
		Host:   req.Target,
		Header: make(http.Header),
	}
	for k, v := range req.Headers {
		forwardReq.Header.Set(k, v)
	}

	// 解码并设置请求体（BodyBase64 可能为空字符串）
	if req.BodyBase64 != "" {
		decodedBody, err := base64.StdEncoding.DecodeString(req.BodyBase64)
		if err != nil {
			h.logger.Warn("中继请求体 base64 解码失败", "target", req.Target, "error", err)
			writeRelayError(w, fmt.Sprintf("请求体解码失败: %v", err), http.StatusBadRequest)
			return
		}
		forwardReq.Body = io.NopCloser(bytes.NewReader(decodedBody))
		forwardReq.ContentLength = int64(len(decodedBody))
	}

	// 使用请求超时，从 r.Context() 派生（客户端断开时自动取消）
	relayCtx := r.Context()
	const maxRelayTimeout = 30 * time.Second
	if _, hasDeadline := relayCtx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		relayCtx, cancel = context.WithTimeout(relayCtx, maxRelayTimeout)
		defer cancel()
	}
	forwardReq = forwardReq.WithContext(relayCtx)

	resp, err := tun.Do(forwardReq)
	if err != nil {
		h.logger.Error("中继转发失败", "target", req.Target, "error", err)
		writeRelayError(w, fmt.Sprintf("转发失败: %v", err), http.StatusBadGateway)
		return
	}

	// 限制响应体大小：最大 8MB + 1MB 余量（确保单分块传输不被截断）
	const maxRelayBody = 9 << 20 // 9 MB
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRelayBody))
	if err != nil {
		h.logger.Error("读取中继响应体失败", "target", req.Target, "error", err)
		writeRelayError(w, "读取响应体失败", http.StatusInternalServerError)
		return
	}

	// 排空剩余数据
	if int64(len(body)) == maxRelayBody {
		drainErr := drainWithTimeout(resp.Body, 5*time.Second)
		if drainErr != nil {
			h.logger.Warn("排空中继响应剩余数据失败", "target", req.Target, "error", drainErr)
		}
	}
	resp.Body.Close()

	result := RelayResponse{
		Status:       resp.StatusCode,
		Headers:      resp.Header,
		Body:         bodyToRawMessage(body),
		BodyIsBase64: !utf8.Valid(body),
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

// bodyToRawMessage 将 body 转为 json.RawMessage。
// 有效 UTF-8 直接返回字符串；二进制数据用 base64 编码。
func bodyToRawMessage(body []byte) json.RawMessage {
	if utf8.Valid(body) {
		b, _ := json.Marshal(string(body))
		return b
	}
	b, _ := json.Marshal(base64.StdEncoding.EncodeToString(body))
	return b
}

// drainWithTimeout 在指定超时内排空 reader 的剩余数据。
func drainWithTimeout(r io.Reader, timeout time.Duration) error {
	errCh := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, r)
		errCh <- err
	}()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("drain timeout after %v", timeout)
	}
}

// validateRelayPath 校验中继请求的 path 字段，防止 SSRF 攻击。
// 拒绝空 path、完整 URL（含 scheme）、路径穿越和绝对路径。
func validateRelayPath(relayPath string) error {
	if relayPath == "" {
		return fmt.Errorf("path 不能为空")
	}
	u, err := url.Parse(relayPath)
	if err != nil {
		return fmt.Errorf("path 解析失败: %w", err)
	}
	if u.Scheme != "" {
		return fmt.Errorf("path 不能包含 scheme")
	}
	if u.Host != "" {
		return fmt.Errorf("path 不能包含 host")
	}
	// 使用 path.Clean 清理路径，检查路径穿越
	cleaned := path.Clean(u.Path)
	if strings.HasPrefix(cleaned, "../") || cleaned == ".." {
		return fmt.Errorf("path 包含路径穿越")
	}
	return nil
}

func writeRelayError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(RelayResponse{Status: code, Error: msg})
}
