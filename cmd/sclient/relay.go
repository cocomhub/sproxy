// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/xferws" // 注册 ws 传输层
	"github.com/spf13/cobra"
)

var relayCmd = &cobra.Command{
	Use:   "relay",
	Short: "启动中继节点，将远程请求转发到本地 HTTP 服务",
	Long: `作为中继节点连接到 Hub，注册自身，然后等待远程请求并通过隧道转发到本地 HTTP 服务。

使用示例:
  sclient relay --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node`,
	RunE: runRelay,
}

type relayFlags struct {
	hubURL string
	local  string
	nodeID string
}

var relayFl relayFlags

func init() {
	relayCmd.Flags().StringVar(&relayFl.hubURL, "hub", "ws://127.0.0.1:18084/ws", "Hub 的 WebSocket 地址")
	relayCmd.Flags().StringVar(&relayFl.local, "local", "http://127.0.0.1:8080", "本地 HTTP 服务地址")
	relayCmd.Flags().StringVar(&relayFl.nodeID, "node-id", "", "节点唯一标识 (默认使用主机名)")
}

func runRelay(cmd *cobra.Command, args []string) error {
	nodeID := relayFl.nodeID
	if nodeID == "" {
		hostname, _ := cmd.Flags().GetString("node-id")
		_ = hostname
		nodeID = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}

	logger := slog.With("node", nodeID, "hub", relayFl.hubURL)
	logger.Info("中继节点启动")

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// 1. 通过 ws 传输层连接到 Hub
	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	conn, err := tp.Dial(ctx, relayFl.hubURL)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	logger.Info("已连接到 Hub")

	// 2. 创建 mux (Listener 角色，由 Hub 端分配偶数 StreamID)
	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 3. 通过控制流 (StreamID=0) 发送 Register 帧
	//    Register 帧内容为节点 ID
	controlStream, err := m.Open(ctx)
	if err != nil {
		return fmt.Errorf("创建控制流失败: %w", err)
	}
	registerMsg := hub.NodeID(nodeID)
	if _, err := controlStream.Write([]byte(registerMsg)); err != nil {
		return fmt.Errorf("发送注册帧失败: %w", err)
	}
	controlStream.Close()
	logger.Info("已注册到 Hub", "node_id", nodeID)

	// 4. 接受转发请求循环
	logger.Info("等待中继请求...")
	for {
		stream, err := m.Accept(ctx)
		if err != nil {
			return fmt.Errorf("接受流失败: %w", err)
		}
		go handleRelayStream(ctx, logger, stream)
	}
}

// handleRelayStream 处理一条中继流：读取 HTTP 请求，转发到本地服务，写回响应。
func handleRelayStream(ctx context.Context, logger *slog.Logger, stream *mux.Stream) {
	defer stream.Close()
	defer stream.CloseWrite()

	logger.Debug("处理中继请求")

	// 读取 HTTP 请求
	req, err := http.ReadRequest(bufio.NewReader(stream))
	if err != nil {
		logger.Warn("读取 HTTP 请求失败", "error", err)
		return
	}

	// 改写 URL 指向本地服务
	localURL := relayFl.local
	if req.URL != nil {
		localURL += req.URL.String()
	}
	req.RequestURI = ""
	req.URL, err = req.URL.Parse(localURL)
	if err != nil {
		logger.Warn("解析本地 URL 失败", "error", err)
		return
	}

	// 发送 HTTP 请求到本地服务
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logger.Warn("转发到本地失败", "error", err)
		writeErrorResponse(stream, http.StatusBadGateway, "bad gateway")
		return
	}
	defer resp.Body.Close()

	// 将响应写回到 stream
	resp.Write(stream)
	_, _ = io.Copy(stream, resp.Body)
}

// writeErrorResponse 向流中写入一个简单的 HTTP 错误响应。
func writeErrorResponse(w io.Writer, code int, body string) {
	statusText := http.StatusText(code)
	if statusText == "" {
		statusText = "Unknown"
	}
	fmt.Fprintf(w, "HTTP/1.1 %d %s\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
		code, statusText, len(body), body)
}

// Tunnel 类型的 Do 方法需要 *tunnel.Tunnel，但我们直接在 relay 中处理流，
// 所以不需要创建 Tunnel 实例。直接使用 mux.Stream 传递 HTTP 请求-响应。
var _ = tunnel.NewTunnel // 确保 tunnel 包被 import（用于编译）
