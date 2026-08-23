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
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

// newCmdMeshNode 创建 mesh node：单进程常驻 mesh 节点（注册 + 服务宣告 + 中继 +
// webrtc 直连 + 自动重连），mesh connect 可直连优先/中继回落到达它。
//
// 依赖：hub 已启用中继（hub.enabled=true + relay_token）。--dial-allow 必须开启
// （mesh connect 恒发 dial 帧，出口拨号依赖它；关闭时只剩 HTTP 中继到 --local）。
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
			_, _ = cmd.Flags().GetString("token") // --token 保留（relay_token 回落）
			relayToken, _ := cmd.Flags().GetString("relay-token")
			services, _ := cmd.Flags().GetStringArray("service")
			dialAllow, _ := cmd.Flags().GetBool("dial-allow")
			dialAllowCIDRs, _ := cmd.Flags().GetStringArray("dial-allow-cidr")
			localAddr, _ := cmd.Flags().GetString("local")
			enableWebRTC, _ := cmd.Flags().GetBool("webrtc")
			discover, _ := cmd.Flags().GetBool("discover")
			discoverInterval, _ := cmd.Flags().GetDuration("discover-interval")
			gatewayAddr, _ := cmd.Flags().GetString("gateway-addr")
			stunServers, _ := cmd.Flags().GetStringSlice("stun")
			insecure, _ := cmd.Flags().GetBool("insecure")
			if stunServers != nil {
				webrtc.SetSTUNServers(stunServers)
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
			if hubURL == "" {
				hubURL = cfg.HubURL
			}
			if hubURL == "" {
				hubURL = cfg.ServerURL
			}
			// 语义对齐 relay start：--token 是 relay_token（hub 注册）；SproxySig 认证
			// AccessKey/SK 从根 --access-key/--access-key-secret 或配置派生（信令/节点
			// 列表/网关均走签名，token 不上线）。
			accessKeyFlag, _ := cmd.Flags().GetString("access-key")
			accessKeySecretFlag, _ := cmd.Flags().GetString("access-key-secret")
			accessKey := client.MeshAccessKey(accessKeyFlag, cfg.AccessKey)
			accessKeySecret := client.MeshAccessKeySecret(accessKeySecretFlag, cfg.AccessKeySecret)
			relayTok := client.MeshRelayToken(relayToken, cfg.RelayToken, "", "")
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
				RelayToken:        relayTok,
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
				Logger:            logger,
			})
		},
	}
	cmd.Flags().String("hub", "", "hub 地址（http(s)/ws(s)，可带 /ws；默认取配置 hub_url，再回落 server_url）")
	cmd.Flags().String("node-id", "", "本节点稳定 ID（mesh connect 用它寻址；默认取配置 node_id，再回落主机名）")
	cmd.Flags().String("token", "", "hub 中继注册 token（relay_token；与 relay start --token 一致；信令 Bearer 用根 --auth-token / 配置 auth_token）")
	cmd.Flags().String("relay-token", "", "hub 中继注册 token（优先于 --token；默认复用 --token / 配置 relay_token）")
	cmd.Flags().StringArray("service", nil, "宣告一个 mesh 服务（格式 name:addr，可重复；mesh connect 可发现）")
	cmd.Flags().Bool("dial-allow", false, "允许出口拨号（mesh connect 恒发 dial 帧，依赖此开关；关闭时只剩 HTTP 中继到 --local）")
	cmd.Flags().StringArray("dial-allow-cidr", nil, "出口拨号白名单网段（如 192.168.0.0/16；配合 --dial-allow 放行内网，默认仅公网+宣告地址）")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址（HTTP 中继转发目标）")
	cmd.Flags().Bool("webrtc", true, "接受 WebRTC 直连（信令 poll + listen）；关闭则仅经 hub 中继可达")
	cmd.Flags().Bool("discover", true, "启用自动对等发现（经 hub 节点列表发现其他 mesh node，并行 webrtc 自动直连并保持，形成 full-mesh 拓扑）")
	cmd.Flags().Duration("discover-interval", 10*time.Second, "对等发现周期（如 1s 测试 / 30s 生产）")
	cmd.Flags().String("gateway-addr", mesh.GatewayDefaultAddr, "本地网关监听地址（mesh connect --gateway 复用已建直连链路的入口；仅 loopback，安全默认；同机多节点用 127.0.0.1:0 随机端口）")
	cmd.Flags().StringSlice("stun", nil,
		"STUN 服务器地址（可重复/逗号分隔，如 stun:stun.qq.com:3478）；默认 Google+腾讯+小米混合，全不通时请指定本地可达服务器")
	return cmd
}
