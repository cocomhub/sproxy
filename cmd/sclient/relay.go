// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/spf13/cobra"
)

const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
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

	logger := slog.With("node", nodeID, "hub", relayFl.hubURL, "local", relayFl.local)
	logger.Info("中继节点启动")

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	return runRelayWithRetry(ctx, nodeID, logger)
}

func runRelayWithRetry(ctx context.Context, nodeID string, logger *slog.Logger) error {
	delay := reconnectBaseDelay
	for {
		err := runRelayOnce(ctx, nodeID, logger)
		if err == nil || ctx.Err() != nil {
			return err
		}
		logger.Warn("中继断开，即将重连", "delay", delay, "error", err)
		select {
		case <-time.After(delay):
			delay *= 2
			if delay > reconnectMaxDelay {
				delay = reconnectMaxDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func runRelayOnce(ctx context.Context, nodeID string, logger *slog.Logger) error {

	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	conn, err := tp.Dial(ctx, relayFl.hubURL)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	logger.Info("已连接到 Hub")

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 注册节点：在控制流上发送 NodeID
	ctrl, err := m.Open(ctx)
	if err != nil {
		return fmt.Errorf("创建控制流失败: %w", err)
	}
	if _, err := ctrl.Write([]byte(nodeID)); err != nil {
		return fmt.Errorf("发送注册帧失败: %w", err)
	}
	ctrl.Close()
	logger.Info("已注册到 Hub")

	// 使用 Tunnel.Serve 接受中继请求，转发到本地 HTTP 服务
	localAddr := relayFl.local
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	tun := tunnel.NewTunnel(m, nil)
	logger.Info("等待中继请求...")

	err = tun.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 转发请求到本地服务
		forwardURL := localAddr + r.URL.Path
		if r.URL.RawQuery != "" {
			forwardURL += "?" + r.URL.RawQuery
		}

		forwardReq, err := http.NewRequestWithContext(ctx, r.Method, forwardURL, r.Body)
		if err != nil {
			logger.Warn("构建转发请求失败", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		forwardReq.Header = r.Header.Clone()

		resp, err := httpClient.Do(forwardReq)
		if err != nil {
			logger.Warn("转发到本地失败", "path", r.URL.Path, "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))

	if err != nil {
		logger.Warn("中继服务停止", "error", err)
	}
	return err
}
