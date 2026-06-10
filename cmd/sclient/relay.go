// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

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
	relayCmd.Flags().StringVar(&relayFl.nodeID, "node-id", "", "节点唯一标识 (默认使用时间戳)")
}

func runRelay(cmd *cobra.Command, args []string) error {
	nodeID := relayFl.nodeID
	if nodeID == "" {
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

	// 2. 创建 mux (Listener 角色)
	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 3. 通过控制流发送 Register 帧
	controlStream, err := m.Open(ctx)
	if err != nil {
		return fmt.Errorf("创建控制流失败: %w", err)
	}
	if _, err := controlStream.Write([]byte(hub.NodeID(nodeID))); err != nil {
		return fmt.Errorf("发送注册帧失败: %w", err)
	}
	controlStream.Close()
	logger.Info("已注册到 Hub")

	// 4. 等待中继请求
	// TODO: 使用 Tunnel.Serve(ctx, localHTTPHandler) 替代原始流处理
	// 当前为骨架版本：接受流后直接关闭
	logger.Info("等待中继请求（骨架模式）...")
	for {
		stream, err := m.Accept(ctx)
		if err != nil {
			return fmt.Errorf("接受流失败: %w", err)
		}
		stream.Close()
	}
}
