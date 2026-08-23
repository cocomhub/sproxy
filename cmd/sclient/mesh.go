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
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层（自动注册拨号用）
	"github.com/spf13/cobra"
)

// meshDialResult 是一次 mesh 连接的结果（薄包装 mesh.Result，保持测试签名稳定）。
type meshDialResult struct {
	conn net.Conn
	// kind 是实际使用的路径：webrtc | relay。
	kind string
}

// meshDialFunc 建立一条到目标服务的连接（选路逻辑）。
// 默认实现：webrtc 打洞优先，失败回落 hub 中继。可注入测试桩。
type meshDialFunc func(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*meshDialResult, error)

// defaultMeshDial 是默认选路：webrtc 打洞优先，失败回落 hub 中继。
// 委托 pkg/tunnel/mesh.Dial（选路逻辑已下沉，含 P1-12 探测超时约束）。
func defaultMeshDial(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, _ string) (*meshDialResult, error) {
	res, err := mesh.Dial(ctx, svc, signaler, target, "")
	if err != nil {
		return nil, err
	}
	return &meshDialResult{conn: res.Conn, kind: res.Kind}, nil
}

// meshWebRTCStream 在已建立的 WebRTC 直连上打开 mux 流并写好拨号帧。
// 委托 pkg/tunnel/mesh.WebRTCStream（含 P0-1 mux 分帧修复）。
func meshWebRTCStream(ctx context.Context, conn *webrtc.Conn, addr string) (*meshDialResult, error) {
	res, err := mesh.WebRTCStream(ctx, conn, addr)
	if err != nil {
		return nil, err
	}
	return &meshDialResult{conn: res.Conn, kind: res.Kind}, nil
}

// newMeshTargetRefresher 创建 refresher（TTL 单飞缓存 + 失败节点 failover）。
// 委托 pkg/client.MeshTargetRefresher。
func newMeshTargetRefresher(svc *client.FileClient, service string) *client.MeshTargetRefresher {
	return client.NewMeshTargetRefresher(svc, service)
}

// NewCmdMesh 创建 mesh 父命令：基于 hub 服务注册表的服务发现与连接。
func NewCmdMesh(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mesh",
		Short: "mesh 服务发现与连接（webrtc 直连优先，hub 中继回落）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdMeshConnect(factory, ios))
	cmd.AddCommand(newCmdMeshStatus(factory, ios))
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
			token, _ := cmd.Flags().GetString("token")
			relayToken, _ := cmd.Flags().GetString("relay-token")
			nodeID, _ := cmd.Flags().GetString("node-id")
			insecure, _ := cmd.Flags().GetBool("insecure")
			stunServers, _ := cmd.Flags().GetStringSlice("stun")
			if stunServers != nil {
				webrtc.SetSTUNServers(stunServers)
			}

			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			// P2-配置3：通用 mesh 参数配置回落——--hub/--relay-token/--node-id 未显式
			// 指定时取配置 hub_url/relay_token/node_id（优先级：CLI > 配置文件 > 默认）。
			if hubURL == "" {
				hubURL = svc.MeshHubURL()
			}
			if relayToken == "" {
				relayToken = svc.RelayToken()
			}
			if nodeID == "" {
				nodeID = svc.NodeID()
			}

			// 按需解析服务 → 目标节点 + 地址（带 TTL 缓存与单飞刷新，感知节点上下线）
			refresher := newMeshTargetRefresher(svc, service)
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
					nodeID = defaultLocalNodeID()
				}
				r, regErr := autoRegister(cmd.Context(), autoRegisterParams{
					hubURL:      hubURL,
					serverURL:   svc.ServerURL(),
					relayToken:  meshRelayToken(relayToken, token, svc),
					signalToken: meshSignalToken(token, svc),
					nodeID:      nodeID,
					prefix:      "mesh",
					exactNode:   false,
					insecure:    insecure,
				})
				if regErr != nil {
					// 注册失败不静默：warn + 回落中继（relay 路径只认 auth_token，
					// 与本机临时注册无关，独立可用）。
					ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", regErr)
				} else {
					signaler = r.signaler
					// 信令结束/命令退出确定性关闭注册连接，防 WS 泄漏
					// （hub 侧断开即 RemoveIfOwned 移除临时节点）。
					defer func() { _ = r.closer() }()
				}
			}
			dial := defaultMeshDial
			localNode := nodeID
			if localNode == "" {
				localNode = defaultLocalNodeID()
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
	cmd.Flags().String("token", "", "信令 Bearer token（默认复用 --auth-token / 配置 auth_token）")
	cmd.Flags().String("relay-token", "", "hub 的 relay_token（自动注册用；与 relay start --token 一致；默认复用 --token / auth_token）")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	cmd.Flags().StringSlice("stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
	return cmd
}

// newCmdMeshStatus 创建 mesh status：列出 hub 上所有 mesh 服务。
func newCmdMeshStatus(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "列出 hub 上的 mesh 服务",
		RunE: func(cmd *cobra.Command, args []string) error {
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
}

// meshForwardListen 监听本地端口，每个入站连接独立建立一条 mesh 连接（选路 dial）。
// ref 负责按需解析最新 target（带 TTL 缓存，感知节点上下线）；initial 仅用于启动横幅。
func meshForwardListen(cmd *cobra.Command, svc *client.FileClient, signaler *hub.HubSignaler, dial meshDialFunc, ref *client.MeshTargetRefresher, initial *client.MeshService, localNode, listenAddr string, ios cli.IOStreams) error {
	// S56：裸 :port 归一为 127.0.0.1:port（loopback 安全默认，防 LAN 暴露 +
	// Windows 防火墙弹窗）；需 LAN 访问时显式 0.0.0.0:port / 具体 IP。
	listenAddr = normalizeListenAddr(listenAddr)
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
			conn := res.conn
			defer conn.Close()
			// 拨号帧已由 dial 内部写好（P0-1）：
			//   - relay：hub 的 RelayStreamHandler 已写好 dial 帧（叶子拨目标），
			//     客户端直接透传字节，不得再写帧；
			//   - webrtc：meshWebRTCStream 已在 mux 流上写好 dial 帧，此处同样透传。
			// 客户端不再按路径手动写帧——曾把帧以裸字节写 DataChannel 导致直连 100% 失败。
			ios.WriteOutLine("连接已建立（%s）: %s ⇄ %s", res.kind, target.Node, target.Addr)
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
	conn := res.conn
	defer conn.Close()
	// 拨号帧已由 dial 内部写好（P0-1，同 meshForwardListen）：relay 由 hub 写，
	// webrtc 由 meshWebRTCStream 在 mux 流上写，客户端均直接透传。
	ios.WriteOutLine("已连接（%s）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", res.kind, target.Name)
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

// meshSignalToken 返回信令 Bearer token：显式 --token 优先，否则复用 auth_token。
// 委托 pkg/client.MeshSignalToken。
func meshSignalToken(flagToken string, svc *client.FileClient) string {
	return client.MeshSignalToken(flagToken, svc.AuthToken())
}

// meshRelayToken 返回自动注册用的 relay_token：显式 --relay-token > 配置 relay_token
// > --token > auth_token。委托 pkg/client.MeshRelayToken（含 P2-配置3 回落链）。
func meshRelayToken(flagRelayToken, flagToken string, svc *client.FileClient) string {
	return client.MeshRelayToken(flagRelayToken, svc.RelayToken(), flagToken, svc.AuthToken())
}

// defaultLocalNodeID 返回本机节点 ID（mesh webrtc 信令来源）。
// 委托 pkg/iostream.LocalHostname（fallback "mesh-node"）。
func defaultLocalNodeID() string {
	return iostream.LocalHostname("mesh-node")
}

// normalizeHubEndpoints 将 hub 地址归一为信令 HTTP 基址与注册 WS 端点。
// 委托 pkg/tunnel/hub.NormalizeEndpoints。
func normalizeHubEndpoints(hubURL, serverURL string) (httpBase, wsURL string, err error) {
	return hub.NormalizeEndpoints(hubURL, serverURL)
}

// normalizeListenAddr 将裸 :port 归一为 127.0.0.1:port（loopback 安全默认）。
// 委托 pkg/iostream.NormalizeListenAddr。
func normalizeListenAddr(addr string) string {
	return iostream.NormalizeListenAddr(addr)
}

// meshTempRegistration 是一次 mesh connect 的临时注册（生命周期与本次命令绑定）。
// 薄包装 pkg/tunnel/mesh.TempRegistration，保持测试签名稳定。
type meshTempRegistration struct {
	signaler *hub.HubSignaler // 携带临时 node_id + per-node secret
	closer   func() error     // 关闭注册连接 → hub 移除临时节点
	tempNode string           // 临时节点 ID（调试/日志用）
}

// autoRegisterParams 是一次自动注册（mesh/p2p 信令前置）的参数。
// 与 meshAutoRegister 的区别：支持 temp/exact 两种 node 生成模式与 insecure TLS，
// 供 mesh connect（temp）、p2p connect（temp）、p2p listen（exact）三处复用（B17）。
type autoRegisterParams struct {
	hubURL      string // hub 地址（空时回落 serverURL）
	serverURL   string // hubURL 为空的回退基址（mesh 用 svc.ServerURL()；p2p 传 ""）
	relayToken  string // 注册用（hub relay_token）
	signalToken string // 信令 Bearer（hub auth_token）
	nodeID      string // 节点 ID 基础
	prefix      string // 临时 node 前缀："mesh" | "p2p"
	exactNode   bool   // true=注册成 nodeID 原样（p2p listen 需稳定 ID 供 --peer 寻址）；false=临时 nodeID
	insecure    bool   // 缺口 2：注册 WS 拨号 + HubSignaler HTTP 跳过证书校验（自签 wss hub）
}

// autoRegister 是 mesh/p2p 共用的信令自动注册实现。委托 pkg/tunnel/mesh.AutoRegister
// （声明 per-node-secret 能力、解析 REG_OK:<secret>、mux 保活、per-node secret 校验），
// 本层仅做参数适配与结果转换。insecure 语义由 mesh.AutoRegister 内部处理。
func autoRegister(ctx context.Context, p autoRegisterParams) (*meshTempRegistration, error) {
	reg, err := mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:      p.hubURL,
		ServerURL:   p.serverURL,
		RelayToken:  p.relayToken,
		SignalToken: p.signalToken,
		NodeID:      p.nodeID,
		Prefix:      p.prefix,
		ExactNode:   p.exactNode,
		Insecure:    p.insecure,
	})
	if err != nil {
		return nil, err
	}
	return &meshTempRegistration{signaler: reg.Signaler, closer: reg.Closer, tempNode: reg.TempNode}, nil
}

// meshAutoRegister 连接前自动注册（B12 语义不变）：mesh connect 的薄包装，
// 固定 prefix="mesh"、temp node 模式、insecure=false。
func meshAutoRegister(ctx context.Context, svc *client.FileClient, hubURL, relayToken, signalToken, nodeID string) (*meshTempRegistration, error) {
	return autoRegister(ctx, autoRegisterParams{
		hubURL:      hubURL,
		serverURL:   svc.ServerURL(),
		relayToken:  relayToken,
		signalToken: signalToken,
		nodeID:      nodeID,
		prefix:      "mesh",
		exactNode:   false,
		insecure:    false,
	})
}
