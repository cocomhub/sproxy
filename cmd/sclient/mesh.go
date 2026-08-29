// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层（自动注册拨号用）
	"github.com/spf13/cobra"
)

// meshDialFunc 建立一条到目标服务的连接（选路逻辑）。
// 默认用 pkg/tunnel/mesh.Dial（webrtc 打洞优先，失败回落 hub 中继）；
// 指定 --gateway 时先经本地 mesh node 网关复用已建直连链路，无已建链路回落常规拨号。
// 可注入测试桩。
type meshDialFunc func(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*mesh.Result, error)

// meshGatewayDial 构造带本地网关优先的选路 dial：先经本地 mesh node 网关复用已建
// 直连链路（零重新打洞），本地节点无到目标的已建链路（ErrNoPeerLink）时回落常规
// 拨号 mesh.Dial；其他网关错误（连接失败/协议错误/token 校验失败）也回落并提示
// （不回归既有路径）。gatewayToken 是网关认证 token（与 mesh node 相同的 auth_token）。
func meshGatewayDial(gatewayAddr, gatewayToken string, ios cli.IOStreams) meshDialFunc {
	return func(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
		if conn, gerr := mesh.GatewayConnect(ctx, gatewayAddr, target.Node, target.Addr, gatewayToken); gerr == nil {
			// 复用已建立直连链路：网关在已建链路上写拨号帧，对端 relay.Serve 出口拨号。
			return &mesh.Result{Conn: conn, Kind: mesh.KindPeerLink}, nil
		} else if errors.Is(gerr, mesh.ErrNoPeerLink) {
			slog.Debug("本地网关无到目标节点的已建链路，回落常规拨号", "peer", target.Node, "addr", target.Addr)
		} else {
			ios.WriteErrLine("本地网关路由失败: %v（回落常规拨号）", gerr)
		}
		return mesh.Dial(ctx, svc, signaler, target, localNode)
	}
}

// NewCmdMesh 创建 mesh 父命令：基于 hub 服务注册表的服务发现与连接。
// cfgSvc 为可选配置提供者（mesh node 常驻节点用；hub/token/node-id 配置回落）。
func NewCmdMesh(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "mesh 服务发现与连接（webrtc 直连优先，hub 中继回落）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdMeshConnect(factory, ios))
	cmd.AddCommand(newCmdMeshStatus(factory, ios))
	cmd.AddCommand(newCmdMeshNode(ios, cfgSvc))
	return cmd
}

// newCmdMeshConnect 创建 mesh connect：按服务名连接（webrtc 优先，中继回落）。
func newCmdMeshConnect(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "connect <service> [-l :port]",
		Short: "连接到 mesh 服务（webrtc 直连优先，hub 中继回落）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service := args[0]
			listenAddr, _ := cmd.Flags().GetString("listen")
			useWebRTC, _ := cmd.Flags().GetBool("webrtc")
			hubURL, _ := cmd.Flags().GetString("hub")
			nodeID, _ := cmd.Flags().GetString("node-id")
			gatewayAddr, _ := cmd.Flags().GetString("gateway")
			insecure, _ := cmd.Flags().GetBool("insecure")
			stunServers, _ := cmd.Flags().GetStringSlice("stun")
			if stunServers != nil {
				webrtc.SetSTUNServers(stunServers)
			}
			turnServers, _ := cmd.Flags().GetStringSlice("turn")
			turnUser, _ := cmd.Flags().GetString("turn-user")
			turnPass, _ := cmd.Flags().GetString("turn-pass")
			if turnServers != nil {
				webrtc.SetTURNServers(turnServers)
			}
			if turnUser != "" || turnPass != "" {
				webrtc.SetTURNCredential(turnUser, turnPass)
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			// P2-配置3：通用 mesh 参数配置回落——--hub/--node-id 未显式指定时取配置
			// hub_url/node_id；hub 注册准入用 SproxySig AccessKey/SK（svc.AccessKey()
			// /AccessKeySecret() 已含 config + flag 覆盖），不需要额外 relay token。
			if hubURL == "" {
				hubURL = svc.MeshHubURL()
			}
			if nodeID == "" {
				nodeID = svc.NodeID()
			}

			// 按需解析服务 → 目标节点 + 地址（带 TTL 缓存与单飞刷新，感知节点上下线）。
			refresher := client.NewMeshTargetRefresher(svc, service)
			target, err := refresher.Resolve(cmd.Context())
			if err != nil {
				return err
			}
			ios.WriteOutLine("目标服务: %s（节点 %s, addr %s）", service, target.Node, target.Addr)

			// 构建信令器（webrtc 打洞用）。连接前自动注册自身（声明 per-node-secret
			// 能力），从 REG_OK:<secret> 拿 per-node secret 供 B3 服务端信令身份校验。
			var signaler *hub.HubSignaler
			if useWebRTC {
				if nodeID == "" {
					nodeID = iostream.LocalHostname("mesh-node")
				}
				r, regErr := mesh.AutoRegister(cmd.Context(), mesh.AutoRegisterParams{
					HubURL:          hubURL,
					ServerURL:       svc.ServerURL(),
					AccessKey:       svc.AccessKey(),
					AccessKeySecret: svc.AccessKeySecret(),
					NodeID:          nodeID,
					Prefix:          "mesh",
					ExactNode:       false,
					Insecure:        insecure,
				})
				if regErr != nil {
					// 注册失败不静默：warn + 回落中继（relay 路径只认 auth_token，
					// 与本机临时注册无关，独立可用）。
					ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", regErr)
				} else {
					signaler = r.Signaler
					// 信令结束/命令退出确定性关闭注册连接，防 WS 泄漏
					// （hub 侧断开即 RemoveIfOwned 移除临时节点）。
					defer func() { _ = r.Closer() }()
				}
			}
			localNode := nodeID
			if localNode == "" {
				localNode = iostream.LocalHostname("mesh-node")
			}

			// --gateway：先经本地 mesh node 网关复用已建直连链路（零重新打洞），
			// 本地节点无到目标的已建链路时回落常规拨号（不回归既有路径）。
			// 网关认证 token 复用信令 token（auth_token），与 mesh node 网关一致。
			dial := meshDialFunc(mesh.Dial)
			if gatewayAddr != "" {
				dial = meshGatewayDial(gatewayAddr, svc.AccessKeySecret(), ios)
			}

			if listenAddr != "" {
				return meshForwardListen(cmd, svc, signaler, dial, refresher, target, localNode, listenAddr, ios)
			}
			return meshStdioOnce(cmd, svc, signaler, dial, refresher, localNode, ios)
		},
	}
	cmd.Flags().StringP("listen", "l", "", "本地监听地址（如 127.0.0.1:2222；裸 :2222 归一为 127.0.0.1:2222）；留空为单次 stdin/stdout 模式")
	cmd.Flags().Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	cmd.Flags().String("hub", "", "hub 地址（http(s) 或 ws(s) 均可；默认取 server_url）")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	cmd.Flags().String("gateway", "", "经本地 mesh node 网关复用已建立直连链路路由（127.0.0.1:port；本地节点无到目标的已建链路时回落常规拨号）")
	cmd.Flags().StringSlice("stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
	cmd.Flags().StringSlice("turn", nil,
		"TURN 中继服务器地址（可重复/逗号分隔，如 turn:relay.example.com:3478）；需配合 --turn-user/--turn-pass，提升对称 NAT 下打洞成功率")
	cmd.Flags().String("turn-user", "", "TURN 用户名（静态密码模式，配 --turn/--turn-pass 使用）")
	cmd.Flags().String("turn-pass", "", "TURN 密码（静态密码模式，配 --turn/--turn-user 使用）")
	return cmd
}

// newCmdMeshStatus 创建 mesh status：列出 hub 上的 mesh 服务；
// 指定 --gateway 时改查本地 mesh node 网关拓扑（node-id + 服务宣告 + 已建直连链路）。
func newCmdMeshStatus(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "列出 hub 上的 mesh 服务（或 --gateway 查本地节点直连拓扑）",
		RunE: func(cmd *cobra.Command, args []string) error {
			gatewayAddr, _ := cmd.Flags().GetString("gateway")
			if gatewayAddr != "" {
				// 网关认证：查询拓扑需与 mesh node 相同的 auth_token（经配置/--auth-token）。
				svc, err := factory.NewClient(cmd)
				if err != nil {
					return err
				}
				st, err := mesh.QueryGatewayStatus(cmd.Context(), gatewayAddr, svc.AccessKeySecret())
				if err != nil {
					return err
				}
				ios.WriteOutLine("mesh 节点: %s", st.NodeID)
				if len(st.Services) == 0 {
					ios.WriteOutLine("服务宣告: 无")
				} else {
					ios.WriteOutLine("服务宣告 (%d):", len(st.Services))
					for _, s := range st.Services {
						ios.WriteOutLine("  %-24s addr=%s", s.Name, s.Addr)
					}
				}
				if len(st.Peers) == 0 {
					ios.WriteOutLine("已建直连链路: 无")
				} else {
					ios.WriteOutLine("已建直连链路 (%d):", len(st.Peers))
					for _, p := range st.Peers {
						ios.WriteOutLine("  %-24s link=%s  since=%s", p.Peer, p.Link, p.Since.Format(time.RFC3339))
					}
				}
				return nil
			}
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			svcs, err := svc.MeshServices(cmd.Context())
			if err != nil {
				return err
			}
			if len(svcs) == 0 {
				ios.WriteOutLine("暂无 mesh 服务")
				return nil
			}
			ios.WriteOutLine("mesh 服务 (%d):", len(svcs))
			for _, s := range svcs {
				addr := s.Addr
				if addr == "" {
					addr = "-"
				}
				ios.WriteOutLine("  %-24s node=%s  addr=%s", s.Name, s.Node, addr)
			}
			return nil
		},
	}
	cmd.Flags().String("gateway", "", "查询本地 mesh node 网关拓扑（127.0.0.1:port；node-id + 服务宣告 + 已建直连链路/链路类型）")
	return cmd
}

// meshForwardListen 监听本地端口，每个入站连接独立建立一条 mesh 连接（选路 dial）。
// ref 负责按需解析最新 target（带 TTL 缓存，感知节点上下线）；initial 仅用于启动横幅。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, ref *client.MeshTargetRefresher, initial *client.MeshService, localNode, listenAddr string, ios cli.IOStreams) error {
	// S56：裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 +
	// Windows 防火墙弹窗）；需 LAN 访问时显式通配地址:port 或具体 IP。
	listenAddr = iostream.NormalizeListenAddr(listenAddr)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	if initial != nil {
		ios.WriteOutLine("端口转发: %s ⇄ mesh(%s) ⇄ %s", listenAddr, initial.Node, initial.Addr)
	}

	ctx := cmd.Context()
	// ctx 取消时关闭 listener，使 Accept 立即返回（优雅停止端口转发）。
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		local, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return aerr
		}
		go func(c net.Conn) {
			defer c.Close()
			// 每个连接用最新 target：服务已下线（不在列表）→ 立即清晰报错并关闭，
			// 不再等 webrtc 30s ICE 超时（静默卡死）。
			target, rerr := ref.Resolve(ctx)
			if rerr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", rerr)
				return
			}
			res, cerr := dial(ctx, svc, signaler, target, localNode)
			if cerr != nil {
				// dial 失败（relay 404 / webrtc 失败）→ 强制缓存过期 + 记录失败节点，
				// 下个连接立即重取并优先跳过该节点（P1-13 候选 failover）。
				ref.Invalidate(target.Node)
				ios.WriteErrLine("建立 mesh 流失败: %v（目标 node=%s addr=%s 不可达或离线）", cerr, target.Node, target.Addr)
				return
			}
			conn := res.Conn
			defer conn.Close()
			// 拨号帧已由 dial 内部写好（P0-1）：relay 由 hub 写，webrtc 由
			// mesh.WebRTCStream 在 mux 流上写，客户端均直接透传。
			ios.WriteOutLine("连接已建立（%s）: %s ⇄ %s", res.Kind, target.Node, target.Addr)
			// 双向泵送（CloseWrite 半关闭 + grace 宽限期，C1 范本，见 iostream.Pump）：
			// 任一方向完成即向对端传播半关闭，让在途响应仍可被读回；对端不回应 FIN
			// 时 grace 超时强制双侧关闭解除阻塞。返回后由外层 defer 收尾。
			iostream.Pump(c, conn, iostream.PumpGrace)
		}(local)
	}
}

// meshStdioOnce 单次模式：stdin/stdout 与一条 mesh 连接直通（选路 dial）。
// ref 负责解析最新 target（单次拨号使用当前缓存；失败返回错误可由调用方重试）。
func meshStdioOnce(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, ref *client.MeshTargetRefresher, localNode string, ios cli.IOStreams) error {
	target, err := ref.Resolve(cmd.Context())
	if err != nil {
		return err
	}
	res, err := dial(cmd.Context(), svc, signaler, target, localNode)
	if err != nil {
		return err
	}
	conn := res.Conn
	defer conn.Close()
	// 拨号帧已由 dial 内部写好（P0-1，同 meshForwardListen）：relay 由 hub 写，
	// webrtc 由 mesh.WebRTCStream 在 mux 流上写，客户端均直接透传。
	ios.WriteOutLine("已连接（%s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.Kind, target.Name)
	// 方向区分通道（I38）：对端断开（outDone）→ 会话结束立即返回，不再挂起；
	// 本地 stdin 读完（inDone，如 EOF/管道结束）→ 等待对端把剩余响应写完
	// （保留 `echo x | mesh connect` 的响应语义）。原 wg.Wait() 在对端断开但
	// stdin 未 EOF 时永久挂起。
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(conn, ios.In)
		// P0-5：stdin EOF 后必须向对端传播半关闭（FIN / 流 EOF），否则对端
		// 永远等不到"输入写完"，<outDone 永久挂起（I38/C2 声称修复的挂死，
		// 在同一批代码的另一方向仍然存在——此处补上）。委托 iostream.CloseWrite。
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
