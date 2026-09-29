// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/credrotate"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"  // 注册内置 TCP 传输层（--transport tcp）
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/grpc" // 注册 gRPC 传输层（--transport grpc）
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic" // 注册 QUIC 传输层（--transport quic）
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"   // 注册 WebSocket 传输层（--transport ws）
	"github.com/spf13/cobra"
)

const (
	reconnectBaseDelay = 1 * time.Second
	reconnectMaxDelay  = 30 * time.Second
	// registerAckTimeout 是等待 hub 注册 ACK 的超时。
	registerAckTimeout = 10 * time.Second
)

// applyWSPath 把自定义 WS 路径应用到 hub URL（roadmap §5.3 P1 被动伪装层）。
// hubURL 为 ws(s):// 且未带路径（或仅根 /）时替换路径；已带显式路径（用户 --hub
// ws://host:port/custom）优先保留。非 ws:// URL 或解析失败原样返回（零回归）。
func applyWSPath(hubURL, wsPath string) string {
	if hubURL == "" || wsPath == "" || wsPath == "/ws" {
		return hubURL
	}
	u, err := url.Parse(hubURL)
	if err != nil {
		return hubURL
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || (u.Path != "" && u.Path != "/") {
		return hubURL
	}
	if !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}
	u.Path = wsPath
	return u.String()
}

// NewCmdRelay 创建 relay 父命令的工厂函数。
func runRelayStart(cmd *cobra.Command, transport, hubURL, local, nodeID, accessKey, accessKeySecret, accessKeyID string, insecure bool, caFile string, dialAllow bool, services, dialAllowCIDRs []string, creds *credrotate.Credentials) error {
	switch transport {
	case "ws", "tcp", "quic":
	default:
		return fmt.Errorf("未知传输层 %q（仅支持 ws/tcp/quic）", transport)
	}
	if nodeID == "" {
		nodeID = fmt.Sprintf("relay-%d", time.Now().UnixMilli())
	}
	// 本地默认 hub（--hub 与配置 hub_url 均未提供时）。注意与 sproxy 默认监听端口
	// :18083 不同——请按实际 hub 地址显式 --hub 或配置 hub_url。
	// ws 传输用 ws(s):// URL；tcp/quic 传输用裸 host:port（hub.transports.tcp/quic.listen）。
	if hubURL == "" {
		switch transport {
		case "tcp":
			hubURL = "127.0.0.1:18084"
		case "quic":
			hubURL = "127.0.0.1:18088"
		default:
			hubURL = "ws://127.0.0.1:18084/ws"
		}
	}
	// 被动伪装层（roadmap §5.3 P1）：--ws-path 覆盖默认 WS 路径（仅 ws 传输且
	// --hub 未显式带路径时生效；--hub 带显式路径（ws://host:port/custom）优先）。
	if transport == "ws" {
		if wsPath, _ := cmd.Flags().GetString("ws-path"); wsPath != "" && wsPath != "/ws" {
			hubURL = applyWSPath(hubURL, wsPath)
		}
	}

	logger := slog.With("node", nodeID, "hub", hubURL, "local", local, "dial_allow", dialAllow, "transport", transport)
	logger.Info("中继节点启动")
	// hub 注册准入已改 SproxySig AccessKey + HMAC proof：Secret 只本端计算签名/证明，
	// 永不上线，故明文 ws:// 不再泄露凭据；仍提示自签证书场景用 wss://。
	if insecure && transport != "tcp" {
		logger.Warn("--insecure 已启用，跳过 TLS 证书验证；仅限开发/测试", "hub", hubURL)
	}

	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()

	// 虚拟 IP 子网：--virtual-subnet 覆盖默认 CGNAT（S-1 审查修复，匹配自定义 hub 子网）。
	virtualSubnet, _ := cmd.Flags().GetString(flagVirtualSubnet)
	wsUpgradeHeader, _ := cmd.Flags().GetString("ws-upgrade-header")
	return runRelayWithRetry(ctx, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID, insecure, caFile, wsUpgradeHeader, dialAllow, services, dialAllowCIDRs, virtualSubnet, logger, creds)
}

func runRelayWithRetry(ctx context.Context, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID string, insecure bool, caFile, wsUpgradeHeader string, dialAllow bool, services, dialAllowCIDRs []string, virtualSubnet string, logger *slog.Logger, creds *credrotate.Credentials) error {
	delay := reconnectBaseDelay
	for {
		// 动态凭据：credrotate 轮换后每次重连取最新 SK（无需重启）。
		cAK, cSK, cID := accessKey, accessKeySecret, accessKeyID
		if creds != nil {
			cAK, cSK, cID = creds.Get()
		}
		err := runRelayOnce(ctx, transport, nodeID, hubURL, local, cAK, cSK, cID, insecure, caFile, wsUpgradeHeader, dialAllow, services, dialAllowCIDRs, virtualSubnet, logger)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if isTerminalRelayError(err) {
			return err
		}
		logger.Warn("中继断开，即将重连", "delay", delay, "error", err)
		select {
		case <-time.After(delay):
			delay *= 2
			if delay > reconnectMaxDelay {
				delay = reconnectMaxDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// errRelayRegistrationRejected 表示 hub 通过注册 ACK 明确拒绝本次注册（鉴权/格式错误）。
// isTerminalRelayError 判断是否应因配置/权限错误终止而非重试。
// 仅当 hub 通过注册 ACK **明确拒绝** 注册时才终止；其余（连接断开、超时、EOF、
// ACK 未到达）均视为可重连的网络问题。哨兵错误定义在 pkg/tunnel/hub
// （hub.ErrRegisterRejected），errors.Is 可穿透任意 %w 包装。
func isTerminalRelayError(err error) bool {
	return errors.Is(err, hub.ErrRegisterRejected)
}

func runRelayOnce(ctx context.Context, transport, nodeID, hubURL, local, accessKey, accessKeySecret, accessKeyID string, insecure bool, caFile, wsUpgradeHeader string, dialAllow bool, services, dialAllowCIDRs []string, virtualSubnet string, logger *slog.Logger) error {
	// 注册准入：hub 已废除共享 token，改用 SproxySig AccessKey + HMAC proof。
	// fail-closed：AccessKeySecret 为空时直接报错（防止无凭据注册被 hub fail-closed
	// 拒绝后客户端困惑——明明连上了却被拒）。
	if accessKeySecret == "" {
		return fmt.Errorf("注册失败: access_key_secret 为空，无法计算注册 proof")
	}
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, err := hub.ComputeRegisterProof(accessKeySecret, nodeID, ts, nonce)
	if err != nil {
		return fmt.Errorf("注册失败: 计算注册证明失败: %w", err)
	}
	// 传输层选择：--transport tcp 走裸 TCP（hub.transports.tcp.listen，hubURL 为
	// host:port）；--transport quic 走 QUIC UDP；--transport grpc 走 HTTP/2；默认 ws 走
	// WebSocket。三者注册/信令/数据面协议完全一致，仅 xfer.Conn 载体不同。
	conn, err := relayDialTransport(ctx, transport, hubURL, insecure, caFile, wsUpgradeHeader)
	if err != nil {
		return fmt.Errorf("连接到 Hub 失败: %w", err)
	}
	logger.Info("已连接到 Hub")

	// 注册协议：连接建立后，在 xfer 层直接发送一条注册帧（JSON 或裸 nodeID）。
	// 与 HubServer.readRegisterFrame 对齐：hub 在创建 mux 前通过 conn.Receive 读取，
	// 因此这里也必须用 conn.Send，而非 mux 控制流。
	meta, serviceAddrs := relayBuildMeta(dialAllow, services, logger)
	// 声明 per-node-secret 能力：hub 回 REG_OK:<base64url secret>（B1 已支持，
	// B3 服务端将据此校验信令身份）；声明 virtual-ip 能力：hub 在 REG_OK 携带本节点
	// 虚拟 IP（Discover=false 的 relay 出口节点也能立即得知自身 VIP）。不感知能力的
	// 旧 hub 忽略未知能力位，回旧格式。现有调用不传 caps 时行为不变。
	// dialAllow 时额外声明 outbound-dial：本节点可作为 SmartDial 多跳中间节点
	// （对端据此从 ListHubNodes 发现「可作中转出口」的候选）。
	caps := relayRegisterCaps(dialAllow)
	if serr := conn.Send(ctx, hub.NewRegisterFrame(nodeID, accessKey, proof, ts, nonce, meta, caps...)); serr != nil {
		_ = conn.Close() // P1-15：mux 创建前失败必须关闭 WS，否则重连循环泄漏连接+sendLoop goroutine
		return fmt.Errorf("发送注册帧失败: %w", serr)
	}

	// 等待 hub 注册 ACK（token 错误/格式错误尽早报错，而非等建流失败才发现）
	ackCtx, ackCancel := context.WithTimeout(ctx, registerAckTimeout)
	ack, ackErr := conn.Receive(ackCtx)
	ackCancel()
	if ackErr != nil {
		_ = conn.Close() // P1-15：同守卫
		return fmt.Errorf("等待注册 ACK 失败: %w", ackErr)
	}
	ackFull, ackErr := hub.ParseRegisterAckFull(string(ack))
	if ackErr != nil {
		_ = conn.Close() // P1-15：同守卫
		return ackErr
	}
	nodeSecret := ackFull.Secret
	if nodeSecret != "" {
		// per-node secret 与本次注册连接生命周期绑定（重连即轮换），
		// 只在注册流程内使用，不落盘、不打印值（I1，方案 B）。
		logger.Info("已注册到 Hub（per-node secret 已获取）")
	} else {
		logger.Info("已注册到 Hub")
	}
	selfVIP := ackFull.VirtualIP

	m := mux.New(conn, mux.RoleListener)
	defer m.Close()

	// 本地 HTTP 服务地址（HTTP 中继转发目标）
	localAddr := local
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	httpClient := &http.Client{Timeout: 30 * time.Second, Transport: netutil.DefaultTransport()}

	logger.Info("等待中继请求...")
	// 始终传入包含宣告服务地址的拨号策略（--dial-allow=false 时 Serve 在咨询
	// 策略前就拒绝 dial 帧，策略不生效）。无服务宣告且无 CIDR 时等价默认
	// DialAllowed（仅公网）。
	opts, oerr := relayServeOpts(virtualSubnet, selfVIP, dialAllowCIDRs, serviceAddrs)
	if oerr != nil {
		return oerr
	}
	// 契约：relay.Serve「ctx 取消 → nil，真错误 → 非 nil」（见 pkg/tunnel/relay/leaf.go）。
	// 故判空守卫有意义：ctx 取消是优雅退出（由上层 runRelayWithRetry 的 ctx.Err() 门禁
	// 拦下，不再重连），只有真错误需要在此告警。
	err = relay.Serve(ctx, m, localAddr, dialAllow, httpClient, logger, opts...)
	if err != nil {
		logger.Warn("中继服务停止", "error", err)
	}
	return err
}

// relayDialTransport 按 --transport 建立到 hub 的 xfer 连接：tcp 裸 TCP / quic UDP /
// grpc HTTP/2 / ws（默认，WebSocket）。三者注册/信令/数据面协议完全一致，仅
// xfer.Conn 载体不同。ws:// 前缀用于 tcp/quic/grpc 时给清晰错误（tcp.Dial 会把
// "ws://..." 当 host 解析，报 "missing port" 之类难懂的错）。B17：insecure 时经
// hubWSDial 注入跳过证书校验的 HTTPClient（自签 wss hub）；caFile 非空时经
// HubWSDialCA 严格校验（受信 CA，替代 insecure）；--ws-upgrade-header 非空时发送
// X-WebSocket-Profile（服务端 WithUpgradeHeader 一致才连通）。
func relayDialTransport(ctx context.Context, transport, hubURL string, insecure bool, caFile, wsUpgradeHeader string) (xfer.Conn, error) {
	switch transport {
	case "ws", "":
		if caFile != "" {
			return mesh.HubWSDialCA(ctx, hubURL, caFile)
		}
		return mesh.HubWSDial(ctx, hubURL, insecure, wsUpgradeHeader)
	case "tcp", "quic", "grpc":
		return dialRawTransport(ctx, transport, hubURL)
	default:
		return nil, fmt.Errorf("未知传输层 %q（仅支持 ws/tcp）", transport)
	}
}

// dialRawTransport 拨号 raw TCP/QUIC/gRPC 传输：--hub 为 host:port（拒绝 ws(s)/http(s) URL
// 误传——tcp.Dial 会把 "ws://..." 当 host 解析，报 "missing port" 之类难懂的错）。
func dialRawTransport(ctx context.Context, transport, hubURL string) (xfer.Conn, error) {
	if strings.HasPrefix(hubURL, "ws://") || strings.HasPrefix(hubURL, schemeWSS) ||
		strings.HasPrefix(hubURL, "http://") || strings.HasPrefix(hubURL, "https://") {
		return nil, fmt.Errorf("--transport %s 的 --hub 应为 host:port（如 127.0.0.1:18084），不能是 URL 地址，got %q", transport, hubURL)
	}
	tp := xfer.Get(transport)
	if tp == nil {
		return nil, fmt.Errorf("%s 传输层未注册", transport)
	}
	return tp.Dial(ctx, hubURL)
}

// relayBuildMeta 构建注册帧的 Meta（Tags + Services）并收集宣告的服务地址（出口拨号
// 精确放行这些地址，含 loopback/私网，否则 mesh connect 回落中继路径拨 127.0.0.1:xxx
// 会被默认策略拒绝）。无效服务宣告（非 name:addr / addr 非 host:port）忽略并告警——
// 否则注册了"可见不可连"的服务，mesh connect 命中后必然拨号失败（S60）。
func relayBuildMeta(dialAllow bool, services []string, logger *slog.Logger) (hub.Meta, []string) {
	meta := hub.Meta{}
	var serviceAddrs []string
	if dialAllow {
		meta.Tags = append(meta.Tags, "exit")
	}
	for _, svc := range services {
		name, addr, ok := strings.Cut(svc, ":")
		if !ok || name == "" || addr == "" {
			logger.Warn("忽略无效服务宣告（应为 name:addr）", "raw", svc)
			continue
		}
		// S60：addr 必须是合法 host:port（net.SplitHostPort），且 host 非空
		// （拒绝 "x::22" 这类空 host）。否则注册了"可见不可连"的服务，
		// mesh connect 命中后必然拨号失败。服务端 hub/router validateServices
		// 应同步补 host:port 校验（B1 防御纵深，本批仅客户端）。
		if host, _, sperr := net.SplitHostPort(addr); sperr != nil || host == "" {
			logger.Warn("忽略无效服务宣告（addr 应为 host:port）", "raw", svc, "addr", addr, "error", sperr)
			continue
		}
		meta.Services = append(meta.Services, hub.Service{Name: name, Addr: addr})
		serviceAddrs = append(serviceAddrs, addr)
	}
	return meta, serviceAddrs
}

// relayServeOpts 构造 relay.ServeOptions：虚拟 IP NAT（selfVIP 由 REG_OK 下发；默认
// CGNAT 子网，可 --virtual-subnet 覆盖以匹配自定义 hub.virtual_subnet，服务宣告端口
// 自动开放）优先，内部已含宣告地址精确匹配（逃生口）与公网/CIDR 回落（S-1 审查修复）。
// DialResultFrames=true：经 hub 中继时向 hub 回写拨号结果帧，使 hub 在写 200 前能
// 确认数据面就绪（I27）。注意 p2p listen（webrtc 直连）必须保持 false，否则结果帧
// 会污染数据流。
func relayServeOpts(virtualSubnet string, selfVIP netip.Addr, dialAllowCIDRs, serviceAddrs []string) ([]relay.ServeOptions, error) {
	vipSubnet, vperr := netip.ParsePrefix(virtualSubnet)
	if vperr != nil || !vipSubnet.Addr().Is4() {
		return nil, fmt.Errorf("--virtual-subnet %q 非法（应为 IPv4 CIDR）", virtualSubnet)
	}
	vipSubnet = vipSubnet.Masked()
	return []relay.ServeOptions{
		{DialPolicy: relay.NewVirtualIPDialPolicy(vipSubnet, selfVIP, nil, dialAllowCIDRs, serviceAddrs), DialResultFrames: true},
	}, nil
}

// relayRegisterCaps 组装 relay 注册帧的能力列表。
// 基线：per-node-secret + virtual-ip（与旧版一致，零行为变更）；
// dialAllow 时追加 outbound-dial——本节点作为 SmartDial 多跳中间节点的可发现性标记
// （对端从 ListHubNodes 发现「可作中转出口」的候选；fail-closed：无此标记不选）。
// 抽成纯函数便于单测（注册帧 JSON 反解断言 caps）。
func relayRegisterCaps(dialAllow bool) []string {
	caps := []string{hub.CapabilityPerNodeSecret, hub.CapabilityVirtualIP}
	if dialAllow {
		caps = append(caps, hub.CapabilityOutboundDial)
	}
	return caps
}

// ---- 工厂函数 ----

// NewCmdRelay 创建 relay 父命令的工厂函数。
func NewCmdRelay(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "relay",
		Short: "中继节点管理",
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}
	cmd.AddCommand(NewCmdRelayStart(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStatus(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStop(ios))
	cmd.AddCommand(NewCmdRelayRemoveNode(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayStats(factory, ios, cfgSvc))
	cmd.AddCommand(NewCmdRelayDial(factory, ios))
	return cmd
}

// NewCmdRelayStart 创建 relay start 命令的工厂函数。
func NewCmdRelayStart(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "启动中继节点，连接到 Hub",
		Long: `作为中继节点连接到 Hub，注册自身，然后等待远程请求并通过隧道转发到本地 HTTP 服务。

使用示例:
  sclient relay start --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node
  sclient relay start --transport tcp --hub 127.0.0.1:18084 --node-id my-node --dial-allow`,
		RunE: func(cmd *cobra.Command, args []string) error {
			p := relayStartFromFlags(cmd, cfgSvc)
			// 运行中凭据自动轮换（renew 热替换到 creds——重连用最新 SK）。
			if stopRenew := relayStartCredRotation(cmd.Context(), cmd, factory, p); stopRenew != nil {
				defer stopRenew()
			}
			return runRelayStart(cmd, p.transport, p.hubURL, p.local, p.nodeID, p.accessKey, p.accessKeySecret, p.accessKeyID, p.insecure, p.caFile, p.dialAllow, p.services, p.dialAllowCIDRs, p.creds)
		},
	}
	cmd.Flags().String("transport", "ws", "连接到 Hub 的传输层: ws（默认，WebSocket）/ tcp（裸 TCP，hub.transports.tcp.listen）/ quic（QUIC UDP，hub.transports.quic.listen）")
	cmd.Flags().String("hub", "", "Hub 地址（默认取配置 hub_url；均未配置时 ws 用 ws://127.0.0.1:18084/ws、tcp 用 127.0.0.1:18084、quic 用 127.0.0.1:18088）")
	cmd.Flags().String("ws-path", "/ws", "WS 升级路径（roadmap §5.3 P1 被动伪装层：与服务端 hub.transports.ws.path 一致；默认 /ws 零回归，自定义时 --hub 省略路径也生效）")
	cmd.Flags().String("ws-upgrade-header", "", "WS 升级附加校验头值（roadmap §5.3 P1 被动伪装层：与服务端 hub.transports.ws.upgrade_header 一致才连通；空 = 不发送零回归）")
	cmd.Flags().String("local", "http://127.0.0.1:8080", "本地 HTTP 服务地址")
	cmd.Flags().String("node-id", "", "节点唯一标识 (默认使用时间戳)")
	cmd.Flags().Bool("dial-allow", false, "作为出口节点：允许收到 dial 帧时向目标地址发起出站 TCP 连接（供中继端充当出口网关）")
	cmd.Flags().StringArray("service", nil, "宣告一个 mesh 服务（格式 name:addr，可重复；供 sclient mesh connect 发现）")
	cmd.Flags().StringArray("dial-allow-cidr", nil, "出口拨号白名单网段（如 192.168.0.0/16；配合 --dial-allow 放行内网服务，默认仅公网）")
	cmd.Flags().String(flagVirtualSubnet, hub.DefaultVirtualSubnet, "虚拟 IP 子网（CIDR，仅 IPv4；需与 hub.virtual_subnet 配置一致；默认 CGNAT 100.64.0.0/10）")
	return cmd
}

// relayStartParams 是 relay start 的参数集合（flag + 配置回落，P2-配置3）。
type relayStartParams struct {
	transport       string
	hubURL          string
	local           string
	nodeID          string
	accessKey       string
	accessKeySecret string
	accessKeyID     string
	insecure        bool
	caFile          string
	dialAllow       bool
	services        []string
	dialAllowCIDRs  []string
	creds           *credrotate.Credentials // 动态凭据（运行中自动轮换；renew 热替换）
}

// relayStartFromFlags 解析 relay start 的 flag 并补齐配置回落（CLI > 配置文件 >
// 默认）：--hub/--node-id/--access-key/--access-key-secret/--access-key-id/--ca-file
// 未显式指定时取配置。
func relayStartFromFlags(cmd *cobra.Command, cfgSvc ConfigProvider) *relayStartParams {
	p := &relayStartParams{}
	p.transport, _ = cmd.Flags().GetString("transport")
	p.hubURL, _ = cmd.Flags().GetString("hub")
	p.local, _ = cmd.Flags().GetString("local")
	p.nodeID, _ = cmd.Flags().GetString("node-id")
	p.accessKey, _ = cmd.Flags().GetString("access-key")
	p.accessKeySecret, _ = cmd.Flags().GetString("access-key-secret")
	p.accessKeyID, _ = cmd.Flags().GetString("access-key-id")
	p.insecure, _ = cmd.Flags().GetBool("insecure")
	p.caFile, _ = cmd.Flags().GetString("ca-file")
	p.dialAllow, _ = cmd.Flags().GetBool("dial-allow")
	p.services, _ = cmd.Flags().GetStringArray("service")
	p.dialAllowCIDRs, _ = cmd.Flags().GetStringArray("dial-allow-cidr")
	// 动态凭据容器（运行中自动轮换支持）。
	p.creds = credrotate.NewCredentials(p.accessKey, p.accessKeySecret, p.accessKeyID)
	if cfgSvc != nil {
		if cfg, cerr := cfgSvc.LoadConfig(); cerr == nil {
			if p.hubURL == "" {
				p.hubURL = cfg.HubURL
			}
			if p.accessKey == "" {
				p.accessKey = cfg.AccessKey
			}
			if p.accessKeySecret == "" {
				p.accessKeySecret = cfg.AccessKeySecret
			}
			if p.accessKeyID == "" {
				p.accessKeyID = cfg.AccessKeyID
			}
			if p.nodeID == "" {
				p.nodeID = cfg.NodeID
			}
			if p.caFile == "" {
				p.caFile = cfg.XferCAFile
			}
		}
	}
	return p
}

// relayStartCredRotation 启动运行中凭据自动轮换（--renew-interval>0 且已配置
// access_key_secret/access_key_id 时）：临时 FileClient 做 renew → OnRotate 热替换
// 到动态凭据——重连用最新 SK，无需重启。返回 stop 函数（未启动时为 nil）。
func relayStartCredRotation(ctx context.Context, cmd *cobra.Command, factory clientfactory.Factory, p *relayStartParams) func() {
	renewInterval, _ := cmd.Flags().GetDuration("renew-interval")
	if renewInterval <= 0 || (p.accessKeySecret == "" && p.accessKeyID == "") {
		return nil
	}
	if renewSvc, rerr := factory.NewClient(cmd); rerr == nil && renewSvc != nil {
		if stopRenew, ok := credrotate.Start(ctx, renewSvc, credrotate.Options{
			Interval: renewInterval,
			Logger:   slog.Default(),
			OnRotate: p.creds.Update,
		}); ok {
			return stopRenew
		}
	}
	return nil
}

// NewCmdRelayStatus 创建 relay status 命令的工厂函数。
func NewCmdRelayStatus(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "查看 Hub 节点状态",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := relayHubClient(cmd, factory, cfgSvc)
			if err != nil {
				return err
			}

			nodes, err := svc.ListHubNodes(cmd.Context())
			if err != nil {
				return fmt.Errorf("查询 Hub 状态失败: %w", err)
			}

			if len(nodes) == 0 {
				ios.WriteOutLine("暂无已连接节点")
				return nil
			}

			ios.WriteOutLine("已连接节点 (%d):", len(nodes))
			for _, n := range nodes {
				connected := ""
				if !n.Connected.IsZero() {
					connected = n.Connected.Format("2006-01-02 15:04:05")
				}
				ios.WriteOutLine("  - ID:       %s", n.ID)
				ios.WriteOutLine("    地址:     %s", n.Addr)
				ios.WriteOutLine("    连接时间: %s", connected)
			}
			return nil
		},
	}
	cmd.Flags().String("hub", "", "Hub 的 HTTP 地址 (如 http://127.0.0.1:18083)")
	return cmd
}

// NewCmdRelayStop 创建 relay stop 命令的工厂函数。
func NewCmdRelayStop(ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "停止中继节点",
		Long: `向正在运行的中继节点发送停止信号。

中继节点作为独立进程运行时，请使用 kill 或 SIGINT 停止。
如果通过 sclient relay start 前台运行，按 Ctrl+C 即可停止。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ios.WriteOutLine("请向中继进程发送 SIGINT 信号以优雅停止。")
			ios.WriteOutLine("如果中继在前台运行，请按 Ctrl+C。")
			return nil
		},
	}
}
