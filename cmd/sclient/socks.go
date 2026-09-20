// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/socks5"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws" // 注册 WebSocket 传输层
	"github.com/spf13/cobra"
)

// newCmdSocks 创建 sclient socks：启动本地 SOCKS5 代理，CONNECT 目标经 mesh 路由到
// 指定出口节点（--exit），由出口节点按 dial 帧出站拨号（出口 dial 策略把关，防 SSRF）。
//
// 安全边界（对齐 mesh 网关）：
//   - 监听默认 loopback-only（NormalizeListenAddr，裸 :port → 127.0.0.1:port）；
//     LAN 暴露需显式监听地址（如全零 IPv4 通配:1080）。
//   - 可选 RFC 1929 用户名/密码认证（--socks-user/--socks-pass，配置了才要求）。
//   - 目标由出口节点 dial 策略（NewServiceDialPolicy：--dial-allow/--dial-allow-cidr/
//     宣告服务地址）把关，本地代理不直接拨任意地址。
func newCmdSocks(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "socks [-l :port] --exit <node>",
		Short: "启动 SOCKS5 代理（CONNECT 经 mesh 到指定出口节点，出口出站拨号）",
		Long: `启动本地 SOCKS5 代理：客户端（如 curl --socks5-hostname）经本代理 CONNECT 任意
目标地址，代理把目标写进 dial 帧经 mesh 路由到 --exit 出口节点，由出口节点出站拨号
（出口的 --dial-allow / --dial-allow-cidr 策略把关可达目标）。

安全边界（对齐 mesh 网关 loopback-only + token 认证）：
  - 监听默认 127.0.0.1（裸 :port 归一）；LAN 暴露需显式监听地址。
  - --socks-user/--socks-pass 配置后要求 RFC 1929 认证（配置了才要求）。
  - SSRF 边界在出口节点 dial 策略：内网/loopback 目标默认拒绝（除非出口宣告该服务）。

使用示例:
  sclient socks -l :1080 --exit node-svc`,
		RunE: func(cmd *cobra.Command, args []string) error {
			listenAddr, _ := cmd.Flags().GetString("listen")
			socksUser, _ := cmd.Flags().GetString("socks-user")
			socksPass, _ := cmd.Flags().GetString("socks-pass")

			// mesh 连接参数组统一装配（flag + 配置回落 + 互斥/fail-closed 校验）。
			conn := &meshconn.Conn{}
			if err := conn.FromFlags(cmd, cfgSvc); err != nil {
				return err
			}
			// socks 语义：--exit 必填（明确指定出口节点，防未授权代理）；
			// --exit-auto 也接受（自动选出口），但两者皆无时拒绝。
			if conn.ExitNode == "" && !conn.ExitAuto {
				return fmt.Errorf("--exit 必填：指定出口节点（该节点需 --dial-allow 并放行目标）")
			}

			// 应用 STUN/TURN 全局配置（webrtc 打洞候选收集用）。
			if err := applyTURNRESTFlags(cmd); err != nil {
				return err
			}
			webrtcSetSTUNImpl(conn)

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "socks")
			// svc best-effort（取 access_key_secret / hub_url / node_id 回落；mDNS 无
			// hub 场景可无 svc）。
			svc, svcErr := factory.NewClient(cmd)
			if svcErr != nil {
				svc = nil
			}
			// 配置回落：hub/node-id/mdns-secret 需 svc（对齐既有 T6b 模式）。
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
				conn.MDNSSecret = svc.AccessKeySecret() // 复用 AK/SK 的 SK 作 mDNS 密钥
			}

			// mDNS 直连信令（hub-less）：浏览发现出口节点信令端点。
			var mdnsSrv *mesh.MDNSServer
			if conn.MDNS {
				ms, merr := mesh.NewMDNS(mesh.MDNSConfig{NodeID: conn.NodeID, BrowseOnly: true, Secret: conn.MDNSSecret})
				if merr != nil {
					return fmt.Errorf("mDNS 初始化失败: %w", merr)
				}
				if merr := ms.Start(cmd.Context()); merr != nil {
					return fmt.Errorf("mDNS 启动失败: %w", merr)
				}
				defer ms.Close()
				mdnsSrv = ms
			}

			// hub 模式信令器（webrtc 打洞；注册失败回落中继）。
			caFile, _ := cmd.Flags().GetString("ca-file")
			if caFile == "" {
				if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
					caFile = cfg.XferCAFile
				}
			}
			signaler, closeSig, sigErr := conn.Signalers(cmd.Context(), svc, caFile)
			if sigErr != nil {
				// 注册失败回落中继（signaler=nil → mesh.Dial 回落 relay-only），
				// 对齐 pre-diff socks 语义：打印诊断后继续，不终止命令。
				ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", sigErr)
			}
			if closeSig != nil {
				defer func() { _ = closeSig() }()
			}

			// CONNECT 目标经 mesh 路由到出口节点：目标写 dial 帧，出口按策略出站拨号。
			// AutoDial 收敛全部出口装配（gateway/smart/mdns/webrtc + 本地直连优先）。
			dial := conn.AutoDial(cmd.Context(), svc, signaler, conn.NodeID, mdnsSrv, logger)

			var auth func(user, pass string) bool
			if socksUser != "" || socksPass != "" {
				// 任一凭据配置即要求认证（防只配密码被静默禁用）。恒时比较对齐网关。
				auth = func(u, p string) bool {
					return subtle.ConstantTimeCompare([]byte(u), []byte(socksUser)) == 1 &&
						subtle.ConstantTimeCompare([]byte(p), []byte(socksPass)) == 1
				}
			}
			ss := socks5.New(socks5.Config{Dial: socks5.DialFunc(dial), Auth: auth, Logger: logger})

			listenAddr = iostream.NormalizeListenAddr(listenAddr)
			ln, lerr := net.Listen("tcp", listenAddr)
			if lerr != nil {
				return fmt.Errorf("监听 SOCKS5 端口失败: %w", lerr)
			}
			defer ln.Close()
			exitDesc := conn.ExitNode
			if conn.ExitAuto {
				exitDesc = "auto"
			}
			ios.WriteOutLine("SOCKS5 代理就绪: %s ⇄ mesh 出口 %s（Ctrl+C 退出）", ln.Addr().String(), exitDesc)
			return ss.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringP("listen", "l", "127.0.0.1:1080", "SOCKS5 监听地址（裸 :port 归一 127.0.0.1:port，loopback 安全默认；LAN 暴露需显式监听通配地址）")
	cmd.Flags().String("socks-user", "", "SOCKS5 RFC 1929 认证用户名（配置后要求认证，防未授权使用代理）")
	cmd.Flags().String("socks-pass", "", "SOCKS5 RFC 1929 认证密码（配 --socks-user 使用）")
	// mesh 连接参数组（hub/node-id/webrtc/insecure/stun/turn/gateway/smart/mdns）
	meshconn.AddFlags(cmd)
	// 出口路由 flag 族（--exit/--exit-auto/--exit-only/--exit-exclude/--local-timeout）
	meshconn.AddExitFlags(cmd)
	addTURNRESTFlags(cmd)
	return cmd
}

// webrtcSetSTUNImpl 应用 STUN/TURN 全局配置。
func webrtcSetSTUNImpl(conn *meshconn.Conn) {
	if conn.STUN != nil {
		webrtc.SetSTUNServers(conn.STUN)
	}
	if conn.TURN != nil {
		webrtc.SetTURNServers(conn.TURN)
	}
	if conn.TURNUser != "" || conn.TURNPass != "" {
		webrtc.SetTURNCredential(conn.TURNUser, conn.TURNPass)
	}
}
