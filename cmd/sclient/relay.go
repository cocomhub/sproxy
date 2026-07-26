// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层
	"github.com/spf13/cobra"
)

const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
)

// NewCmdRelay 创建 relay 父命令的工厂函数。
func runRelayStart(cmd *cobra.Command, hubURL, local, nodeID string) error {
	if nodeID == "" {
		nodeID = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}

	logger := slog.With("node", nodeID, "hub", hubURL, "local", local)
	logger.Info("中继节点启动")

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	return runRelayWithRetry(ctx, nodeID, hubURL, local, logger)
}

func runRelayWithRetry(ctx context.Context, nodeID, hubURL, local string, logger *slog.Logger) error {
	delay := reconnectBaseDelay
	for {
		err := runRelayOnce(ctx, nodeID, hubURL, local, logger)
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

func runRelayOnce(ctx context.Context, nodeID, hubURL, local string, logger *slog.Logger) error {
	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	conn, err := tp.Dial(ctx, hubURL)
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
	localAddr := local
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	tun := tunnel.NewTunnel(m, nil)
	logger.Info("等待中继请求...")

	err = tun.Serve(ctx, buildRelayHandler(ctx, localAddr, httpClient, logger))
	if err != nil {
		logger.Warn("中继服务停止", "error", err)
	}
	return err
}

// buildRelayHandler 创建用于转发中继请求的 HTTP handler。
// 将远程隧道请求转发到本地 HTTP 服务并返回响应。
func buildRelayHandler(ctx context.Context, localAddr string, httpClient *http.Client, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// ---- 工厂函数 ----

// NewCmdRelay 创建 relay 父命令的工厂函数。
func NewCmdRelay(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "中继节点管理",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(NewCmdRelayStart(ios))
	cmd.AddCommand(NewCmdRelayStatus(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStop(ios))
	cmd.AddCommand(NewCmdRelayRemoveNode(ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStats(ios, cfgSvc))
	return cmd
}

// NewCmdRelayStart 创建 relay start 命令的工厂函数。
func NewCmdRelayStart(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动中继节点，连接到 Hub",
		Long: `作为中继节点连接到 Hub，注册自身，然后等待远程请求并通过隧道转发到本地 HTTP 服务。

使用示例:
  sclient relay start --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, _ := cmd.Flags().GetString("hub")
			local, _ := cmd.Flags().GetString("local")
			nodeID, _ := cmd.Flags().GetString("node-id")
			return runRelayStart(cmd, hubURL, local, nodeID)
		},
	}
	cmd.Flags().String("hub", "ws://127.0.0.1:18084/ws", "Hub 的 WebSocket 地址")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址")
	cmd.Flags().String("node-id", "", "节点唯一标识 (默认使用时间戳)")
	return cmd
}

// NewCmdRelayStatus 创建 relay status 命令的工厂函数。
func NewCmdRelayStatus(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "查看 Hub 节点状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 获取服务器地址（从根命令的 persistent flag 或 --hub flag 或配置文件）
			serverURL, _ := cmd.Flags().GetString("server")
			if serverURL == "" {
				if hubURL, _ := cmd.Flags().GetString("hub"); hubURL != "" {
					if u, parseErr := url.Parse(hubURL); parseErr == nil {
						u.Scheme = "http"
						u.Path = ""
						serverURL = u.String()
					}
				}
			}
			if serverURL == "" && cfgSvc != nil {
				if cfg, err := cfgSvc.LoadConfig(); err == nil {
					serverURL = cfg.ServerURL
				}
			}
			if serverURL == "" {
				return fmt.Errorf("未指定服务器地址，请使用 --server 或 --hub 或配置 server_url")
			}

			// 获取 auth token
			authToken, _ := cmd.Flags().GetString("auth-token")
			if authToken == "" && cfgSvc != nil {
				if cfg, err := cfgSvc.LoadConfig(); err == nil {
					authToken = cfg.AuthToken
				}
			}

			// 查询节点列表
			nodesURL := strings.TrimRight(serverURL, "/") + "/api/hub/nodes"
			req, err := http.NewRequest("GET", nodesURL, nil)
			if err != nil {
				return fmt.Errorf("创建请求失败: %w", err)
			}
			if authToken != "" {
				req.Header.Set("Authorization", "Bearer "+authToken)
			}
			httpClient := &http.Client{Timeout: 10 * time.Second}
			resp, err := httpClient.Do(req)
			if err != nil {
				return fmt.Errorf("查询 Hub 状态失败: %w", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
				return fmt.Errorf("查询 Hub 状态失败 (HTTP %d): %s", resp.StatusCode, string(body))
			}

			var nodes []struct {
				ID        string `json:"id"`
				Addr      string `json:"addr,omitempty"`
				Connected string `json:"connected,omitempty"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
				return fmt.Errorf("解析响应失败: %w", err)
			}

			if len(nodes) == 0 {
				ios.WriteOutLine("暂无已连接节点")
				return nil
			}

			ios.WriteOutLine("已连接节点 (%d):", len(nodes))
			for _, n := range nodes {
				connected := n.Connected
				if connected != "" {
					if t, parseErr := time.Parse(time.RFC3339, connected); parseErr == nil {
						connected = t.Format("2006-01-02 15:04:05")
					}
				}
				ios.WriteOutLine("  - ID:       %s", n.ID)
				ios.WriteOutLine("    地址:     %s", n.Addr)
				ios.WriteOutLine("    连接时间: %s", connected)
			}
			return nil
		},
	}
	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")
	return cmd
}

// NewCmdRelayStop 创建 relay stop 命令的工厂函数。
func NewCmdRelayStop(ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "停止中继节点",
		Long: `向正在运行的中继节点发送停止信号。

中继节点作为独立进程运行时，请使用 kill 或 SIGINT 停止。
如果通过 sclient relay start 前台运行，按 Ctrl+C 即可停止。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ios.WriteOutLine("请向中继进程发送 SIGINT 信号以优雅停止。")
			ios.WriteOutLine("如果中继在前台运行，请按 Ctrl+C。")
			return nil
		},
	}
}
