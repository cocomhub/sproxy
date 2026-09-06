// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
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
			exit, _ := cmd.Flags().GetString("exit")
			remote, _ := cmd.Flags().GetString("remote")
			mdns, _ := cmd.Flags().GetBool("mdns")
			mdnsSecret, _ := cmd.Flags().GetString("mdns-secret")
			hubURL, _ := cmd.Flags().GetString("hub")
			nodeID, _ := cmd.Flags().GetString("node-id")
			insecure, _ := cmd.Flags().GetBool("insecure")
			if exit == "" || remote == "" {
				return fmt.Errorf("--exit（出口节点）与 --remote（远程 UDP 地址）均必填")
			}
			// 本地预校验 --remote（出口节点还会经拨号策略再次校验，防 SSRF）。
			if _, rerr := net.ResolveUDPAddr("udp", remote); rerr != nil {
				return fmt.Errorf("--remote 目标地址非法（应为 host:port）: %w", rerr)
			}
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
			if err := applyTURNRESTFlags(cmd); err != nil {
				return err
			}

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "udp")
			svc, svcErr := factory.NewClient(cmd)
			if svcErr != nil {
				// 不吞错误：配置加载失败会导致 hub 模式报"无可用 mesh 路由"（真实根因
				// 被隐藏），此处打 Warn 供排查；--mdns 可无客户端，不影响。
				logger.Warn("创建客户端失败（hub 模式将无可用 mesh 路由；--mdns 可忽略）", "error", svcErr)
				svc = nil
			}
			if hubURL == "" && svc != nil {
				hubURL = svc.MeshHubURL()
			}
			if nodeID == "" && svc != nil {
				nodeID = svc.NodeID()
			}
			if nodeID == "" {
				nodeID = iostream.LocalHostname("mesh-node")
			}
			if mdnsSecret == "" && svc != nil {
				mdnsSecret = svc.AccessKeySecret()
			}

			// Ctrl+C/SIGTERM 优雅收尾（ctx 取消 → 有序关闭 mux/控制流，出口 UDP 映射
			// 随之回收）。
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// 建立到出口节点的信令器（hub 或 mDNS 直连）。
			var signaler webrtc.Signaler
			var closeSignaler func() error
			if mdns {
				ms, merr := mesh.NewMDNS(mesh.MDNSConfig{NodeID: nodeID, BrowseOnly: true, Secret: mdnsSecret})
				if merr != nil {
					return fmt.Errorf("mDNS 初始化失败: %w", merr)
				}
				if merr := ms.Start(ctx); merr != nil {
					return fmt.Errorf("mDNS 启动失败: %w", merr)
				}
				defer ms.Close()
				peer, perr := ms.LookupPeer(ctx, exit, mdnsLookupTimeout)
				if perr != nil {
					return fmt.Errorf("mDNS 未发现出口节点 %s: %w", exit, perr)
				}
				if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
					return verr
				}
				sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, nodeID)
				if serr != nil {
					return fmt.Errorf("直连信令失败: %w", serr)
				}
				sig.SetSecret(mdnsSecret)
				signaler = sig
				closeSignaler = sig.Close
			} else {
				if svc == nil {
					return fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
				}
				r, regErr := mesh.AutoRegister(ctx, mesh.AutoRegisterParams{
					HubURL:          hubURL,
					ServerURL:       svc.ServerURL(),
					AccessKey:       svc.AccessKey(),
					AccessKeySecret: svc.AccessKeySecret(),
					AccessKeyID:     svc.AccessKeyID(),
					NodeID:          nodeID,
					Prefix:          "mesh",
					ExactNode:       false,
					Insecure:        insecure,
				})
				if regErr != nil {
					return fmt.Errorf("webrtc 信令注册失败: %w", regErr)
				}
				signaler = r.Signaler
				closeSignaler = r.Closer
			}
			defer func() { _ = closeSignaler() }()

			// 建立 UDP 映射 mux + 控制流。
			m, control, oerr := mesh.OpenUDPMux(ctx, signaler, exit, remote)
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

			var mu sync.Mutex
			var clientAddr *net.UDPAddr
			// 出口响应 → 回传本地 UDP 客户端（异步写 + 信号量防慢消费者拖垮 readLoop）。
			// data 是 DecodeFrame 的独立拷贝，可安全交给 goroutine。
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

			// 本地 UDP 数据报 → 经 mesh 到出口（读缓冲对齐 MaxDatagramPayload，防
			// 超长触发 ErrDatagramTooLarge 杀映射；瞬时错误 log+continue）。
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

			ios.WriteOutLine("UDP 映射就绪: %s ⇄ mesh(%s) ⇄ %s（Ctrl+C 退出）", udpLn.LocalAddr().String(), exit, remote)
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
	cmd.Flags().String("exit", "", "出口节点 node-id（必填）")
	cmd.Flags().String("remote", "", "出口节点侧的远程 UDP 目标地址 host:port（必填；出口仅转发到该地址，且需在出口节点 --dial-allow 放行/宣告的服务范围内）")
	cmd.Flags().Bool("mdns", false, "纯 mDNS 直连（不经 hub）：经 mDNS 发现出口节点信令端点")
	cmd.Flags().String("mdns-secret", "", "mDNS 模式共享密钥（与出口节点 mesh node --mdns-secret 一致）")
	cmd.Flags().String("hub", "", "hub 地址（http(s)/ws(s)；默认取配置 hub_url，再回落 server_url）")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	cmd.Flags().Bool("insecure", false, "跳过 TLS 证书验证（自签 wss hub）")
	cmd.Flags().StringSlice("stun", nil, "STUN 服务器地址（可重复/逗号分隔）")
	cmd.Flags().StringSlice("turn", nil, "TURN 服务器地址（可重复/逗号分隔）")
	cmd.Flags().String("turn-user", "", "TURN 用户名")
	cmd.Flags().String("turn-pass", "", "TURN 密码")
	addTURNRESTFlags(cmd)
	return cmd
}
