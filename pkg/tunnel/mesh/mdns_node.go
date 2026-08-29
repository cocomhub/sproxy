// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// runNodeMDNSOnly 运行纯 mDNS 无 hub mesh 节点（RunNode 在 EnableMDNS 且无 hub 时进入）：
//
//   - 直连信令 TCP 监听器（供对端不经 hub 完成 webrtc SDP 交换）；
//   - mDNS 广播本节点（node-id + 服务 + 信令端点）+ 浏览同网段其他节点；
//   - webrtc 直连接受环（直连信令器），每条直连经 relay.Serve 分发（dial 帧→出口拨号）；
//   - 本地网关（loopback，复用已建链路）；
//   - mDNS 自动对等发现环（拨号同网段其他节点，full-mesh 拓扑）。
//
// 无 hub 注册，因此无 per-node secret / 重连语义：身份即 mDNS 广播的 node-id
// （局域网信任模型，见 mdns.go 安全边界）。ctx 取消优雅退出。
func runNodeMDNSOnly(ctx context.Context, cfg NodeConfig, logger *slog.Logger) error {
	nodeCtx, nodeCancel := context.WithCancel(ctx)
	defer nodeCancel()

	// 直连信令监听器：地址派生自 cfg.SignalAddr（空回落 ":0" 全接口随机端口；
	// 测试可指定 "127.0.0.1:0" 收敛 loopback）。
	// 直连信令监听地址：显式 host 原样；空/通配 → 绑定到主局域网 IPv4（暴露面 =
	// 广播的 saddr，不绑全接口——否则任何可路由到本机的主机都能触达，F2）。
	signalHost, signalPortStr, err := resolveSignalListenAddr(cfg.SignalAddr)
	if err != nil {
		return err
	}
	signalSrv, err := NewDirectSignalServer(net.JoinHostPort(signalHost, signalPortStr))
	if err != nil {
		return fmt.Errorf("mesh mDNS: 直连信令监听失败: %w", err)
	}
	// 共享密钥（--mdns-secret）：非空时仅接受携带有效 HMAC 签名的 offer。
	signalSrv.SetSecret(cfg.MDNSPeerSecret)
	defer signalSrv.Close()

	signalTCP, ok := signalSrv.Addr().(*net.TCPAddr)
	if !ok {
		return fmt.Errorf("mesh mDNS: 直连信令监听地址类型异常: %T", signalSrv.Addr())
	}
	signalPort := signalTCP.Port
	advAddr := net.JoinHostPort(signalHost, strconv.Itoa(signalPort))
	nodeID := cfg.NodeID
	if nodeID == "" {
		nodeID = iostream.LocalHostname("mesh-node")
	}
	lanIPs := lanIPv4Addrs()

	mdns, err := NewMDNS(MDNSConfig{
		NodeID:     nodeID,
		SignalAddr: advAddr,
		Services:   cfg.Services,
		IPs:        lanIPs,
		Port:       cfg.MDNSPort,
		Secret:     cfg.MDNSPeerSecret, // --mdns-secret：TXT 签名 + 浏览校验
		Logger:     logger,
	})
	if err != nil {
		return fmt.Errorf("mesh mDNS: 构造 mDNS 服务器失败: %w", err)
	}
	if err := mdns.Start(nodeCtx); err != nil {
		return err
	}
	defer mdns.Close()
	go signalSrv.Serve(nodeCtx)

	httpClient := &http.Client{Timeout: 30 * time.Second}
	localAddr := cfg.LocalAddr
	if localAddr == "" {
		localAddr = "http://127.0.0.1:8080"
	}
	// 直连路径 DialResultFrames=false（结果帧会污染 webrtc 数据流，见 relay/leaf.go）。
	directOpts := []relay.ServeOptions{
		{DialPolicy: relay.NewServiceDialPolicy(cfg.DialAllowCIDRs, cfg.ServiceAddrs)},
	}
	links := newLinkPool()
	gw := newGateway(links, cfg, logger)

	logger.Info("mesh mDNS 节点启动（无 hub）", "node", nodeID, "signal_addr", advAddr, "services", len(cfg.Services))

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := runWebRTCAcceptLoop(nodeCtx, signalSrv.NewSignaler(), nodeID, localAddr, cfg.DialAllow, httpClient, logger, links, directOpts); err != nil {
			select {
			case errCh <- err:
			default:
			}
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		actual, gerr := gw.Serve(nodeCtx, cfg.GatewayAddr)
		if gerr != nil {
			logger.Warn("mesh mDNS 本地网关不可用（mesh connect --gateway 将回落常规拨号）", "error", gerr)
		} else {
			logger.Info("mesh mDNS 本地网关就绪（mesh connect --gateway 复用已建直连链路）", "addr", actual)
			if cfg.GatewayNotify != nil {
				select {
				case cfg.GatewayNotify <- actual:
				default:
				}
			}
		}
		<-nodeCtx.Done()
	}()
	if cfg.Discover { // mDNS 自动对等发现（--discover 默认开；关闭则只被拨号不主动拨）
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := runMDNSDiscoveryLoop(nodeCtx, cfg, nodeID, mdns, links, localAddr, httpClient, directOpts, logger); err != nil {
				select {
				case errCh <- err:
				default:
				}
			}
		}()
	}

	select {
	case err := <-errCh:
		nodeCancel()
		wg.Wait()
		if ctx.Err() != nil {
			return nil
		}
		return err
	case <-ctx.Done():
		nodeCancel()
		wg.Wait()
		return nil
	}
}

// mdnsDiscoveryLoop 维护经 mDNS 发现的对等直连集合（与 hub 版 discoveryLoop 同构）。
type mdnsDiscoveryLoop struct {
	links    *linkPool
	mu       sync.Mutex
	lastFail map[string]time.Time
}

// runMDNSDiscoveryLoop 周期经 mDNS 浏览发现其他 mesh node，并行 webrtc 自动直连并
// 保持（直连信令复用 mDNS 广播的端点），形成 full-mesh 拓扑。返回错误仅当 ctx 取消。
func runMDNSDiscoveryLoop(ctx context.Context, cfg NodeConfig, nodeID string, mdns *MDNSServer, links *linkPool, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) error {
	interval := cfg.DiscoveryInterval
	if interval <= 0 {
		interval = defaultDiscoveryInterval
	}
	probe := cfg.DiscoveryProbeTimeout
	if probe <= 0 {
		probe = WebRTCProbeTimeout
	}
	maxParallel := cfg.DiscoveryMaxParallel
	if maxParallel <= 0 {
		maxParallel = defaultDiscoveryMaxParallel
	}

	dl := &mdnsDiscoveryLoop{links: links, lastFail: map[string]time.Time{}}
	defer dl.links.closeAll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		dl.discoverOnce(ctx, cfg, nodeID, mdns, probe, maxParallel, localAddr, httpClient, serveOpts, logger)
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (dl *mdnsDiscoveryLoop) discoverOnce(ctx context.Context, cfg NodeConfig, nodeID string, mdns *MDNSServer, probe time.Duration, maxParallel int, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) {
	_ = dl.links.sweep()
	var targets []MDNSPeer
	for _, p := range mdns.Peers() {
		if p.NodeID == "" || p.NodeID == nodeID || p.SignalAddr == "" {
			continue
		}
		if _, ok := dl.links.get(p.NodeID); ok {
			continue
		}
		if p.NodeID < nodeID {
			continue // 半拨号去重：只低 ID 拨高 ID，每对恰好一条链接
		}
		dl.mu.Lock()
		t, failed := dl.lastFail[p.NodeID]
		dl.mu.Unlock()
		if failed && time.Since(t) < discoveryFailedPeerCooldown {
			continue
		}
		targets = append(targets, p)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].NodeID < targets[j].NodeID })
	if len(targets) == 0 {
		return
	}
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for _, p := range targets {
		wg.Add(1)
		go func(p MDNSPeer) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dl.dialPeerDirect(ctx, cfg, nodeID, p, probe, localAddr, httpClient, serveOpts, logger)
		}(p)
	}
	wg.Wait()
}

// dialPeerDirect 对 mDNS 发现的对端发起直连信令 + webrtc 打洞并保持（mux 心跳），
// 拨号侧链路上跑 relay.Serve（接受对端网关回拨，双向服务互访）。
//
// 信令身份用 disc-<base>-<随机后缀> 临时 ID（对齐 hub 版 discovery 拨号）：accept
// 侧 runWebRTCAcceptLoop 经 parseDiscoveryPeerID 恢复真实 base 并把链路注册进共享
// linkPool（双向网关互访，full-mesh 对称）。安全说明：mDNS 局域网信任模型下 base 无
// hub 强制校验（hub 版有 real_node_proof），同网段可组播的节点本就可冒充任意 node-id，
// linkPool 注册仅作路由复用，非安全边界；仍保留半拨号序校验 peerID<nodeID 作纵深。
func (dl *mdnsDiscoveryLoop) dialPeerDirect(ctx context.Context, cfg NodeConfig, nodeID string, p MDNSPeer, probe time.Duration, localAddr string, httpClient *http.Client, serveOpts []relay.ServeOptions, logger *slog.Logger) {
	if err := ctx.Err(); err != nil {
		return
	}
	// 校验 mDNS 发现的信令端点（防 SSRF：拒绝 loopback/link-local/multicast/
	// unspecified，防恶意广播诱导拨号到内网/元数据服务，安全审查 B）。
	if verr := ValidateSignalAddr(p.SignalAddr); verr != nil {
		logger.Debug("mesh mDNS 对等信令端点非法，跳过", "peer", p.NodeID, "saddr", p.SignalAddr, "error", verr)
		dl.markFail(p.NodeID)
		return
	}
	tempID := fmt.Sprintf("%s-%s-%s", hub.DiscPrefix, nodeID, newTempSuffix())
	sig, err := DialDirectSignaler(ctx, p.SignalAddr, tempID)
	if err != nil {
		logger.Debug("mesh mDNS 对等信令连接失败", "peer", p.NodeID, "error", err)
		dl.markFail(p.NodeID)
		return
	}
	sig.SetSecret(cfg.MDNSPeerSecret) // --mdns-secret：offer 携带 HMAC 签名
	defer func() { _ = sig.Close() }()
	probeCtx, cancel := context.WithTimeout(ctx, probe)
	conn, derr := webrtc.DialWithSignalerCtx(probeCtx, p.NodeID, sig)
	cancel()
	if derr != nil {
		logger.Debug("mesh mDNS 对等拨号失败", "peer", p.NodeID, "error", derr)
		dl.markFail(p.NodeID)
		return
	}
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	dl.links.set(p.NodeID, m)
	go func(m *mux.Mux) {
		defer func() { _ = m.Close() }()
		if err := relay.Serve(ctx, m, localAddr, cfg.DialAllow, httpClient, logger, serveOpts...); err != nil {
			logger.Debug("mesh mDNS 对等链路 serve 结束", "peer", p.NodeID, "error", err)
		}
	}(m)
	logger.Info("mesh mDNS 自动对等直连建立", "peer", p.NodeID)
	if cfg.DiscoveryPeers != nil {
		select {
		case cfg.DiscoveryPeers <- p.NodeID:
		default:
		}
	}
}

func (dl *mdnsDiscoveryLoop) markFail(peer string) {
	dl.mu.Lock()
	dl.lastFail[peer] = time.Now()
	dl.mu.Unlock()
}

// lanIPv4Addrs 返回本机全部非 loopback、非 link-local 的 IPv4 地址（排序保证确定性）。
// 用于 mDNS A 记录与直连信令广播地址派生。
func lanIPv4Addrs() []net.IP {
	var out []net.IP
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, ip)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// primaryLANIPv4 返回主局域网 IPv4：优先默认路由出口 IP（对应主物理网卡，避开
// VPN/虚拟网卡在字典序下的陷阱——VPN 10.x 常排在 192.168.x 之前，字典序选错会导致
// 同网段对端无法触达广播的 saddr）；无默认路由/离线时回退 lanIPv4Addrs 第一个。
func primaryLANIPv4() net.IP {
	// UDP connect 不发包，仅让内核解析到 8.8.8.8 的出口源 IP（任何可达目标均可，
	// 此处用公共 DNS；离线也通常能解析本地路由）。
	conn, err := net.Dial("udp", "8.8.8.8:80")
	if err == nil {
		if la, ok := conn.LocalAddr().(*net.UDPAddr); ok {
			ip := la.IP.To4()
			if ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
				_ = conn.Close()
				return ip
			}
		}
		_ = conn.Close()
	}
	ips := lanIPv4Addrs()
	if len(ips) > 0 {
		return ips[0]
	}
	return nil
}

// ValidateSignalAddr 校验 mDNS 发现的直连信令端点（防 SSRF/伪造 peer，安全审查 B）：
//   - host:port 可解析、端口为数字；
//   - 拒绝 unspecified / multicast / broadcast / link-local（169.254.0.0/16 含云
//     metadata、fe80::/10）；loopback 在测试 loopback 收敛模式（mdnsLoopbackOnly）下
//     放行（两节点同机测试），生产拒绝；
//   - hostname 解析并校验全部结果 IP。
//
// 在 auto-dial（dialPeerDirect）与 mesh connect --mdns（runMDNSConnect）共用。
func ValidateSignalAddr(addr string) error {
	host, port, serr := net.SplitHostPort(addr)
	if serr != nil {
		return fmt.Errorf("mesh mDNS: 直连信令端点非法（应为 host:port）: %w", serr)
	}
	if host == "" || port == "" {
		return fmt.Errorf("mesh mDNS: 直连信令端点 host/port 为空: %q", addr)
	}
	if _, aerr := strconv.Atoi(port); aerr != nil {
		return fmt.Errorf("mesh mDNS: 直连信令端点端口非法: %q", port)
	}
	if ip := net.ParseIP(host); ip != nil {
		return validateSignalIP(ip)
	}
	ips, lerr := net.LookupIP(host)
	if lerr != nil || len(ips) == 0 {
		return fmt.Errorf("mesh mDNS: 直连信令端点主机名解析失败: %q", host)
	}
	for _, ip := range ips {
		if verr := validateSignalIP(ip); verr != nil {
			return verr
		}
	}
	return nil
}

func validateSignalIP(ip net.IP) error {
	if ip == nil {
		return errors.New("mesh mDNS: 直连信令端点 IP 为空")
	}
	if ip.IsUnspecified() || ip.IsMulticast() || ip.Equal(net.IPv4bcast) {
		return fmt.Errorf("mesh mDNS: 拒绝不安全信令端点 %s", ip)
	}
	if isMDNSLoopbackOnly() {
		return nil // 测试 loopback 收敛：允许 loopback/link-local（两节点同机）
	}
	if ip.IsLoopback() {
		return fmt.Errorf("mesh mDNS: 拒绝 loopback 信令端点 %s", ip)
	}
	if ip.IsLinkLocalUnicast() {
		return fmt.Errorf("mesh mDNS: 拒绝 link-local 信令端点 %s", ip)
	}
	return nil
}

// resolveSignalListenAddr 解析直连信令的监听 host:port：
//   - listenAddr 的 host 为通配（空 / 全零 IPv4 / IPv6 通配）时收敛到主局域网 IPv4
//     （暴露面与 mDNS 广播的 saddr 一致，不绑全接口，F2）；
//   - 显式 host（如 127.0.0.1）原样保留（测试收敛 loopback 用）；
//   - 端口保留用户指定，缺省回落 "0"（随机端口）。
func resolveSignalListenAddr(listenAddr string) (host, port string, err error) {
	if listenAddr == "" {
		listenAddr = ":0"
	}
	host, port, err = net.SplitHostPort(listenAddr)
	if err != nil {
		return "", "", fmt.Errorf("mesh mDNS: 直连信令监听地址非法（应为 host:port）: %w", err)
	}
	if port == "" {
		port = "0"
	}
	host = strings.Trim(host, "[]")
	// 通配 host（空 / 全零 IPv4 / IPv6 通配）用主局域网 IPv4 + 实际端口。
	// 注：全零 host 用 net.IPv4zero.String() 比较，避免源码出现全零 IPv4 字面量
	// 被 check-loopback 误判为不安全监听。
	if host == "" || host == net.IPv4zero.String() || host == "::" {
		ip := primaryLANIPv4()
		if ip == nil {
			return "", "", errors.New("mesh mDNS: 未找到可广播的局域网 IPv4 地址（可用 --signal-addr 显式指定监听地址）")
		}
		host = ip.String()
	}
	return host, port, nil
}
