// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdRelayDial 创建「经 hub 中继到目标叶子出口」的拨号命令。
//
// 用法：
//
//	sclient relay dial --node company --tcp target-host:22 [-l :2222]
//
// 默认单次模式：连接建立后把 stdin/stdout 与远端双向泵送（适合脚本/SSH -J 风格）。
// 指定 -l :port 时改为本地端口转发：监听本地端口，每个连接都经 hub 中继到目标。
func NewCmdRelayDial(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dial",
		Short: "经 hub 中继拨号到目标叶子出口（任意 TCP）",
		Long: `通过 hub 的流中继建立到目标叶子的任意 TCP 通道。

需要目标叶子以 --dial-allow（出口模式）运行。拨号路径：
  本地端口 ⇄ hub(/api/relay/stream) ⇄ 目标叶子出站 net.Dial ⇄ 目标服务

示例:
  sclient relay dial --node company --tcp target-host:22 -l :2222
  # 然后: ssh -p 2222 user@127.0.0.1

  sclient relay dial --node company --tcp target-host:80
  # 单次模式: 标准输入/输出直接接远端`,
		RunE: func(cmd *cobra.Command, args []string) error {
			node, _ := cmd.Flags().GetString("node")
			tcpAddr, _ := cmd.Flags().GetString("tcp")
			listenAddr, _ := cmd.Flags().GetString("listen")
			if node == "" || tcpAddr == "" {
				return fmt.Errorf("--node 与 --tcp 均不能为空")
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			if listenAddr != "" {
				return relayDialListen(cmd, svc, node, tcpAddr, listenAddr, ios)
			}
			return relayDialOnce(cmd, svc, node, tcpAddr, ios)
		},
	}
	cmd.Flags().String("node", "", "目标叶子节点 ID（需以 --dial-allow 运行）")
	cmd.Flags().String("tcp", "", "目标叶子出站连接的 TCP 地址，如 target-host:22")
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 :2222）；留空为单次 stdin/stdout 模式")
	_ = cmd.MarkFlagRequired("node")
	_ = cmd.MarkFlagRequired("tcp")
	return cmd
}

// relayDialOnce 单次模式：本地 stdin/stdout ⇄ 远端。
func relayDialOnce(cmd *cobra.Command, svc relayDialClient, node, tcpAddr string, ios cli.IOStreams) error {
	ctx := cmd.Context()
	conn, err := svc.RelayStream(ctx, node, tcpAddr)
	if err != nil {
		return err
	}
	defer conn.Close()
	ios.WriteOutLine("已连接: %s ⇄ %s (Ctrl+D / EOF 断开)", node, tcpAddr)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(conn, ios.In) }()
	go func() { defer wg.Done(); _, _ = io.Copy(ios.Out, conn) }()
	wg.Wait()
	return nil
}

// relayDialListen 本地端口转发模式。
func relayDialListen(cmd *cobra.Command, svc relayDialClient, node, tcpAddr, listenAddr string, ios cli.IOStreams) error {
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发已启动: %s ⇄ %s (经 hub 中继到 %s)", listenAddr, node, tcpAddr)
	ios.WriteOutLine("按 Ctrl+C 停止。")

	ctx := cmd.Context()
	for {
		clientConn, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return aerr
		}
		go func(c net.Conn) {
			defer c.Close()
			remote, rerr := svc.RelayStream(ctx, node, tcpAddr)
			if rerr != nil {
				ios.WriteErrLine("中继拨号失败: %v", rerr)
				return
			}
			defer remote.Close()
			var wg sync.WaitGroup
			wg.Add(2)
			go func() { defer wg.Done(); _, _ = io.Copy(remote, c) }()
			go func() { defer wg.Done(); _, _ = io.Copy(c, remote) }()
			wg.Wait()
		}(clientConn)
	}
}

// relayDialClient 抽象，便于测试注入 mock。
type relayDialClient interface {
	RelayStream(ctx context.Context, target, addr string) (net.Conn, error)
}

var _ relayDialClient = (*client.FileClient)(nil)
