// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
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
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 127.0.0.1:2222；裸 :2222 归一为 127.0.0.1:2222）；留空为单次 stdin/stdout 模式")
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

	// 方向区分通道（I41，meshStdioOnce 同款）：对端断开（outDone）→ 会话结束立即
	// 返回；本地 stdin 读完（inDone，如 EOF/管道结束）→ 等待对端把剩余响应写完
	// （保留 `echo x | relay dial` 的响应语义）。原 wg.Wait() 在对端断开但 stdin
	// 未 EOF 时永久挂起（CLI 假死）。
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(conn, ios.In)
		// P0-5：stdin EOF 后传播半关闭，否则对端永远等不到"输入写完"，
		// <outDone 永久挂起（与 meshStdioOnce / p2pStdio 同款修复）。
		iostream.CloseWrite(conn)
	}()
	go func() { defer close(outDone); _, _ = io.Copy(ios.Out, conn) }()
	select {
	case <-outDone: // 对端断开：会话结束
	case <-inDone: // 本地 stdin 读完：半关闭已传播，等对端把剩余数据写完
		<-outDone
	}
	return nil
}

// relayDialListen 本地端口转发模式。
func relayDialListen(cmd *cobra.Command, svc relayDialClient, node, tcpAddr, listenAddr string, ios cli.IOStreams) error {
	// 裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 +
	// Windows Defender 防火墙弹窗）；需 LAN 访问时显式通配地址:port 或具体 IP
	// （S56 同款 normalizeListenAddr）。
	listenAddr = iostream.NormalizeListenAddr(listenAddr)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发已启动: %s ⇄ %s (经 hub 中继到 %s)", listenAddr, node, tcpAddr)
	ios.WriteOutLine("按 Ctrl+C 停止。")

	return relayDialListenOn(cmd.Context(), svc, node, tcpAddr, ln, ios)
}

// relayDialListenOn 在已注入的 listener 上运行端口转发，每连接建立一条中继流。
// 拆出独立函数以便测试注入 127.0.0.1:0 动态端口 listener（S59）。
func relayDialListenOn(ctx context.Context, svc relayDialClient, node, tcpAddr string, ln net.Listener, ios cli.IOStreams) error {
	// ctx 取消时关闭 listener，使 Accept 立即返回（优雅停止端口转发，S58）。
	// 若无此行，Accept 在 ln 关闭前永不返回，accept 循环里 ctx.Err() 分支是死代码。
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
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
			// 双向泵送（CloseWrite 半关闭 + grace 宽限期，C1 范本，见 iostream.Pump）：
			// 任一方向完成即向对端传播半关闭，让在途响应仍可被读回；对端不回应 FIN
			// 时 grace 超时强制双侧关闭解除阻塞。返回后由外层 defer 收尾。
			iostream.Pump(c, remote, iostream.PumpGrace)
		}(clientConn)
	}
}

// relayDialClient 抽象，便于测试注入 mock。
type relayDialClient interface {
	RelayStream(ctx context.Context, target, addr string) (net.Conn, error)
}

var _ relayDialClient = (*client.FileClient)(nil)
