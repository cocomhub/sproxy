// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// RelayStreamRequest 是任意 TCP 流中继请求（对应 hub.DialRequest）。
type RelayStreamRequest struct {
	Target string `json:"target"`
	Type   string `json:"type"` // 固定 "tcp"
	Addr   string `json:"addr"` // 目标叶子要出站连接的 TCP 地址（如 sg-vps-2:22）
}

// RelayStreamHandler 通过 hub 路由表把一条 HTTP 请求升级为到目标叶子的双向字节流，
// 实现任意 TCP（SSH/长连接）中继。
type RelayStreamHandler struct {
	routeTable *hub.RouteTable
	logger     *slog.Logger
}

// NewRelayStreamHandler 创建流中继处理器。
func NewRelayStreamHandler(rt *hub.RouteTable, logger *slog.Logger) *RelayStreamHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &RelayStreamHandler{routeTable: rt, logger: logger}
}

// ServeHTTP 处理中继流请求：
//  1. 解析 RelayStreamRequest
//  2. 查找目标叶子 mux
//  3. 在目标 mux 上 Open 一条流，写入 DialRequest 首帧
//  4. 用 http.Hijacker 拿底层 conn，做双向 io.Copy
//
// 叶子侧（sclient relay start / portal）在收到 DialRequest 后向 addr 发起出站
// net.Dial，随后把远程流与本地 socket 双向泵送。
func (h *RelayStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req RelayStreamRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("解析请求失败: %v", err), http.StatusBadRequest)
		return
	}
	if req.Target == "" || req.Addr == "" {
		http.Error(w, "缺少 target 或 addr", http.StatusBadRequest)
		return
	}
	if req.Type != "tcp" {
		http.Error(w, fmt.Sprintf("不支持的 type: %q", req.Type), http.StatusBadRequest)
		return
	}

	targetMux := h.routeTable.Lookup(hub.NodeID(req.Target))
	if targetMux == nil {
		h.logger.Warn("流中继目标节点未找到", "target", req.Target)
		http.Error(w, fmt.Sprintf("目标节点 %s 未找到", req.Target), http.StatusNotFound)
		return
	}

	// 在目标 mux 上打开一条流，写入「隧道元数据帧」格式的 dial 指令：
	//   [4B big-endian length][JSON {"dial":"addr"}]
	// 叶子侧（自定义 accept 循环）读到该帧后向 addr 发起出站 TCP。
	stream, err := targetMux.Open(r.Context())
	if err != nil {
		h.logger.Error("打开流中继流失败", "target", req.Target, "error", err)
		http.Error(w, fmt.Sprintf("打开流失败: %v", err), http.StatusBadGateway)
		return
	}
	defer stream.Close()

	head, merr := json.Marshal(hub.DialRequest{Dial: req.Addr})
	if merr != nil {
		http.Error(w, fmt.Sprintf("序列化 dial 指令失败: %v", merr), http.StatusInternalServerError)
		return
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, werr := stream.Write(lenBuf); werr != nil {
		h.logger.Error("写 dial 长度失败", "target", req.Target, "error", werr)
		http.Error(w, fmt.Sprintf("写 dial 指令失败: %v", werr), http.StatusBadGateway)
		return
	}
	if _, werr := stream.Write(head); werr != nil {
		h.logger.Error("写 dial 指令失败", "target", req.Target, "error", werr)
		http.Error(w, fmt.Sprintf("写 dial 指令失败: %v", werr), http.StatusBadGateway)
		return
	}

	// 升级为原始 TCP 流，做双向泵送
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "服务器不支持连接升级", http.StatusInternalServerError)
		return
	}
	conn, rw, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, fmt.Sprintf("升级连接失败: %v", err), http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	// Hijack 后 http.ResponseWriter 不再可用；写首行表示连接建立
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
	if err := rw.Flush(); err != nil {
		return
	}

	// 双向泵送：浏览器/SSH 端 <-> mux 流 <-> 叶子出站 TCP
	// 读侧必须用 rw.Reader（包含 conn + 已缓冲字节），否则 Hijack 时
	// 客户端在请求体后追加的数据若已被 bufio 预读，从 conn 直接读会跳过
	// 缓冲字节导致流错位（I4'）。写侧用 conn（rw.Writer 内部就是 conn）。
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(stream, rw)
		_ = stream.CloseWrite()
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(conn, stream)
		done <- struct{}{}
	}()

	// 等待一个方向完成；随后关闭另一方向（半关闭传播由叶子负责）。
	// 关键：同时 stream.Close() 解除 io.Copy(conn, stream) 对 stream 的阻塞，
	// 否则远端 TCP 不随 EOF 关闭时第二半段永久阻塞（goroutine 泄漏）。
	<-done
	_ = conn.Close()
	_ = stream.Close()
	<-done
}
