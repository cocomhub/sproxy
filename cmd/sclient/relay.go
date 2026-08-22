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
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层
	"github.com/spf13/cobra"
)

const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
	// registerAckTimeout 是等待 hub 注册 ACK 的超时。
	registerAckTimeout = 10 * time.Second
)

// NewCmdRelay 创建 relay 父命令的工厂函数。
func runRelayStart(cmd *cobra.Command, hubURL, local, nodeID, token string, dialAllow bool, services, dialAllowCIDRs []string) error {
	if nodeID == "" {
		nodeID = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}

	logger := slog.With("node", nodeID, "hub", hubURL, "local", local, "dial_allow", dialAllow)
	logger.Info("中继节点启动")

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	return runRelayWithRetry(ctx, nodeID, hubURL, local, token, dialAllow, services, dialAllowCIDRs, logger)
}

func runRelayWithRetry(ctx context.Context, nodeID, hubURL, local, token string, dialAllow bool, services, dialAllowCIDRs []string, logger *slog.Logger) error {
	delay := reconnectBaseDelay
	for {
		err := runRelayOnce(ctx, nodeID, hubURL, local, token, dialAllow, services, dialAllowCIDRs, logger)
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

func runRelayOnce(ctx context.Context, nodeID, hubURL, local, token string, dialAllow bool, services, dialAllowCIDRs []string, logger *slog.Logger) error {
	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	conn, err := tp.Dial(ctx, hubURL)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	logger.Info("已连接到 Hub")

	// 注册协议：连接建立后，在 xfer 层直接发送一条注册帧（JSON 或裸 nodeID）。
	// 与 HubServer.readRegisterFrame 对齐：hub 在创建 mux 前通过 conn.Receive 读取，
	// 因此这里也必须用 conn.Send，而非 mux 控制流。
	meta := hub.Meta{}
	if dialAllow {
		meta.Tags = append(meta.Tags, "exit")
	}
	for _, svc := range services {
		name, addr, ok := strings.Cut(svc, ":")
		if !ok || name == "" || addr == "" {
			logger.Warn("忽略无效服务宣告（应为 name:addr）", "raw", svc)
			continue
		}
		meta.Services = append(meta.Services, hub.Service{Name: name, Addr: addr})
	}
	if serr := conn.Send(ctx, hub.NewRegisterFrame(nodeID, token, meta)); serr != nil {
		return fmt.Errorf("发送注册帧失败: %w", serr)
	}

	// 等待 hub 注册 ACK（token 错误/格式错误尽早报错，而非等建流失败才发现）
	ackCtx, ackCancel := context.WithTimeout(ctx, registerAckTimeout)
	ack, ackErr := conn.Receive(ackCtx)
	ackCancel()
	if ackErr != nil {
		return fmt.Errorf("等待注册 ACK 失败: %w", ackErr)
	}
	if strings.HasPrefix(string(ack), hub.RegisterAckErr) {
		return fmt.Errorf("注册被拒绝: %s", strings.TrimPrefix(string(ack), hub.RegisterAckErr))
	}
	if string(ack) != hub.RegisterAckOK {
		logger.Warn("收到未知注册响应", "ack", string(ack))
	}
	logger.Info("已注册到 Hub")

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 本地 HTTP 服务地址（HTTP 中继转发目标）
	localAddr := local
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}

	logger.Info("等待中继请求...")
	var opts []relay.ServeOptions
	if len(dialAllowCIDRs) > 0 {
		opts = append(opts, relay.ServeOptions{DialPolicy: relay.NewDialPolicy(dialAllowCIDRs)})
	}
	err = relay.Serve(ctx, m, localAddr, dialAllow, httpClient, logger, opts...)
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

		forwardReq, err := http.NewRequestWithContext(ctx, r.Method, forwardURL, r.Body) //nolint:gosec // G704: SSRF is intentional (relay proxy)
		if err != nil {
			logger.Warn("构建转发请求失败", "error", err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		forwardReq.Header = r.Header.Clone()

		resp, err := httpClient.Do(forwardReq) //nolint:gosec // G704: SSRF is intentional (relay proxy)
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
		_, _ = io.Copy(w, resp.Body)
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
	cmd.AddCommand(NewCmdRelayDial(factory, ios))
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
			token, _ := cmd.Flags().GetString("token")
			dialAllow, _ := cmd.Flags().GetBool("dial-allow")
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			return runRelayStart(cmd, hubURL, local, nodeID, token, dialAllow, services, dialAllowCIDRs)
		},
	}
	cmd.Flags().String("hub", "ws://127.0.0.1:18084/ws", "Hub 的 WebSocket 地址")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址")
	cmd.Flags().String("node-id", "", "节点唯一标识 (默认使用时间戳)")
	cmd.Flags().String("token", "", "中继注册 token（与 hub.relay_token 一致；未配置 hub token 时可不填）")
	cmd.Flags().Bool("dial-allow", false, "作为出口节点：允许收到 dial 帧时向目标地址发起出站 TCP 连接（供中继端充当出口网关）")
	cmd.Flags().StringArray("service", nil, "宣告一个 mesh 服务（格式 name:addr，可重复；供 sclient mesh connect 发现）")
	cmd.Flags().StringArray("dial-allow-cidr", nil, "出口拨号白名单网段（如 192.168.0.0/16；配合 --dial-allow 放行内网服务，默认仅公网）")
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
