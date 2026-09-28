// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/spf13/cobra"
)

// newCmdUDP 创建 sclient udp 父命令（当前含 map 子命令）。
func newCmdUDP(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "udp",
		Short: "UDP 隧道（端口映射）",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(newCmdUDPMap(factory, ios, cfgSvc))
	return cmd
}

// routeExitNode 是 udp map 的分流选路：remote host 命中 --route 规则时返回组内
// 第一个节点（UDP 映射是单 mux 固定出口，组内 failover 不适用）；未命中回落
// 默认 --exit 节点。
func routeExitNode(conn *meshconn.Conn, remote string) string {
	if group := conn.SelectRoute(remote); len(group) > 0 {
		return group[0]
	}
	return conn.ExitNode
}

// udpMapResolveExit 校验 udp map 参数并解析出口节点：--exit 必填（不支持
// --exit-auto——UDP 映射是单 mux 固定出口，P1-2 fail-closed），--remote 必填且本地
// 预校验 host:port（出口节点还会经拨号策略再次校验，防 SSRF）。remote host 命中
// --route 路由组时替换出口节点（仍单 mux 固定出口，组内 failover 不适用，取组内
// 第一个节点）。
func udpMapResolveExit(conn *meshconn.Conn, remote string) (string, error) {
	if conn.ExitAuto {
		return "", fmt.Errorf("udp map 需要固定 --exit 出口节点（不支持 --exit-auto；UDP 映射是单 mux 固定出口）")
	}
	if conn.ExitNode == "" {
		return "", fmt.Errorf("--exit（出口节点）与 --remote（远程 UDP 地址）均必填")
	}
	if remote == "" {
		return "", fmt.Errorf("--exit（出口节点）与 --remote（远程 UDP 地址）均必填")
	}
	exitNode := routeExitNode(conn, remote)
	if _, rerr := net.ResolveUDPAddr("udp", remote); rerr != nil {
		return "", fmt.Errorf("--remote 目标地址非法（应为 host:port）: %w", rerr)
	}
	return exitNode, nil
}

// udpMapApplyConfig 用 svc 补齐 hub/node-id/mdns-secret 配置回落（对齐既有 T6b 模式）。
// svc 为 nil（--mdns 可无客户端）时跳过回落；node-id 为空回落主机名。
func udpMapApplyConfig(conn *meshconn.Conn, svc *client.FileClient) {
	if conn.HubURL == "" && svc != nil {
		conn.HubURL = svc.MeshHubURL()
	}
	if conn.NodeID == "" && svc != nil {
		conn.NodeID = svc.NodeID()
	}
	if conn.NodeID == "" {
		conn.NodeID = iostream.LocalHostname("mesh-node")
	}
	if conn.MDNSSecret == "" && svc != nil {
		conn.MDNSSecret = svc.AccessKeySecret()
	}
}

// udpMapSignaler 建立到出口节点的信令器：--mdns 用 mDNS 直连信令（BrowseOnly，
// 不注册 hub），否则经 hub 自动注册（SproxySig 凭据，REG_OK 下发 per-node secret）。
// 返回 signaler 与两个清理函数：closeMDNS（mDNS 实例，hub 路径为 nil）与
// closeSignaler（信令器/注册连接 close，命令退出时确定性关闭防泄漏）。
func udpMapSignaler(ctx context.Context, cmd *cobra.Command, conn *meshconn.Conn, svc *client.FileClient, cfgSvc ConfigProvider, exitNode string) (webrtc.Signaler, func() error, func() error, error) {
	if conn.MDNS {
		ms, merr := mesh.NewMDNS(mesh.MDNSConfig{NodeID: conn.NodeID, BrowseOnly: true, Secret: conn.MDNSSecret})
		if merr != nil {
			return nil, nil, nil, fmt.Errorf("mDNS 初始化失败: %w", merr)
		}
		if merr := ms.Start(ctx); merr != nil {
			return nil, nil, nil, fmt.Errorf("mDNS 启动失败: %w", merr)
		}
		peer, perr := ms.LookupPeer(ctx, exitNode, meshconn.DefaultMDNSLookupTimeout)
		if perr != nil {
			_ = ms.Close()
			return nil, nil, nil, fmt.Errorf("mDNS 未发现出口节点 %s: %w", exitNode, perr)
		}
		if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
			_ = ms.Close()
			return nil, nil, nil, verr
		}
		sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, conn.NodeID)
		if serr != nil {
			_ = ms.Close()
			return nil, nil, nil, fmt.Errorf("直连信令失败: %w", serr)
		}
		sig.SetSecret(conn.MDNSSecret)
		return sig, ms.Close, sig.Close, nil
	}
	if svc == nil {
		return nil, nil, nil, fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
	}
	caFile, _ := cmd.Flags().GetString("ca-file")
	if caFile == "" {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			caFile = cfg.XferCAFile
		}
	}
	r, regErr := mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
		HubURL:          conn.HubURL,
		ServerURL:       svc.ServerURL(),
		AccessKey:       svc.AccessKey(),
		AccessKeySecret: svc.AccessKeySecret(),
		AccessKeyID:     svc.AccessKeyID(),
		NodeID:          conn.NodeID,
		Prefix:          "mesh",
		ExactNode:       false,
		Insecure:        conn.Insecure,
		CAFile:          caFile,
	})
	if regErr != nil {
		return nil, nil, nil, fmt.Errorf("webrtc 信令注册失败: %w", regErr)
	}
	return r.Signaler, nil, r.Closer, nil
}

// udpMapDatagramBridge 装配双向 UDP 数据面：
//   - 出口响应 → 回传本地 UDP 客户端（异步写 + 信号量防慢消费者拖垮 readLoop；
//     data 是 DecodeFrame 的独立拷贝，可安全交给 goroutine）；
//   - 本地 UDP 数据报 → 经 mesh 到出口（读缓冲对齐 MaxDatagramPayload，防超长触发
//     ErrDatagramTooLarge 杀映射；瞬时错误 log+continue）。
func udpMapDatagramBridge(udpLn *net.UDPConn, m *mux.Mux, logger *slog.Logger) {
	var mu sync.Mutex
	var clientAddr *net.UDPAddr
	writeSem := make(chan struct{}, 64)
	m.SetDatagramHandler(func(flowID uint32, data []byte) {
		mu.Lock()
		addr := clientAddr
		mu.Unlock()
		if addr == nil {
			return
		}
		select {
		case writeSem <- struct{}{}:
		default:
			return // 写信号量满（本地消费跟不上），丢弃（UDP 语义）
		}
		go func(addr *net.UDPAddr, data []byte) {
			defer func() { <-writeSem }()
			_, _ = udpLn.WriteToUDP(data, addr)
		}(addr, data)
	})
	go func() {
		buf := make([]byte, mux.MaxDatagramPayload)
		for {
			n, addr, rerr := udpLn.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			mu.Lock()
			clientAddr = addr
			mu.Unlock()
			if serr := m.SendDatagram(0, buf[:n]); serr != nil {
				// 拥塞丢弃（ErrDatagramDrop）属 UDP 语义，Debug 级避免刷屏。
				logger.Debug("UDP 发送经 mesh 失败（丢弃）", "error", serr)
				if errors.Is(serr, mux.ErrMuxClosed) {
					return
				}
			}
		}
	}()
}

// newCmdUDPMap 创建 sclient udp map：本地 UDP 端口经 mesh 映射到出口节点的远程 UDP
// 地址——本地 UDP 数据报经 mesh（FrameDatagram）到出口，出口转发到 --remote 目标；
// 响应原路回传（双向 UDP 转发）。
func newCmdUDPMap(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "map -l :udp --exit <node> --remote <host:port>",
		Short: "UDP 端口映射（经 mesh 到出口节点，出口转发到远程 UDP 地址）",
		Long: `把本地 UDP 端口映射到出口节点的远程 UDP 地址：本地 UDP 数据报经 mesh
（webrtc 直连 + mux FrameDatagram）到出口节点，出口转发到 --remote 目标；目标响应
原路回传本地（双向 UDP 转发）。

安全边界：与 TCP dial 帧同属"出口模式"，出口节点须运行 mesh node（含 webrtc 直连
环）并开启 --dial-allow；--remote 目标还须通过出口节点拨号策略（默认仅公网 + 宣告的
服务地址，防 SSRF）。注意：出口拒绝目标（策略不通过）时无回帧，本命令仍会打印
"UDP 映射就绪"但无数据流转——请在出口节点把目标加入 --dial-allow-cidr 或宣告为服务
地址。本地监听默认 loopback。

使用示例:
  sclient udp map -l :5300 --exit node-b --remote 8.8.8.8:53
  # 转发到出口本机 loopback 目标时，需出口节点宣告该服务或放行网段：
  #   mesh node ... --service dns:127.0.0.1:53  或  --dial-allow-cidr 127.0.0.1/32`,
		RunE: func(cmd *cobra.Command, args []string) error {
			listenAddr, _ := cmd.Flags().GetString("listen")
			remote, _ := cmd.Flags().GetString("remote")

			// mesh 连接参数组统一装配（flag + 配置回落 + 互斥/fail-closed 校验）。
			conn := &meshconn.Conn{}
			if err := conn.FromFlags(cmd, cfgSvc); err != nil {
				return err
			}
			// --exit 必填（不支持 --exit-auto，UDP 映射是单 mux 固定出口）与 --remote
			// 本地预校验（出口节点还会经拨号策略再次校验，防 SSRF）；分流规则（--route）
			// 命中时替换出口节点，见 udpMapResolveExit。
			exitNode, err := udpMapResolveExit(conn, remote)
			if err != nil {
				return err
			}
			// 应用 STUN/TURN 全局配置（webrtc 打洞候选收集用）。
			if err = applyTURNRESTFlags(cmd); err != nil {
				return err
			}
			webrtcSetSTUNImpl(conn)

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "udp")
			svc, svcErr := factory.NewClient(cmd)
			if svcErr != nil {
				// 不吞错误：配置加载失败会导致 hub 模式报"无可用 mesh 路由"（真实根因
				// 被隐藏），此处打 Warn 供排查；--mdns 可无客户端，不影响。
				logger.Warn("创建客户端失败（hub 模式将无可用 mesh 路由；--mdns 可忽略）", "error", svcErr)
				svc = nil
			}
			// 配置回落：hub/node-id/mdns-secret 需 svc（对齐既有 T6b 模式）。
			udpMapApplyConfig(conn, svc)

			// Ctrl+C/SIGTERM 优雅收尾（ctx 取消 → 有序关闭 mux/控制流，出口 UDP 映射
			// 随之回收）。
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// 建立到出口节点的信令器（hub 自动注册或 mDNS 直连）。
			signaler, closeMDNS, closeSignaler, err := udpMapSignaler(ctx, cmd, conn, svc, cfgSvc, exitNode)
			if err != nil {
				return err
			}
			if closeMDNS != nil {
				defer func() { _ = closeMDNS() }()
			}
			defer func() { _ = closeSignaler() }()

			// 建立 UDP 映射 mux + 控制流。
			m, control, oerr := mesh.OpenUDPMux(ctx, signaler, exitNode, remote)
			if oerr != nil {
				return fmt.Errorf("建立 UDP 映射失败: %w", oerr)
			}
			// 收尾顺序（LIFO）：先 m.Close 关闭 mux（解除流/读阻塞），再用 control.Abort
			// 非阻塞放弃控制流（绝不用 Close——writeCh 满时 Close 会永久阻塞，造成
			// Ctrl+C 收尾死锁）。
			defer func() { _ = control.Abort() }()
			defer func() { _ = m.Close() }()

			// 本地 UDP 监听。
			listenAddr = iostream.NormalizeListenAddr(listenAddr)
			udpAddr, aerr := net.ResolveUDPAddr("udp", listenAddr)
			if aerr != nil {
				return fmt.Errorf("解析本地 UDP 地址失败: %w", aerr)
			}
			udpLn, lerr := net.ListenUDP("udp", udpAddr)
			if lerr != nil {
				return fmt.Errorf("监听本地 UDP 失败: %w", lerr)
			}
			defer func() { _ = udpLn.Close() }()

			// 双向 UDP 数据面装配（出口响应回传 / 本地数据报到出口）。
			udpMapDatagramBridge(udpLn, m, logger)

			exitDesc := exitNode
			if conn.ExitAuto {
				exitDesc = "auto"
			}
			ios.WriteOutLine("UDP 映射就绪: %s ⇄ mesh(%s) ⇄ %s（Ctrl+C 退出）", udpLn.LocalAddr().String(), exitDesc, remote)
			// 主循环：ctx 取消（Ctrl+C）优雅退出；mux 死亡（出口重启/网络断）报错退出，
			// 不静默永久挂起（客户端"就绪"后零数据流应可感知）。
			select {
			case <-ctx.Done():
				logger.Info("UDP 映射结束")
				return nil
			case <-m.Done():
				return fmt.Errorf("mesh 连接已断开（UDP 映射终止，出口节点可能已重启/网络中断）")
			}
		},
	}
	cmd.Flags().StringP("listen", "l", "127.0.0.1:0", "本地 UDP 监听地址（裸 :port 归一 127.0.0.1:port；默认随机端口）")
	cmd.Flags().String("remote", "", "出口节点侧的远程 UDP 目标地址 host:port（必填；出口仅转发到该地址，且需在出口节点 --dial-allow 放行/宣告的服务范围内）")
	// mesh 连接参数组（hub/node-id/webrtc/insecure/stun/turn/gateway/smart/mdns）
	meshconn.AddFlags(cmd)
	// 出口路由 flag 族（--exit/--exit-auto/--exit-only/--exit-exclude/--local-timeout）
	meshconn.AddExitFlags(cmd)
	addTURNRESTFlags(cmd)
	return cmd
}
