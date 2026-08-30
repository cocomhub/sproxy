// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/socks5"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
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
			exit, _ := cmd.Flags().GetString("exit")
			gatewayAddr, _ := cmd.Flags().GetString("gateway")
			mdns, _ := cmd.Flags().GetBool("mdns")
			mdnsSecret, _ := cmd.Flags().GetString("mdns-secret")
			socksUser, _ := cmd.Flags().GetString("socks-user")
			socksPass, _ := cmd.Flags().GetString("socks-pass")
			useWebRTC, _ := cmd.Flags().GetBool("webrtc")
			hubURL, _ := cmd.Flags().GetString("hub")
			nodeID, _ := cmd.Flags().GetString("node-id")
			insecure, _ := cmd.Flags().GetBool("insecure")
			if exit == "" {
				return fmt.Errorf("--exit 必填：指定出口节点（该节点需 --dial-allow 并放行目标）")
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

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "socks")
			// svc best-effort（取 access_key_secret / hub_url / node_id 回落；mDNS 无
			// hub 场景可无 svc）。
			svc, svcErr := factory.NewClient(cmd)
			if svcErr != nil {
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
				mdnsSecret = svc.AccessKeySecret() // 复用 AK/SK 的 SK 作 mDNS 密钥
			}

			// mDNS 直连信令（hub-less）：浏览发现出口节点信令端点。
			var mdnsSrv *mesh.MDNSServer
			if mdns {
				ms, merr := mesh.NewMDNS(mesh.MDNSConfig{NodeID: nodeID, BrowseOnly: true, Secret: mdnsSecret})
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
			var signaler *hub.HubSignaler
			if !mdns && svc != nil && useWebRTC {
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
					ios.WriteErrLine("webrtc 信令注册失败: %v（回落 hub 中继）", regErr)
				} else {
					signaler = r.Signaler
					defer func() { _ = r.Closer() }()
				}
			}

			// CONNECT 目标经 mesh 路由到出口节点：目标写 dial 帧，出口按策略出站拨号。
			dial := func(ctx context.Context, addr string) (net.Conn, error) {
				target := &client.MeshService{Name: "socks", Node: exit, Addr: addr}
				if gatewayAddr != "" && svc != nil {
					conn, gerr := mesh.GatewayConnect(ctx, gatewayAddr, exit, addr, svc.AccessKeySecret())
					if gerr == nil {
						return conn, nil
					}
					if !errors.Is(gerr, mesh.ErrNoPeerLink) {
						ios.WriteErrLine("本地网关路由失败: %v（回落常规拨号）", gerr)
					}
				}
				if mdns && mdnsSrv != nil {
					peer, perr := mdnsSrv.LookupPeer(ctx, exit, mdnsLookupTimeout)
					if perr != nil {
						return nil, fmt.Errorf("mDNS 未发现出口节点 %s: %w", exit, perr)
					}
					if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
						return nil, verr
					}
					sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, nodeID)
					if serr != nil {
						return nil, serr
					}
					sig.SetSecret(mdnsSecret)
					res, derr := mesh.DialDirect(ctx, sig, target)
					_ = sig.Close()
					if derr != nil {
						return nil, derr
					}
					return res.Conn, nil
				}
				if svc == nil {
					return nil, fmt.Errorf("无可用 mesh 路由（需 --mdns 或可用的 hub 配置）")
				}
				// signaler 为 nil（--webrtc=false / 注册失败）时 mesh.Dial 回落 relay-only。
				res, derr := mesh.Dial(ctx, svc, signaler, target, nodeID)
				if derr != nil {
					return nil, derr
				}
				return res.Conn, nil
			}

			var auth func(user, pass string) bool
			if socksUser != "" || socksPass != "" {
				// 任一凭据配置即要求认证（防只配密码被静默禁用）。恒时比较对齐网关。
				auth = func(u, p string) bool {
					return subtle.ConstantTimeCompare([]byte(u), []byte(socksUser)) == 1 &&
						subtle.ConstantTimeCompare([]byte(p), []byte(socksPass)) == 1
				}
			}
			ss := socks5.New(socks5.Config{Dial: dial, Auth: auth, Logger: logger})

			listenAddr = iostream.NormalizeListenAddr(listenAddr)
			ln, lerr := net.Listen("tcp", listenAddr)
			if lerr != nil {
				return fmt.Errorf("监听 SOCKS5 端口失败: %w", lerr)
			}
			defer ln.Close()
			ios.WriteOutLine("SOCKS5 代理就绪: %s ⇄ mesh 出口 %s（Ctrl+C 退出）", ln.Addr().String(), exit)
			return ss.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringP("listen", "l", "127.0.0.1:1080", "SOCKS5 监听地址（裸 :port 归一 127.0.0.1:port，loopback 安全默认；LAN 暴露需显式监听通配地址）")
	cmd.Flags().String("exit", "", "出口节点 node-id（必填；该节点需 --dial-allow 并放行 CONNECT 目标）")
	cmd.Flags().String("gateway", "", "经本地 mesh node 网关复用已建立直连链路路由（127.0.0.1:port；无已建链路回落常规拨号）")
	cmd.Flags().Bool("mdns", false, "纯 mDNS 直连（不经 hub）：经 mDNS 发现出口节点信令端点")
	cmd.Flags().String("mdns-secret", "", "mDNS 模式共享密钥（与出口节点 mesh node --mdns-secret 一致；为空回落 access_key_secret，再空 = LAN 信任）")
	cmd.Flags().String("socks-user", "", "SOCKS5 RFC 1929 认证用户名（配置后要求认证，防未授权使用代理）")
	cmd.Flags().String("socks-pass", "", "SOCKS5 RFC 1929 认证密码（配 --socks-user 使用）")
	cmd.Flags().Bool("webrtc", true, "优先 webrtc 打洞直连，失败回落 hub 中继")
	cmd.Flags().String("hub", "", "hub 地址（http(s)/ws(s)；默认取配置 hub_url，再回落 server_url）")
	cmd.Flags().String("node-id", "", "本节点 ID（信令来源；默认主机名）")
	cmd.Flags().Bool("insecure", false, "跳过 TLS 证书验证（自签 wss hub）")
	cmd.Flags().StringSlice("stun", nil, "STUN 服务器地址（可重复/逗号分隔）")
	cmd.Flags().StringSlice("turn", nil, "TURN 服务器地址（可重复/逗号分隔）")
	cmd.Flags().String("turn-user", "", "TURN 用户名")
	cmd.Flags().String("turn-pass", "", "TURN 密码")
	return cmd
}
