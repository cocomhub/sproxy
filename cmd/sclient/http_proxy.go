// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/meshconn"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/httpproxy"
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

			// 出口拨号闭包（经 mesh 到出口节点；--exit-auto 时按 nodeID 自动选择）。
			// 首期实现：svc 构造 + 信令装配复用 socks 骨架（任务 5 迁移到 meshconn）。
			exitDial := buildExitDial(cmd, conn, cfgSvc, logger)

			var auth func(u, p string) bool
			if proxyUser != "" || proxyPass != "" {
				auth = func(u, p string) bool {
					return subtle.ConstantTimeCompare([]byte(u), []byte(proxyUser)) == 1 &&
						subtle.ConstantTimeCompare([]byte(p), []byte(proxyPass)) == 1
				}
			}
			dial := httpproxy.DialFunc(conn.LocalOrExit(exitDial))
			ss := httpproxy.New(httpproxy.Config{Dial: dial, Auth: auth, Logger: logger})

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
	meshconn.AddFlags(cmd)
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

// buildExitDial 构造经出口的拨号闭包（首期复用 socks 骨架逻辑，任务 5 收敛）。
// 注：--exit-auto 时按 nodeID 选择出口（NewAutoExitDial 内部），固定 --exit 时直接经该节点。
func buildExitDial(cmd *cobra.Command, conn *meshconn.Conn, cfgSvc ConfigProvider, logger *slog.Logger) meshconn.DialFunc {
	// 无出口场景：nil 回退本地直连。
	if conn.ExitNode == "" && !conn.ExitAuto {
		return nil
	}
	// 占位：真实实现复用 socks.go 的 svc/signaler/mDNS/AutoRegister 装配
	// （任务 5 迁移到 meshconn.Signalers + meshconn.Target + meshconn.Dial）。
	return func(ctx context.Context, addr string) (net.Conn, error) {
		return nil, fmt.Errorf("出口拨号装配待任务 5 完成")
	}
}
