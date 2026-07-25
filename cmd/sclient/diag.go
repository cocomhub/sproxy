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
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层
	"github.com/spf13/cobra"
)

var diagCmd = &cobra.Command{
	Use:   "diag",
	Short: "中继诊断工具",
	Long: `检测 Hub 连通性和查看中继节点状态。

使用示例:
  sclient diag --ping ws://hub.example.com/ws
  sclient diag --hub-status ws://hub.example.com/ws`,
	RunE: runDiag,
}

var (
	diagPing      string
	diagHubStatus string
)

func init() {
	diagCmd.Flags().StringVar(&diagPing, "ping", "", "检测 Hub 连通性和延迟，参数为 WebSocket 地址")
	diagCmd.Flags().StringVar(&diagHubStatus, "hub-status", "", "查看 Hub 在线节点列表，参数为 WebSocket 地址")
	rootCmd.AddCommand(diagCmd)
}

func runDiag(cmd *cobra.Command, args []string) error {
	switch {
	case diagPing != "":
		return runPing(cmd.Context(), diagPing)
	case diagHubStatus != "":
		return runHubStatus(cmd.Context(), diagHubStatus)
	default:
		return cmd.Help()
	}
}

func runPing(ctx context.Context, hubAddr string) error {
	logger := slog.With("hub", hubAddr)
	logger.Info("正在检测 Hub 连通性...")

	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	start := time.Now()
	conn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	dialDur := time.Since(start)
	defer conn.Close()

	logger.Info("Hub 连接成功", "延迟", dialDur.Round(time.Millisecond))

	sendStart := time.Now()
	if err := conn.Send(ctx, []byte("ping")); err != nil {
		return fmt.Errorf("发送 ping 失败: %w", err)
	}
	_, err = conn.Receive(ctx)
	if err != nil {
		return fmt.Errorf("接收 pong 失败: %w", err)
	}
	rtt := time.Since(sendStart)

	fmt.Printf("✅ Hub 可达\n")
	fmt.Printf("   连接延迟: %s\n", dialDur.Round(time.Millisecond))
	fmt.Printf("   消息往返: %s\n", rtt.Round(time.Millisecond))
	fmt.Printf("   地址: %s\n", hubAddr)
	return nil
}

func runHubStatus(ctx context.Context, hubAddr string) error {
	logger := slog.With("hub", hubAddr)
	logger.Info("正在获取 Hub 节点列表...")

	apiURL := hubAddr
	if len(apiURL) >= 2 && apiURL[:2] == "ws" {
		apiURL = "http" + apiURL[2:]
	}
	if len(apiURL) > 3 && apiURL[len(apiURL)-3:] == "/ws" {
		apiURL = apiURL[:len(apiURL)-3]
	}
	apiURL += "/api/hub/nodes"

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("构建请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("获取节点列表失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub 返回错误状态: %s", resp.Status)
	}

	var nodes []struct {
		ID        string `json:"id"`
		Addr      string `json:"addr,omitempty"`
		Connected string `json:"connected,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}

	fmt.Printf("在线节点数量: %d\n", len(nodes))
	for _, n := range nodes {
		fmt.Printf("  - ID: %s\n", n.ID)
		if n.Addr != "" {
			fmt.Printf("    地址: %s\n", n.Addr)
		}
		if n.Connected != "" {
			fmt.Printf("    连接时间: %s\n", n.Connected)
		}
	}
	return nil
}

// NewCmdDiag 创建 diag 命令的工厂函数。
// 使用 ioStreams 替代 fmt.Printf / os.Stderr 进行输出。
func NewCmdDiag(ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "diag",
		Short: "中继诊断工具",
		Long: `检测 Hub 连通性和查看中继节点状态。

使用示例:
  sclient diag --ping ws://hub.example.com/ws
  sclient diag --hub-status ws://hub.example.com/ws`,
		RunE: func(cmd *cobra.Command, args []string) error {
			pingURL, _ := cmd.Flags().GetString("ping")
			hubURL, _ := cmd.Flags().GetString("hub-status")

			switch {
			case pingURL != "":
				return runPingWithIO(cmd.Context(), pingURL, ios.Out)
			case hubURL != "":
				return runHubStatusWithIO(cmd.Context(), hubURL, ios.Out)
			default:
				return cmd.Help()
			}
		},
	}

	cmd.Flags().String("ping", "", "检测 Hub 连通性和延迟，参数为 WebSocket 地址")
	cmd.Flags().String("hub-status", "", "查看 Hub 在线节点列表，参数为 WebSocket 地址")
	return cmd
}

// runPingWithIO 是 runPing 的 ioStreams 版本，使用 w 替代 fmt.Printf。
func runPingWithIO(ctx context.Context, hubAddr string, w io.Writer) error {
	logger := slog.With("hub", hubAddr)
	logger.Info("正在检测 Hub 连通性...")

	tp := xfer.Get("ws")
	if tp == nil {
		return fmt.Errorf("ws 传输层未注册")
	}

	start := time.Now()
	conn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	dialDur := time.Since(start)
	defer conn.Close()

	logger.Info("Hub 连接成功", "延迟", dialDur.Round(time.Millisecond))

	sendStart := time.Now()
	if err := conn.Send(ctx, []byte("ping")); err != nil {
		return fmt.Errorf("发送 ping 失败: %w", err)
	}
	_, err = conn.Receive(ctx)
	if err != nil {
		return fmt.Errorf("接收 pong 失败: %w", err)
	}
	rtt := time.Since(sendStart)

	fmt.Fprintf(w, "✅ Hub 可达\n")
	fmt.Fprintf(w, "   连接延迟: %s\n", dialDur.Round(time.Millisecond))
	fmt.Fprintf(w, "   消息往返: %s\n", rtt.Round(time.Millisecond))
	fmt.Fprintf(w, "   地址: %s\n", hubAddr)
	return nil
}

// runHubStatusWithIO 是 runHubStatus 的 ioStreams 版本，使用 w 替代 fmt.Printf。
func runHubStatusWithIO(ctx context.Context, hubAddr string, w io.Writer) error {
	logger := slog.With("hub", hubAddr)
	logger.Info("正在获取 Hub 节点列表...")

	apiURL := hubAddr
	if len(apiURL) >= 2 && apiURL[:2] == "ws" {
		apiURL = "http" + apiURL[2:]
	}
	if len(apiURL) > 3 && apiURL[len(apiURL)-3:] == "/ws" {
		apiURL = apiURL[:len(apiURL)-3]
	}
	apiURL += "/api/hub/nodes"

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return fmt.Errorf("构建请求失败: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("获取节点列表失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("hub 返回错误状态: %s", resp.Status)
	}

	var nodes []struct {
		ID        string `json:"id"`
		Addr      string `json:"addr,omitempty"`
		Connected string `json:"connected,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		return fmt.Errorf("解析响应失败: %w", err)
	}

	fmt.Fprintf(w, "在线节点数量: %d\n", len(nodes))
	for _, n := range nodes {
		fmt.Fprintf(w, "  - ID: %s\n", n.ID)
		if n.Addr != "" {
			fmt.Fprintf(w, "    地址: %s\n", n.Addr)
		}
		if n.Connected != "" {
			fmt.Fprintf(w, "    连接时间: %s\n", n.Connected)
		}
	}
	return nil
}