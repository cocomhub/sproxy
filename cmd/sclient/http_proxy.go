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
	"github.com/cocomhub/sproxy/pkg/httpproxy"
	"github.com/cocomhub/sproxy/pkg/iostream"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/spf13/cobra"
)

// newCmdHTTPProxy 创建 sclient http-proxy：启动本地正向 HTTP 代理，
// 绝对 URI + CONNECT 经 mesh 到出口节点（本地直连优先，失败回退出口）出站拨号。
//
// 安全边界（对齐 socks）：
//   - 监听默认 loopback-only（NormalizeListenAddr，裸 :port → 127.0.0.1:port）；
//   - --proxy-user/--proxy-pass 任一配置即要求 Proxy-Authorization Basic（防只配密码被静默禁用）；
//   - 出口路径目标由出口节点 dial 策略把关（NewServiceDialPolicy，防 SSRF）。
func newCmdHTTPProxy(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "http-proxy [-l :port] [--exit <node>|--exit-auto] [--proxy-user u] [--proxy-pass p]",
		Short: "正向 HTTP 代理（绝对 URI + CONNECT，本地直连优先/经出口出站）",
		Long: `启动本地正向 HTTP 代理：客户端（curl/wget/浏览器/Git/Go·Python·Node 应用）配
http_proxy / https_proxy / no_proxy 环境变量即用。HTTP 请求以绝对 URI 发往代理转发；
HTTPS 走 CONNECT 隧道（端到端 TLS，代理不可见明文）。

路由（本地直连优先）：网络良好时本地直连目标（零 mesh 开销）；本地失败/超时（被墙/网络差）
自动回退出口：--exit <node> 固定出口，--exit-auto 自动选（outbound-dial 能力优先 +
--exit-exclude 排除名单）。--exit-only 强制恒经出口；无 --exit/--exit-auto 时恒本地直连。

安全边界（对齐 mesh 网关 loopback-only + 认证）：
  - 监听默认 127.0.0.1（裸 :port 归一）；LAN 暴露需显式监听地址。
  - --proxy-user/--proxy-pass 配置后要求 Proxy-Authorization Basic（未认证回 407）。
  - SSRF 边界在出口节点拨号策略：经出口路径目标由 NewServiceDialPolicy 把关。

使用示例:
  sclient http-proxy -l :1080 --exit node-svc
  sclient http-proxy -l :1080 --exit-auto --exit-exclude node-a,node-b`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 装配 mesh 连接参数组（flag + 配置回落 + 互斥校验）。
			conn := &meshconn.Conn{}
			if err := conn.FromFlags(cmd, cfgSvc); err != nil {
				return err
			}
			listenAddr, _ := cmd.Flags().GetString("listen")
			proxyUser, _ := cmd.Flags().GetString("proxy-user")
			proxyPass, _ := cmd.Flags().GetString("proxy-pass")

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("cmd", "http-proxy")

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
				conn.MDNSSecret = svc.AccessKeySecret()
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

			// 最终拨号：本地直连优先（网络好零 mesh 开销）→ 回退出口（--exit 或 --exit-auto）。
			// fail-closed：出口拨号错误向上传播，--exit-only 恒经出口（AutoDial 内部保证）。
			dial := conn.AutoDial(cmd.Context(), svc, signaler, conn.NodeID, mdnsSrv, logger)

			var auth func(u, p string) bool
			if proxyUser != "" || proxyPass != "" {
				auth = func(u, p string) bool {
					return subtle.ConstantTimeCompare([]byte(u), []byte(proxyUser)) == 1 &&
						subtle.ConstantTimeCompare([]byte(p), []byte(proxyPass)) == 1
				}
			}
			ss := httpproxy.New(httpproxy.Config{Dial: httpproxy.DialFunc(dial), Auth: auth, Logger: logger})

			listenAddr = meshconn.NormalizeListen(listenAddr)
			ln, lerr := net.Listen("tcp", listenAddr)
			if lerr != nil {
				return fmt.Errorf("监听 HTTP 代理端口失败: %w", lerr)
			}
			defer ln.Close()
			ios.WriteOutLine("HTTP 代理就绪: %s（本地直连优先 ⇄ 出口 %s）（Ctrl+C 退出）", ln.Addr().String(), exitLabel(conn))
			return ss.Serve(cmd.Context(), ln)
		},
	}
	cmd.Flags().StringP("listen", "l", "127.0.0.1:1080", "HTTP 代理监听地址（裸 :port 归一 127.0.0.1:port，loopback 安全默认；LAN 暴露需显式监听通配地址）")
	cmd.Flags().String("proxy-user", "", "Proxy-Authorization Basic 用户名（配置后要求认证，防未授权使用代理）")
	cmd.Flags().String("proxy-pass", "", "Proxy-Authorization Basic 密码（配 --proxy-user 使用）")
	// mesh 连接参数组（hub/node-id/webrtc/insecure/stun/turn/gateway/smart/mdns）
	meshconn.AddFlags(cmd)
	// 出口路由 flag 族（--exit/--exit-auto/--exit-only/--exit-exclude/--local-timeout）
	meshconn.AddExitFlags(cmd)
	addTURNRESTFlags(cmd)
	return cmd
}

// exitLabel 生成横幅中的出口描述。
func exitLabel(conn *meshconn.Conn) string {
	switch {
	case conn.ExitNode != "":
		return conn.ExitNode
	case conn.ExitAuto:
		return "auto"
	default:
		return "本地直连"
	}
}
