// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// newCmdMeshNode 创建 mesh node：单进程常驻 mesh 节点（注册 + 服务宣告 + 中继 +
// webrtc 直连 + 自动重连），mesh connect 可直连优先/中继回落到达它。
//
// 依赖：hub 已启用中继（hub.enabled=true + access_keys 配置，注册走 SproxySig
// AccessKey + HMAC proof 准入）。--dial-allow 必须开启（mesh connect 恒发 dial 帧，
// 出口拨号依赖它；关闭时只剩 HTTP 中继到 --local）。
func newCmdMeshNode(ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "node",
		Short: "运行常驻 mesh 节点（注册+中继+webrtc 直连+自动重连）",
		Long: `作为常驻 mesh 节点连接 Hub：单进程单注册（稳定 node-id + 服务宣告 +
per-node secret），并行提供经 hub 的中继服务与 WebRTC 直连，mesh connect 可
直连优先/中继回落到达本节点。断线自动指数退避重连。

使用示例:
  sclient mesh node --hub wss://hub.example.com/ws --node-id nodeA \
    --service ssh:127.0.0.1:22 --dial-allow`,
		RunE: func(cmd *cobra.Command, args []string) error {
			hubURL, _ := cmd.Flags().GetString("hub")
			nodeID, _ := cmd.Flags().GetString("node-id")
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllow, _ := cmd.Flags().GetBool("dial-allow")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			localAddr, _ := cmd.Flags().GetString("local")
			enableWebRTC, _ := cmd.Flags().GetBool("webrtc")
			discover, _ := cmd.Flags().GetBool("discover")
			discoverInterval, _ := cmd.Flags().GetDuration("discover-interval")
			gatewayAddr, _ := cmd.Flags().GetString("gateway-addr")
			mdns, _ := cmd.Flags().GetBool("mdns")
			mdnsSecret, _ := cmd.Flags().GetString("mdns-secret")
			signalAddr, _ := cmd.Flags().GetString("signal-addr")
			socksAddr, _ := cmd.Flags().GetString("socks")
			socksUser, _ := cmd.Flags().GetString("socks-user")
			socksPass, _ := cmd.Flags().GetString("socks-pass")
			stunServers, _ := cmd.Flags().GetStringSlice("stun")
			insecure, _ := cmd.Flags().GetBool("insecure")
			virtualSubnet, _ := cmd.Flags().GetString("virtual-subnet")
			vipAllowPorts, _ := cmd.Flags().GetIntSlice("vip-allow-port")
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

			// P2-配置3：通用参数配置回落（CLI > 配置文件 > 默认）。常驻节点不需要
			// FileClient（避免 tunnel_key/InitError 拖垮），直接经 cfgSvc 回落。
			var cfg *client.Config
			if cfgSvc != nil {
				cfg, _ = cfgSvc.LoadConfig()
			}
			if cfg == nil {
				cfg = client.DefaultConfig()
			}
			if !mdns { // 纯 mDNS 模式不解析 hub 配置（不经 hub）
				if hubURL == "" {
					hubURL = cfg.HubURL
				}
				if hubURL == "" {
					hubURL = cfg.ServerURL
				}
			}
			// SproxySig 认证 AccessKey/SK 从根 --access-key/--access-key-secret 或配置派生
			// （信令/节点列表/网关/hub 注册准入均走签名，Secret 永不上线）。hub 注册
			// 准入由 AutoRegister 用 SK 计算 HMAC proof（绑定 nodeID），无需共享 token。
			accessKeyFlag, _ := cmd.Flags().GetString("access-key")
			accessKeySecretFlag, _ := cmd.Flags().GetString("access-key-secret")
			accessKey := client.MeshAccessKey(accessKeyFlag, cfg.AccessKey)
			accessKeySecret := client.MeshAccessKeySecret(accessKeySecretFlag, cfg.AccessKeySecret)
			if nodeID == "" {
				nodeID = cfg.NodeID
			}

			logger := slog.New(slog.NewTextHandler(ios.ErrOut, nil)).With("node", nodeID)
			svcs, addrs := mesh.ParseServiceDecls(services, logger)
			if len(svcs) == 0 && len(services) > 0 {
				logger.Warn("全部服务宣告无效，节点不宣告任何服务")
			}
			var tags []string
			if dialAllow {
				tags = append(tags, "exit")
			}

			// 常驻进程：SIGINT/SIGTERM 优雅摘除节点（RunNode 收到 ctx 取消即 closeReg）。
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return mesh.RunNode(ctx, mesh.NodeConfig{
				HubURL:            hubURL,
				NodeID:            nodeID,
				AccessKey:         accessKey,
				AccessKeySecret:   accessKeySecret,
				Services:          svcs,
				ServiceAddrs:      addrs,
				Tags:              tags,
				DialAllow:         dialAllow,
				DialAllowCIDRs:    dialAllowCIDRs,
				LocalAddr:         localAddr,
				Insecure:          insecure,
				EnableWebRTC:      enableWebRTC,
				Discover:          discover,
				DiscoveryInterval: discoverInterval,
				GatewayAddr:       gatewayAddr,
				EnableMDNS:        mdns,
				MDNSOnly:          mdns,
				SignalAddr:        signalAddr,
				MDNSPeerSecret:    mdnsSecret,
				SocksAddr:         socksAddr,
				SocksUser:         socksUser,
				SocksPass:         socksPass,
				VirtualSubnet:     virtualSubnet,
				VIPAllowPorts:     vipAllowPorts,
				Logger:            logger,
			})
		},
	}
	cmd.Flags().String("hub", "", "hub 地址（http(s)/ws(s)，可带 /ws；默认取配置 hub_url，再回落 server_url）")
	cmd.Flags().String("node-id", "", "本节点稳定 ID（mesh connect 用它寻址；默认取配置 node_id，再回落主机名）")
	cmd.Flags().StringArray("service", nil, "宣告一个 mesh 服务（格式 name:addr，可重复；mesh connect 可发现）")
	cmd.Flags().Bool("dial-allow", false, "允许出口拨号（mesh connect 恒发 dial 帧，依赖此开关；关闭时只剩 HTTP 中继到 --local）")
	cmd.Flags().StringArray("dial-allow-cidr", nil, "出口拨号白名单网段（如 192.168.0.0/16；配合 --dial-allow 放行内网，默认仅公网+宣告地址）")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址（HTTP 中继转发目标）")
	cmd.Flags().Bool("webrtc", true, "接受 WebRTC 直连（信令 poll + listen）；关闭则仅经 hub 中继可达")
	cmd.Flags().Bool("discover", true, "启用自动对等发现（经 hub 节点列表发现其他 mesh node，并行 webrtc 自动直连并保持，形成 full-mesh 拓扑）")
	cmd.Flags().Duration("discover-interval", 10*time.Second, "对等发现周期（如 1s 测试 / 30s 生产）")
	cmd.Flags().String("gateway-addr", mesh.GatewayDefaultAddr, "本地网关监听地址（mesh connect --gateway 复用已建直连链路的入口；仅 loopback，安全默认；同机多节点用 127.0.0.1:0 随机端口）")
	cmd.Flags().Bool("mdns", false, "纯 mDNS 局域网模式（不经 hub）：广播本节点（node-id + 服务 + 直连信令端点）并经 mDNS 发现同网段节点自动直连。两节点同网段运行 `mesh node --mdns` 即可互发现并 mesh connect 直连。注意：需监听局域网接口（直连信令 + mDNS 组播），Windows 首次运行会弹防火墙授权")
	cmd.Flags().String("signal-addr", "", "直连 webrtc 信令监听地址（--mdns 用；空默认绑定主局域网 IP 的随机端口；可显式指定如 127.0.0.1:0 收敛 loopback 开发/测试避免防火墙弹窗，但仅本机可达）")
	cmd.Flags().String("mdns-secret", "", "mDNS 模式共享密钥（直连信令 offer 与 mDNS TXT 均 HMAC 签名校验，防未授权 peer 借本节点作中继/出口、防广告伪造；同 mesh 所有节点须一致；为空 = 无认证 LAN 信任，出口由 dial-allow 策略约束）")
	cmd.Flags().String("socks", "", "本地 SOCKS5 出口监听地址（本节点为出口，CONNECT 目标本机拨号；裸 :port 归一 127.0.0.1:port，loopback 安全默认；远程 peer 可 mesh connect socks -l :port 隧道到它。注意：不配 --socks-user/--socks-pass 时，对可触及者即开放本机内网出口，请显式开启认证或保持 loopback）")
	cmd.Flags().String("virtual-subnet", hub.DefaultVirtualSubnet, "虚拟 IP 子网（CIDR，仅 IPv4；默认 CGNAT 100.64.0.0/10；mDNS 无 hub 模式本地确定性分配用，有 hub 时以 hub 分配为准）")
	cmd.Flags().IntSlice("vip-allow-port", nil, "虚拟 IP 开放的额外端口白名单（可重复；缺省 = --service 宣告端口自动开放，此处额外开放未宣告的本机端口）")
	cmd.Flags().String("socks-user", "", "SOCKS5 RFC 1929 认证用户名（配 --socks 使用；配置后要求认证，防未授权使用本节点作代理）")
	cmd.Flags().String("socks-pass", "", "SOCKS5 RFC 1929 认证密码（配 --socks/--socks-user 使用）")
	cmd.Flags().StringSlice("stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
	cmd.Flags().StringSlice("turn", nil,
		"TURN 中继服务器地址（可重复/逗号分隔，如 turn:relay.example.com:3478）；需配合 --turn-user/--turn-pass，提升对称 NAT 下打洞成功率")
	cmd.Flags().String("turn-user", "", "TURN 用户名（静态密码模式，配 --turn/--turn-pass 使用）")
	cmd.Flags().String("turn-pass", "", "TURN 密码（静态密码模式，配 --turn/--turn-user 使用）")
	return cmd
}
