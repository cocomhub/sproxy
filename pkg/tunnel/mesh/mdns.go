// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"golang.org/x/net/dns/dnsmessage"
)

// mDNS 局域网发现（DNS-SD over mDNS，RFC 6762/6763，纯标准库 + golang.org/x/net/dns/dnsmessage）。
//
// 每个 mesh node 在 `_sproxy-mesh._tcp.local.` 服务类型下广播自身：
//   - PTR：`_sproxy-mesh._tcp.local.` → `<instance>._sproxy-mesh._tcp.local.`
//   - TXT：`<instance>...` → `node=<node-id>`、`saddr=<ip:port>`（直连 webrtc 信令端点）、
//     `svc.<name>=<addr>`（每服务一条）
//   - A：`<instance>...` → 本节点 LAN IPv4（供对端获取源地址）
//
// 节点周期发送 unsolicited 宣告（无需先收到查询即可被发现，天然满足"同网段互发现"），
// 同时监听组播：接收其他节点的宣告/查询应答更新本地缓存（TTL 过期剔除），并对
// PTR/TXT/A 查询即时应答。
//
// 安全边界：mDNS 是局域网广播协议，天然信任同网段。本实现只做发现（node-id + 服务 +
// 信令端点），不携带任何密钥/凭据；后续 webrtc 直连信令与数据面仍走既有加密与身份
// 校验（数据面 p2p，信令端点仅暴露给同网段可组播触达的节点）。不把 mDNS 用于公网。

const (
	// mDNSIPv4 是 IPv4 mDNS 组播组地址（RFC 6762）。
	mDNSIPv4 = "224.0.0.251"
	// mDNSPort 是 mDNS 标准 UDP 端口。
	mDNSPort = 5353
	// mdnsServiceName 是本 mesh 使用的 DNS-SD 服务类型（trailing dot）。
	mdnsServiceName = "_sproxy-mesh._tcp.local."
	// mdnsTTL 是广告记录的 TTL 秒数（RFC 6762 推荐 120s）。
	mdnsTTL = 120
	// mdnsAnnounceInterval 是 unsolicited 宣告周期。
	mdnsAnnounceInterval = 3 * time.Second
	// mdnsBrowseInterval 是 PTR 查询周期（主动探测新节点上线）。
	mdnsBrowseInterval = 5 * time.Second
	// mdnsSweepInterval 是缓存过期清扫周期。
	mdnsSweepInterval = 5 * time.Second
	// mdnsCacheFlush 是 DNS 类字段的 cache-flush 位（RFC 6762 §10.2）。
	mdnsCacheFlush = 0x8000
)

// ErrMDNSServiceNotFound 表示在超时窗口内未通过 mDNS 发现指定服务。
var ErrMDNSServiceNotFound = errors.New("mdns: 局域网内未发现该 mesh 服务")

// MDNSConfig 是 mDNS 服务器配置。
type MDNSConfig struct {
	// NodeID 是本节点 node-id（必填；广播与自去重键）。
	NodeID string
	// SignalAddr 是直连 webrtc 信令端点 host:port（广播给对端供拨号；BrowseOnly 时忽略）。
	SignalAddr string
	// Services 是本节点宣告的 mesh 服务（mesh connect 据此发现）。
	Services []hub.Service
	// IPs 是广告进 A 记录的 LAN IPv4 地址（空则不发 A 记录）。
	IPs []net.IP
	// BrowseOnly 时只浏览不宣告（mesh connect 瞬态客户端用，不暴露自身）。
	BrowseOnly bool
	// Port 是组播端口（0 回落 mDNSPort；测试可覆盖避免占用标准 5353）。
	Port int
	// Logger 是会话日志（nil 用 slog.Default()）。
	Logger *slog.Logger
}

// MDNSPeer 是通过 mDNS 发现的 mesh 节点。
type MDNSPeer struct {
	// NodeID 是对端稳定 node-id。
	NodeID string
	// SignalAddr 是对端直连 webrtc 信令端点 host:port。
	SignalAddr string
	// Services 是对端宣告的 mesh 服务。
	Services []hub.Service
	// IPs 是对端广告的 LAN IPv4 地址。
	IPs []net.IP
}

// mdnsPeerCache 是一条已发现对端的缓存条目。
type mdnsPeerCache struct {
	peer      MDNSPeer
	instance  string
	expiresAt time.Time
}

// MDNSServer 是 mDNS 广播器 + 浏览器：加入组播组，周期宣告自身、周期查询服务、
// 解析收到的应答/宣告更新对端缓存（TTL 过期剔除）。
type MDNSServer struct {
	conf          MDNSConfig
	instance      string // 本节点实例名（DNS-SD instance，全限定名）
	instanceLabel string // 本节点实例首标签（自去重比较键）

	conn *net.UDPConn

	mu    sync.Mutex
	peers map[string]*mdnsPeerCache // key: 实例名

	closed    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
	logger    *slog.Logger
}

// NewMDNS 构造 mDNS 服务器（不绑定，Start 时才加入组播）。
func NewMDNS(conf MDNSConfig) (*MDNSServer, error) {
	if conf.NodeID == "" {
		return nil, errors.New("mdns: node-id 为空")
	}
	if !conf.BrowseOnly && conf.SignalAddr == "" {
		return nil, errors.New("mdns: 非 BrowseOnly 时必须提供直连信令端点 SignalAddr")
	}
	logger := conf.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &MDNSServer{
		conf:          conf,
		instance:      mdnsInstanceName(conf.NodeID),
		instanceLabel: mdnsInstanceLabel(conf.NodeID),
		peers:         map[string]*mdnsPeerCache{},
		closed:        make(chan struct{}),
		logger:        logger,
	}, nil
}

// Start 加入 mDNS 组播并启动读/宣告/查询/清扫循环，直到 ctx 取消或 Close。
func (s *MDNSServer) Start(ctx context.Context) error {
	port := s.conf.Port
	if port == 0 {
		port = mDNSPort
	}
	group := &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port}
	conn, err := net.ListenMulticastUDP("udp4", nil, group)
	if err != nil {
		return fmt.Errorf("mdns: 加入组播 %s 失败: %w", group, err)
	}
	// 组播回环（同机多实例互收）在主流平台默认开启；Go 1.26 起不再提供
	// UDPConn.SetMulticastLoopback，需按平台经 SyscallConn 调整（见 mdns_platform.go）。
	setMulticastLoopback(conn)
	_ = conn.SetReadBuffer(64 << 10)
	s.conn = conn

	s.wg.Add(1)
	go s.readLoop(ctx)
	if !s.conf.BrowseOnly {
		s.wg.Add(1)
		go s.announceLoop(ctx)
	}
	s.wg.Add(1)
	go s.browseLoop(ctx)
	s.wg.Add(1)
	go s.sweepLoop(ctx)
	return nil
}

// Close 关闭组播连接并等待循环退出（幂等）。
func (s *MDNSServer) Close() error {
	s.closeOnce.Do(func() {
		close(s.closed)
		if s.conn != nil {
			_ = s.conn.Close()
		}
	})
	s.wg.Wait()
	return nil
}

// Peers 返回当前已发现的非自身对端（按 node-id 排序，已跳过过期条目）。
func (s *MDNSServer) Peers() []MDNSPeer {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := make([]MDNSPeer, 0, len(s.peers))
	for _, cp := range s.peers {
		if cp.peer.NodeID == "" || now.After(cp.expiresAt) {
			continue
		}
		out = append(out, cp.peer)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out
}

// LookupService 在 timeout 窗口内等待并返回宣告指定服务的对端（mDNS 发现入口）。
// 未发现返回 ErrMDNSServiceNotFound。
func (s *MDNSServer) LookupService(ctx context.Context, service string, timeout time.Duration) ([]MDNSPeer, error) {
	if timeout <= 0 {
		timeout = mdnsAnnounceInterval * 2
	}
	deadline := time.Now().Add(timeout)
	for {
		matches := s.peersWithService(service)
		if len(matches) > 0 {
			return matches, nil
		}
		if time.Now().After(deadline) {
			return nil, ErrMDNSServiceNotFound
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (s *MDNSServer) peersWithService(service string) []MDNSPeer {
	var matches []MDNSPeer
	for _, p := range s.Peers() {
		for _, svc := range p.Services {
			if svc.Name == service {
				matches = append(matches, p)
				break
			}
		}
	}
	return matches
}

// ---- 循环 ---------------------------------------------------------------

func (s *MDNSServer) readLoop(ctx context.Context) {
	defer s.wg.Done()
	buf := make([]byte, 64<<10)
	for {
		n, _, err := s.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-s.closed:
				return
			default:
				continue
			}
		}
		s.handlePacket(ctx, buf[:n])
	}
}

func (s *MDNSServer) announceLoop(ctx context.Context) {
	defer s.wg.Done()
	s.sendAnnouncement() // 启动即宣告（新节点上线立即可发现）
	ticker := time.NewTicker(mdnsAnnounceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			s.sendAnnouncement()
		}
	}
}

func (s *MDNSServer) browseLoop(ctx context.Context) {
	defer s.wg.Done()
	s.sendQuery() // 启动即查询（尽早拉取已有节点）
	ticker := time.NewTicker(mdnsBrowseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			s.sendQuery()
		}
	}
}

func (s *MDNSServer) sweepLoop(ctx context.Context) {
	defer s.wg.Done()
	ticker := time.NewTicker(mdnsSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for inst, cp := range s.peers {
				if now.After(cp.expiresAt) {
					delete(s.peers, inst)
				}
			}
			s.mu.Unlock()
		}
	}
}

// ---- 报文处理 -------------------------------------------------------------

func (s *MDNSServer) handlePacket(ctx context.Context, buf []byte) {
	var p dnsmessage.Parser
	h, err := p.Start(buf)
	if err != nil {
		return
	}
	if h.Response {
		if h.Truncated {
			// 截断的应答（TC 位）：丢弃，避免以部分记录覆盖完整记录集
			// （本实现宣告包极小，截断概率低，但防御性处理）。
			return
		}
		s.handleResponse(&p)
		return
	}
	s.handleQuery(ctx, &p, h)
}

// handleResponse 解析其他节点发来的应答/宣告，更新对端缓存。
func (s *MDNSServer) handleResponse(p *dnsmessage.Parser) {
	// 跳过可能的查询回显（应答通常无 query，宣告必无）。
	for {
		if _, err := p.Question(); err != nil {
			if errors.Is(err, dnsmessage.ErrSectionDone) {
				break
			}
			return
		}
	}
	answers, err := p.AllAnswers()
	if err != nil {
		return
	}
	for _, a := range answers {
		s.applyAnswer(a)
	}
}

func (s *MDNSServer) applyAnswer(res dnsmessage.Resource) {
	switch b := res.Body.(type) {
	case *dnsmessage.PTRResource:
		// 服务 PTR → 实例：确认实例属于本服务类型，预建缓存条目（供先于 TXT 到达的
		// A 记录归属）。
		target := b.PTR.String()
		if strings.HasSuffix(target, "."+mdnsServiceName) {
			inst := strings.TrimSuffix(target, "."+mdnsServiceName)
			if inst != s.instanceLabel {
				s.mu.Lock()
				s.getOrCreatePeerLocked(inst)
				s.mu.Unlock()
			}
		}
	case *dnsmessage.TXTResource:
		name := res.Header.Name.String()
		if !strings.HasSuffix(name, "."+mdnsServiceName) {
			return
		}
		inst := strings.TrimSuffix(name, "."+mdnsServiceName)
		if inst == s.instanceLabel {
			return // 自己的宣告忽略
		}
		s.applyTXT(inst, b.TXT, res.Header.TTL)
	case *dnsmessage.AResource:
		name := res.Header.Name.String()
		if !strings.HasSuffix(name, "."+mdnsServiceName) {
			return
		}
		inst := strings.TrimSuffix(name, "."+mdnsServiceName)
		if inst == s.instanceLabel {
			return
		}
		s.applyA(inst, b.A, res.Header.TTL)
	}
}

// applyTXT 解析一条实例的 TXT 记录，填充 node-id / 信令端点 / 服务列表。
func (s *MDNSServer) applyTXT(inst string, txt []string, ttl uint32) {
	var nodeID, signalAddr string
	var services []hub.Service
	for _, str := range txt {
		k, v, ok := strings.Cut(str, "=")
		if !ok {
			continue
		}
		switch k {
		case "node":
			nodeID = unescapeMDNS(v)
		case "saddr":
			signalAddr = unescapeMDNS(v)
		default:
			if rest, ok2 := strings.CutPrefix(k, "svc."); ok2 {
				svcName := unescapeMDNS(rest)
				svcAddr := unescapeMDNS(v)
				if svcName != "" && svcAddr != "" {
					services = append(services, hub.Service{Name: svcName, Addr: svcAddr})
				}
			}
		}
	}
	if nodeID == "" {
		return // 非本 mesh 的 TXT（缺 node 标识），忽略
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.getOrCreatePeerLocked(inst)
	cp.peer.NodeID = nodeID
	if signalAddr != "" {
		cp.peer.SignalAddr = signalAddr
	}
	// 无条件替换服务列表：本实例的 TXT 记录在单条宣告中总是完整集合，替换保证
	// 节点删服务/换服务后对端缓存立即反映（否则陈旧服务残留到 TTL 过期）。
	cp.peer.Services = services
	s.setTTL(cp, ttl)
}

func (s *MDNSServer) applyA(inst string, a [4]byte, ttl uint32) {
	ip := net.IP(append([]byte(nil), a[:]...))
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := s.getOrCreatePeerLocked(inst)
	for _, e := range cp.peer.IPs {
		if e.Equal(ip) {
			return
		}
	}
	cp.peer.IPs = append(cp.peer.IPs, ip)
	s.setTTL(cp, ttl)
}

// handleQuery 响应 PTR/TXT/A 查询（BrowseOnly 不响应，避免瞬态客户端暴露自身）。
func (s *MDNSServer) handleQuery(ctx context.Context, p *dnsmessage.Parser, h dnsmessage.Header) {
	if s.conf.BrowseOnly {
		return
	}
	var questions []dnsmessage.Question
	for {
		q, err := p.Question()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return
		}
		questions = append(questions, q)
	}
	for _, q := range questions {
		qname := q.Name.String()
		relevant := (q.Type == dnsmessage.TypePTR && qname == mdnsServiceName) ||
			((q.Type == dnsmessage.TypeTXT || q.Type == dnsmessage.TypeA) && qname == s.instance)
		if relevant {
			s.sendAnswerResponse(ctx, h.ID, q)
			return
		}
	}
}

// ---- 报文构造 -------------------------------------------------------------

// appendRecords 把本节点全部记录（PTR + TXT + A）追加到应答。
func (s *MDNSServer) appendRecords(b *dnsmessage.Builder) {
	inst := s.instance
	flushClass := dnsmessage.Class(dnsmessage.ClassINET | mdnsCacheFlush)
	_ = b.PTRResource(dnsmessage.ResourceHeader{
		Name: dnsmessage.MustNewName(mdnsServiceName), Type: dnsmessage.TypePTR,
		Class: flushClass, TTL: mdnsTTL,
	}, dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(inst)})
	_ = b.TXTResource(dnsmessage.ResourceHeader{
		Name: dnsmessage.MustNewName(inst), Type: dnsmessage.TypeTXT,
		Class: flushClass, TTL: mdnsTTL,
	}, dnsmessage.TXTResource{TXT: s.txtPairs()})
	for _, ip := range s.conf.IPs {
		ip4 := ip.To4()
		if ip4 == nil {
			continue
		}
		var a [4]byte
		copy(a[:], ip4)
		_ = b.AResource(dnsmessage.ResourceHeader{
			Name: dnsmessage.MustNewName(inst), Type: dnsmessage.TypeA,
			Class: flushClass, TTL: mdnsTTL,
		}, dnsmessage.AResource{A: a})
	}
}

func (s *MDNSServer) sendAnnouncement() {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeSuccess})
	_ = b.StartAnswers()
	s.appendRecords(&b)
	msg, err := b.Finish()
	if err != nil {
		return
	}
	s.send(msg)
}

func (s *MDNSServer) sendQuery() {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{
		Name:  dnsmessage.MustNewName(mdnsServiceName),
		Type:  dnsmessage.TypePTR,
		Class: dnsmessage.ClassINET,
	})
	msg, err := b.Finish()
	if err != nil {
		return
	}
	s.send(msg)
}

func (s *MDNSServer) sendAnswerResponse(ctx context.Context, id uint16, q dnsmessage.Question) {
	if err := ctx.Err(); err != nil {
		return
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true, RCode: dnsmessage.RCodeSuccess})
	_ = b.StartQuestions()
	_ = b.Question(q)
	_ = b.StartAnswers()
	s.appendRecords(&b)
	msg, err := b.Finish()
	if err != nil {
		return
	}
	// 查询可能来自单播（QU 位）；mDNS 应答统一发组播即可，简单可靠。
	s.send(msg)
}

func (s *MDNSServer) send(msg []byte) {
	if s.conn == nil {
		return
	}
	port := s.conf.Port
	if port == 0 {
		port = mDNSPort
	}
	dst := &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port}
	if _, err := s.conn.WriteToUDP(msg, dst); err != nil {
		s.logger.Debug("mdns: 发送组播失败", "error", err)
	}
}

// ---- TXT 编解码 -------------------------------------------------------------

func (s *MDNSServer) txtPairs() []string {
	pairs := []string{"node=" + escapeMDNS(s.conf.NodeID)}
	if s.conf.SignalAddr != "" {
		pairs = append(pairs, "saddr="+escapeMDNS(s.conf.SignalAddr))
	}
	for _, svc := range s.conf.Services {
		pairs = append(pairs, "svc."+escapeMDNS(svc.Name)+"="+escapeMDNS(svc.Addr))
	}
	return pairs
}

// escapeMDNS / unescapeMDNS 用 URL 编码保证 node-id/服务名/地址中任意字节
// （含空格、`=`、非 ASCII）在 TXT 字符串中安全往返。
func escapeMDNS(s string) string { return url.QueryEscape(s) }

func unescapeMDNS(s string) string {
	if v, err := url.QueryUnescape(s); err == nil {
		return v
	}
	return s
}

// ---- 实例名 -------------------------------------------------------------

// mdnsInstanceLabel 把 node-id 收敛为合法 DNS 标签（[a-zA-Z0-9_-]，其余替换为 `-`，
// ≤63 字节；空回落 "mesh-node"）。实例名仅供 mDNS 唯一寻址；真实 node-id 经 TXT 传输。
func mdnsInstanceLabel(nodeID string) string {
	var b strings.Builder
	for _, r := range nodeID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	s := b.String()
	if s == "" {
		s = "mesh-node"
	}
	if len(s) > 63 {
		s = s[:63]
	}
	return s
}

func mdnsInstanceName(nodeID string) string {
	return mdnsInstanceLabel(nodeID) + "." + mdnsServiceName
}

// ---- 缓存辅助 -------------------------------------------------------------

func (s *MDNSServer) getOrCreatePeerLocked(inst string) *mdnsPeerCache {
	cp, ok := s.peers[inst]
	if !ok {
		cp = &mdnsPeerCache{instance: inst}
		s.peers[inst] = cp
	}
	return cp
}

// setTTL 以"不缩短现有过期时间"的方式刷新 TTL（记录重复到达时以最长存活为准）。
func (s *MDNSServer) setTTL(cp *mdnsPeerCache, ttl uint32) {
	if ttl == 0 {
		return
	}
	candidate := time.Now().Add(time.Duration(ttl) * time.Second)
	if cp.expiresAt.IsZero() || candidate.After(cp.expiresAt) {
		cp.expiresAt = candidate
	}
}
