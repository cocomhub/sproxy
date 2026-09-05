// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// parseVirtualSubnet 解析虚拟 IP 子网：空/非法/非 IPv4 回落默认 CGNAT 100.64.0.0/10
// （非法配置经 config.Validate 拦截，此处仅防御性兜底）。
func parseVirtualSubnet(cidr string) netip.Prefix {
	if cidr != "" {
		if p, err := netip.ParsePrefix(cidr); err == nil && p.Addr().Is4() {
			return p.Masked()
		}
	}
	return netip.MustParsePrefix(hub.DefaultVirtualSubnet)
}

const (
	// nodeReconnectBaseDelay 是 mesh node 断线重连的初始退避。
	nodeReconnectBaseDelay = 1 * time.Second
	// nodeReconnectMaxDelay 是重连退避上限。
	nodeReconnectMaxDelay = 30 * time.Second
	// defaultDiscoveryInterval 是自动对等发现的默认周期。
	defaultDiscoveryInterval = 10 * time.Second
	// discoveryFailedPeerCooldown 是对等拨号失败后的冷却时长（避免每周期重复探测）。
	discoveryFailedPeerCooldown = 60 * time.Second
	// defaultDiscoveryMaxParallel 是并行拨号的默认并发数。
	defaultDiscoveryMaxParallel = 3
)

// NodeConfig 是 mesh node 常驻节点的配置。
type NodeConfig struct {
	// HubURL 是 hub 地址（http(s)/ws(s)，空时回落 ServerURL）。
	HubURL string
	// ServerURL 是 HubURL 为空的回退基址。
	ServerURL string
	// NodeID 是本节点稳定 ID（为空回落主机名；mesh connect 用它寻址，需唯一）。
	NodeID string
	// AccessKey 是 SproxySig 请求签名认证的 AccessKey（信令/节点列表/网关/hub 注册准入）。
	AccessKey string
	// AccessKeySecret 是 SproxySig AccessKeySecret（本地密钥，仅计算签名，永不上线）。
	AccessKeySecret string
	// AccessKeyID 是 SproxySig SK 条目 ID（skey-id，v2 协议必传——信令/节点列表/网关签名用）。
	AccessKeyID string
	// Services 是宣告到 hub 的服务（mesh connect 服务发现）。
	Services []hub.Service
	// ServiceAddrs 是出口拨号精确放行地址（含 loopback/私网，供 NewServiceDialPolicy）。
	ServiceAddrs []string
	// Tags 是节点标签（如 ["exit"] 表示出口节点，mesh node --dial-allow 时打）。
	Tags []string
	// DialAllow 允许出口拨号（mesh connect 恒发 dial 帧，依赖此开关）。
	DialAllow bool
	// DialAllowCIDRs 是出口拨号额外放行的网段。
	DialAllowCIDRs []string
	// VirtualSubnet 是虚拟 IP 子网（CIDR，空回落默认 CGNAT 100.64.0.0/10）。
	// 有 hub 时虚拟 IP 由 hub 分配（REG_OK 下发）；mDNS 无 hub 模式用本地确定性分配。
	VirtualSubnet string
	// VIPAllowPorts 是虚拟 IP 开放的额外端口白名单（--vip-allow-port，可多次）。
	// 缺省 = --service 宣告端口自动开放；此处可额外开放未宣告的本机服务端口。
	VIPAllowPorts []int
	// LocalAddr 是 HTTP 中继转发目标（空回落 http://127.0.0.1:8080）。
	LocalAddr string
	// Insecure 注册 WS + 信令 HTTP 跳过证书校验（自签 wss hub）。
	Insecure bool
	// EnableWebRTC 是否接受 WebRTC 直连（信令 poll + listen）。
	EnableWebRTC bool
	// Discover 启用自动对等发现：周期经 hub 节点列表发现其他 mesh node，
	// 并行 webrtc 自动直连并保持，形成 full-mesh 拓扑（只验证可达 + 拓扑，不承载业务）。
	Discover bool
	// DiscoveryInterval 是发现周期（0 回落 defaultDiscoveryInterval）。
	DiscoveryInterval time.Duration
	// DiscoveryProbeTimeout 是单次对等拨号探测超时（0 回落 WebRTCProbeTimeout）。
	DiscoveryProbeTimeout time.Duration
	// DiscoveryMaxParallel 是并行拨号并发数（0 回落 defaultDiscoveryMaxParallel）。
	// 每个拨号用独立临时信令身份（per-dial AutoRegister），规避共享 signaler 的
	// WaitAnswer 竞态；限并发控制瞬时注册开销与对端 accept 环卡窗叠加。
	DiscoveryMaxParallel int
	// DiscoveryPeers 是可选的观测通道：每次建立新的对等直连时非阻塞发送 peer nodeID。
	DiscoveryPeers chan<- string
	// GatewayAddr 是本地网关监听地址（mesh connect --gateway 复用已建直连链路的
	// 入口；空回落 GatewayDefaultAddr；同机多 mesh node 时用 127.0.0.1:0 随机端口）。
	// 仅监听 loopback，安全默认。
	GatewayAddr string
	// GatewayNotify 是可选的观测通道：网关实际监听地址（含回落随机端口）就绪时
	// 非阻塞发送（供测试/上层启动流程感知）。
	GatewayNotify chan<- string
	// EnableMDNS 启用 mDNS 局域网发现：广播本节点（node-id + 服务 + 直连信令端点）、
	// 浏览并自动直连同网段其他 mesh node。MDNSOnly=true 时进入纯 mDNS 无 hub 模式
	// （本节点不注册 hub，直连信令替代 HubSignaler，供局域网内互发现与直连）。
	EnableMDNS bool
	// MDNSOnly 强制纯 mDNS 无 hub 模式（mesh node --mdns）：忽略 HubURL/ServerURL，
	// 不注册 hub。与 EnableMDNS 一起由 CLI 设置。
	MDNSOnly bool
	// MDNSPort 是 mDNS 组播端口（0 回落 5353；测试可覆盖）。
	MDNSPort int
	// SignalAddr 是直连 webrtc 信令的监听地址（空回落 ":0" 全接口随机端口）。
	// 测试可指定 "127.0.0.1:0" 收敛到 loopback（避免防火墙弹窗）。实际广播的
	// saddr 按监听 host 派生：通配 host 用主局域网 IP，显式 host 原样保留。
	SignalAddr string
	// SocksAddr 是本地 SOCKS5 出口监听地址（mesh node --socks；空 = 不启用）。
	// 本节点作为出口：CONNECT 目标由节点本机拨号（本地网络出口）。远程 peer 可
	// `mesh connect socks -l :port` 隧道到它使用。监听默认 loopback 安全；
	// 可配 --socks-user/--socks-pass 要求 RFC 1929 认证。
	SocksAddr string
	// SocksUser / SocksPass 是 SOCKS5 RFC 1929 认证凭据（仅 SocksAddr 非空时生效；
	// 配置了才要求认证，防未授权使用本节点作代理）。
	SocksUser string
	SocksPass string
	// MDNSPeerSecret 是 mDNS 模式的共享密钥（--mdns-secret）。非空时：
	//   - 直连信令 offer 携带 HMAC 签名，listener 校验（防未授权 peer 借本节点作
	//     中继/出口）；
	//   - mDNS TXT 宣告携带 HMAC 签名，浏览方校验（防伪造/MITM）。
	// 同 mesh 所有节点须配置相同密钥；空 = 无认证（LAN 信任模型，出口由
	// dial-allow 策略约束）。
	MDNSPeerSecret string
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// RunNode 运行 mesh 常驻节点：单进程单注册（稳定 node-id + 服务宣告 + per-node
// secret），并行提供中继服务（经 hub 的中继流）与 WebRTC 直连（信令 poll + listen），
// 断线指数退避重连（per-node secret 随重连轮换）。阻塞直到 ctx 取消或终态错误
// （hub 注册被拒，errors.Is(err, hub.ErrRegisterRejected)）。
func RunNode(ctx context.Context, cfg NodeConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	// 纯 mDNS 无 hub 模式：不注册 hub，直连信令 + mDNS 发现/广播。无重连语义
	// （无 hub 会话可断）；ctx 取消即优雅退出。触发条件：显式 MDNSOnly（mesh node
	// --mdns），或 EnableMDNS 且无 hub 可连（HubURL/ServerURL 均为空）。
	if cfg.MDNSOnly || (cfg.EnableMDNS && cfg.HubURL == "" && cfg.ServerURL == "") {
		return runNodeMDNSOnly(ctx, cfg, logger)
	}
	delay := nodeReconnectBaseDelay
	for {
		err := runNodeOnce(ctx, cfg, logger)
		if err == nil || ctx.Err() != nil {
			return err
		}
		if errors.Is(err, hub.ErrRegisterRejected) {
			return err
		}
		logger.Warn("mesh node 会话断开，退避重连", "delay", delay, "error", err)
		select {
		case <-time.After(delay):
			delay *= 2
			if delay > nodeReconnectMaxDelay {
				delay = nodeReconnectMaxDelay
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runNodeOnce 一次完整注册 + 中继/直连双循环。
// 返回 nil（ctx 取消/优雅退出）或真实错误（触发 RunNode 退避重连）。
//
// 生命周期：cycleCtx 内 AutoRegister（拿注册 mux + per-node secret + HubSignaler），
// 两个 goroutine 并行——中继 relay.Serve（注册 mux 上）与 webrtc 直连环；
// 首个真实失败 → cycleCancel 终止双 serve → 幂等 closeReg 关注册连接（hub
// RemoveIfOwned 摘节点）→ wg.Wait 有界回收。
func runNodeOnce(ctx context.Context, cfg NodeConfig, logger *slog.Logger) error {
	cycleCtx, cycleCancel := context.WithCancel(ctx)
	defer cycleCancel()

	reg, err := AutoRegister(cycleCtx, AutoRegisterParams{
		HubURL:          cfg.HubURL,
		ServerURL:       cfg.ServerURL,
		AccessKey:       cfg.AccessKey,
		AccessKeySecret: cfg.AccessKeySecret,
		AccessKeyID:     cfg.AccessKeyID,
		NodeID:          cfg.NodeID,
		Prefix:          "mesh",
		ExactNode:       true, // mesh node 是稳定 node-id，供 mesh connect 寻址
		Insecure:        cfg.Insecure,
		Services:        cfg.Services,
		Tags:            cfg.Tags,
	})
	if err != nil {
		return err
	}
	var closeOnce sync.Once
	closeReg := func() { closeOnce.Do(func() { _ = reg.Closer() }) }
	defer closeReg()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	localAddr := cfg.LocalAddr
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	// 出口拨号策略：虚拟 IP NAT（selfVIP 由 REG_OK 下发；子网默认 CGNAT，可配
	// --virtual-subnet 覆盖）优先，内部已含宣告地址精确匹配（逃生口）与公网/CIDR
	// 回落。selfVIP 无效（临时身份/旧 hub）时虚拟子网内 fail-closed 拒绝，其余不变。
	subnet := parseVirtualSubnet(cfg.VirtualSubnet)
	// S-4：自定义 hub.virtual_subnet 时，本节点 selfVIP（REG_OK 下发）若不在配置的
	// 虚拟子网内，虚拟 IP 拨号将 fail-closed 拒绝——此处显式告警（而非仅出口策略
	// Warn）引导核对 hub.virtual_subnet 与 --virtual-subnet 一致性。
	if reg.VirtualIP.IsValid() && !subnet.Contains(reg.VirtualIP) {
		logger.Warn("本节点虚拟 IP 不在配置的虚拟子网内（请检查 hub.virtual_subnet 与 --virtual-subnet 是否一致；不一致时虚拟 IP 拨号 fail-closed 拒绝）", "self_vip", reg.VirtualIP, "subnet", subnet)
	}
	vipPolicy := relay.NewVirtualIPDialPolicy(subnet, reg.VirtualIP, cfg.VIPAllowPorts, cfg.DialAllowCIDRs, cfg.ServiceAddrs)
	// 中继路径 DialResultFrames=true：hub 写 200 前读拨号结果帧确认数据面就绪（I27）。
	relayOpts := []relay.ServeOptions{
		{DialPolicy: vipPolicy, DialResultFrames: true},
	}
	// 直连路径 DialResultFrames=false：结果帧会污染 webrtc 数据流（见 relay/leaf.go）。
	directOpts := []relay.ServeOptions{
		{DialPolicy: vipPolicy},
	}

	// 自动对等发现隐含需接受回拨（Discover=true 时即使 EnableWebRTC=false 也跑直连环）。
	enableAccept := cfg.EnableWebRTC || cfg.Discover
	errCh := make(chan error, 3)
	// 共享链路池：自动对等发现写入，本地网关复用（mesh connect --gateway 路由）。
	links := newLinkPool()
	// 虚拟 IP 表：由 hub 节点列表填充（认证数据源），供网关虚拟 IP 路由与本地拓扑。
	// subnet 与出口拨号策略一致（默认 CGNAT，可 --virtual-subnet 覆盖）。
	vipTable := NewVipTable(parseVirtualSubnet(cfg.VirtualSubnet))
	gw := newGateway(links, cfg, logger, vipTable)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := relay.Serve(cycleCtx, reg.Mux, localAddr, cfg.DialAllow, httpClient, logger, relayOpts...); err != nil {
			errCh <- err
		}
	}()
	wg.Add(1)
	if enableAccept {
		go func() {
			defer wg.Done()
			if err := runWebRTCAcceptLoop(cycleCtx, reg.Signaler, reg.TempNode, localAddr, cfg.DialAllow, httpClient, logger, links, directOpts); err != nil {
				errCh <- err
			}
		}()
	} else {
		go func() {
			defer wg.Done()
			<-cycleCtx.Done()
		}()
	}
	// 本地网关（恒启用，loopback）：mesh connect --gateway 复用已建链路的入口。
	// 绑定失败（默认端口被占 + 随机端口也失败）不致命——节点仍经 hub 中继/webrtc 直连
	// 服务，只是复用已建链路的快捷路径不可用。
	wg.Add(1)
	go func() {
		defer wg.Done()
		actual, gerr := gw.Serve(cycleCtx, cfg.GatewayAddr)
		if gerr != nil {
			logger.Warn("mesh 本地网关不可用（mesh connect --gateway 将回落常规拨号）", "error", gerr)
		} else {
			logger.Info("mesh 本地网关就绪（mesh connect --gateway 复用已建直连链路）", "addr", actual)
			if cfg.GatewayNotify != nil {
				select {
				case cfg.GatewayNotify <- actual:
				default:
				}
			}
		}
		<-cycleCtx.Done()
	}()
	if cfg.SocksAddr != "" { // 本地 SOCKS5 出口（本节点为出口，CONNECT 目标本机拨号）
		wg.Add(1)
		go func() {
			defer wg.Done()
			// 绑定失败不致命：Warn 后节点仍正常运行（对齐网关降级）。
			if err := serveLocalSocks(cycleCtx, cfg.SocksAddr, cfg.SocksUser, cfg.SocksPass, logger); err != nil {
				logger.Warn("mesh SOCKS5 出口不可用（节点仍正常运行）", "error", err)
			}
		}()
	}
	if cfg.Discover {
		httpBase, _, herr := hub.NormalizeEndpoints(cfg.HubURL, cfg.ServerURL)
		if herr != nil {
			return herr
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runDiscoveryLoop(cycleCtx, cfg, reg.TempNode, httpBase, links, reg.Secret, localAddr, httpClient, directOpts, vipTable, logger); err != nil {
				// 非阻塞写：只有 /api/hub/nodes 4xx（auth/配置级）才致命触发整 cycle
				// 重连；拨号/瞬时失败在 runDiscoveryLoop 内部冷却处理，不写 errCh。
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	var loopErr error
	select {
	case err := <-errCh:
		loopErr = err
	case <-ctx.Done():
		loopErr = nil // 优雅退出
	}
	cycleCancel() // 终止中继/直连 serve（Accept/轮询返回）
	closeReg()    // 关闭注册 mux → WS → hub RemoveIfOwned
	wg.Wait()     // 有界：cycleCtx 取消保证 goroutine 在 ≤一次 poll/Accept 周期内返回
	if ctx.Err() != nil {
		return nil
	}
	return loopErr
}

// runWebRTCAcceptLoop 循环接受 webrtc 直连：每条直连用 relay.Serve 分发
// （dial 帧→出口拨号 / HTTP 中继到 localAddr）。
//
// signaler 抽象信令通道：hub 模式传 *hub.HubSignaler（HTTP 存转桥），mDNS 无 hub
// 模式传 directSignalerServer（TCP 直连信令）。两者均实现 webrtc.Signaler。
//
// 自动对等发现的拨号（临时身份 disc-<base>-<unixnano>）接受后，把该链路注册进共享
// linkPool（键=拨号者真实 node ID），使本节点网关也能回拨对端（同一条已建链路双向
// 服务互访）；serve 结束（链路断开）removeIf 摘除（重连竞态安全）。mesh connect /
// p2p 的临时拨号（mesh-/p2p- 前缀）不注册。
//
// 空闲（ErrNoIncomingConnection，signalingTimeout 内无对端发起连接）不是失败，
// 不重注册继续监听（P1-11）；ctx 取消返回 nil；真实信令失败返回错误触发整 cycle
// 重连（节点被 hub 移除时 secret 已轮换，重连即拿新 secret 自愈）。
func runWebRTCAcceptLoop(ctx context.Context, signaler webrtc.Signaler, nodeID, localAddr string, dialAllow bool, httpClient *http.Client, logger *slog.Logger, links *linkPool, opts []relay.ServeOptions) error {
	for {
		conn, err := webrtc.ListenWithSignalerCtx(ctx, nodeID, signaler)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, webrtc.ErrNoIncomingConnection) || errors.Is(err, errDirectSignalConn) {
				// ErrNoIncomingConnection：超时窗口内无 offer → 空闲，继续监听（P1-11）。
				// errDirectSignalConn：直连信令一条连接异常（端口扫描/畸形帧）→ 已关闭该
				// 连接，属 per-connection 瞬时失败，继续监听而非把整节点判死（F1）。
				continue
			}
			return err
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		// discovery 拨号（disc- 前缀）→ 恢复真实 node ID 并注册链路（网关双向复用）。
		// base 已由 hub 注册时强制校验（base==real_node_id + HMAC 证明），不可伪造；
		// 再加半拨号序校验 peerID<nodeID 作纵深（真实 discovery 恒低 ID 拨高 ID，
		// 绝不误伤正常注册），根除"冒充高 ID"类投毒与 discovery 侧 set 竞态。
		peerID, isDiscovery := parseDiscoveryPeerID(conn.RemotePeerID())
		registered := isDiscovery && peerID < nodeID
		if registered {
			links.set(peerID, m)
			logger.Info("mesh 自动对等链路 accept 注册", "peer", peerID)
		}
		go func(m *mux.Mux, peerID string, registered bool) {
			defer m.Close() // serve 结束即关 mux → 关底层 webrtc conn → 解除 pump
			if err := relay.Serve(ctx, m, localAddr, dialAllow, httpClient, logger, opts...); err != nil {
				logger.Debug("mesh node 直连会话结束", "error", err)
			}
			if registered {
				// 仅当链路池中仍指向本条 mux 才移除（防重连竞态：新链路已 set 时不误删）。
				links.removeIf(peerID, m)
			}
		}(m, peerID, registered)
	}
}
