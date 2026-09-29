// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/spf13/cobra"
)

// mdnsLookupTimeout 是 mesh connect --mdns 单次服务发现的等待窗口。
var mdnsLookupTimeout = 5 * time.Second

// runMDNSConnect 纯 mDNS 直连（mesh connect --mdns，不经 hub）：
// 经 mDNS 发现局域网内宣告 service 的 mesh node（mesh node --mdns 运行），用直连
// 信令建立 webrtc 数据面，走对端出口拨号。listenAddr 非空时为端口转发模式，否则
// 单次 stdin/stdout 模式。secret 是共享密钥（--mdns-secret，可为空 = 无认证）。
// virtualSubnet 是虚拟 IP 子网（--virtual-subnet，默认 CGNAT；mDNS 无 hub 模式用
// 确定性分配，S-1：虚拟 IP 目标经 AddVerified 校验后解析 node-id 直连）。
func runMDNSConnect(cmd *cobra.Command, service, listenAddr, nodeID, secret, virtualSubnet string, ios cli.IOStreams) error {
	if nodeID == "" {
		nodeID = iostream.LocalHostname("mesh-node")
	}
	vipSubnet, perr := netip.ParsePrefix(virtualSubnet)
	if perr != nil || !vipSubnet.Addr().Is4() {
		return fmt.Errorf("--virtual-subnet %q 非法（应为 IPv4 CIDR）", virtualSubnet)
	}
	vipSubnet = vipSubnet.Masked()
	alloc := mesh.NewDeterministicAllocator(vipSubnet)
	mdns, err := mesh.NewMDNS(mesh.MDNSConfig{NodeID: nodeID, BrowseOnly: true, Secret: secret})
	if err != nil {
		return fmt.Errorf("mDNS 初始化失败: %w", err)
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	if err := mdns.Start(ctx); err != nil {
		return fmt.Errorf("mDNS 启动失败: %w", err)
	}
	defer mdns.Close()

	// dial 每次连接重新解析 mDNS 目标（节点可能上下线/迁移），并建立新直连信令会话。
	dial := func(dctx context.Context) (net.Conn, error) {
		return mdnsDialOnce(dctx, mdns, service, nodeID, secret, vipSubnet, alloc)
	}

	if listenAddr != "" {
		return mdnsForwardListen(ctx, dial, listenAddr, service, ios)
	}
	return mdnsStdioOnce(ctx, dial, service, ios)
}

// mdnsDialOnce 单次 mDNS 拨号（每次连接重新解析 mDNS 目标——节点可能上下线/迁移，
// 并建立新直连信令会话）。拨号侧每次 dial 加载本端身份指纹（fp=）：配置了身份则
// offer 携带指纹供接受侧白名单校验（双层认证）；未配置则 fp 为空（向后兼容）。
// 虚拟 IP 寻址（S-1）：host ∈ 虚拟子网 → 从 mDNS peers 的 VirtualIP 表（AddVerified
// 校验与确定性分配一致）解析 node-id，DialDirect 到对端。
func mdnsDialOnce(dctx context.Context, mdns *mesh.MDNSServer, service, nodeID, secret string, vipSubnet netip.Prefix, alloc hub.Allocator) (net.Conn, error) {
	var fp string
	if id, lErr := clientfactory.LoadIdentityOptional(); lErr == nil && id != nil {
		fp = id.Fingerprint()
	}
	if host, _, herr := net.SplitHostPort(service); herr == nil {
		if vip, ok := mesh.ParseVirtualAddr(host); ok && mesh.IsVirtualAddr(vip, vipSubnet) {
			return dialMDNSVirtualIP(dctx, mdns, vip, vipSubnet, alloc, mdnsConnectParams{service: service, nodeID: nodeID, secret: secret, fp: fp})
		}
	}
	peers, lerr := mdns.LookupService(dctx, service, mdnsLookupTimeout)
	if lerr != nil {
		return nil, fmt.Errorf("mDNS 服务发现失败: %w", lerr)
	}
	if len(peers) == 0 {
		return nil, mesh.ErrMDNSServiceNotFound
	}
	return mdnsDialPeers(dctx, peers, service, nodeID, secret, fp)
}

// mdnsDialPeers 逐个尝试宣告该服务的 mDNS peers（首个信令/拨号失败继续下一个），
// 避免单一节点陈旧/不可达即失败。校验 mDNS 发现的信令端点（防 SSRF：拒绝
// loopback/link-local 等，安全审查 B/D）。
func mdnsDialPeers(ctx context.Context, peers []mesh.MDNSPeer, service, nodeID, secret, fp string) (net.Conn, error) {
	var lastErr error
	for _, peer := range peers {
		svcAddr := ""
		for _, s := range peer.Services {
			if s.Name == service {
				svcAddr = s.Addr
				break
			}
		}
		if peer.SignalAddr == "" || svcAddr == "" {
			lastErr = fmt.Errorf("节点 %s 未广播信令端点或服务地址", peer.NodeID)
			continue
		}
		if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
			lastErr = fmt.Errorf("节点 %s 信令端点非法（%s）: %v", peer.NodeID, peer.SignalAddr, verr)
			continue
		}
		sig, serr := mesh.DialDirectSignaler(ctx, peer.SignalAddr, nodeID)
		if serr != nil {
			lastErr = fmt.Errorf("直连信令失败（%s）: %w", peer.SignalAddr, serr)
			continue
		}
		sig.SetSecret(secret) // --mdns-secret：offer 携带 HMAC 签名
		if fp != "" {
			sig.SetFingerprint(fp) // 身份指纹：接受侧白名单校验（双层认证）
		}
		target := &client.MeshService{Name: service, Node: peer.NodeID, Addr: svcAddr}
		res, derr := mesh.DialDirect(ctx, sig, target)
		// 信令握手已完成、数据面独立；无论成败都释放信令连接（成功后仅剩数据通道）。
		_ = sig.Close()
		if derr != nil {
			lastErr = derr
			continue
		}
		return res.Conn, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, mesh.ErrMDNSServiceNotFound
}

// mdnsConnectParams 是 mDNS 直连的拨号参数（目标服务名 + 本端身份 + 共享密钥），
// 收敛 mdnsDialOnce/dialMDNSVirtualIP 间的重复传递（避免 S107 参数爆炸）。
type mdnsConnectParams struct {
	service string
	nodeID  string
	secret  string
	fp      string // 本端身份指纹（配置了 Identity 时非空；空 = 不携带）
}

// dialMDNSVirtualIP 经 mDNS peers 的 VirtualIP 表（AddVerified 校验确定性）解析
// 虚拟 IP → node-id，建立直连信令拨号到对端（Addr 保持 <vip>:<port>，出口策略
// 改写本机端口）。
func dialMDNSVirtualIP(ctx context.Context, mdns *mesh.MDNSServer, vip netip.Addr, subnet netip.Prefix, alloc hub.Allocator, cp mdnsConnectParams) (net.Conn, error) {
	// S-2：mDNS 组播宣告是周期性的，单次 connect 可能在对端首个含 vip= 的 TXT
	// 到达前发起——有界等待 peers 表填充（复用服务名路径的 mdnsLookupTimeout），
	// 超时才报错；否则单次 stdio 模式在对端刚启动时必然失败。
	node, nerr := mdnsVIPResolveNode(ctx, mdns, vip, subnet, alloc)
	if nerr != nil {
		return nil, nerr
	}
	peerSignal, serr := mdnsPeerSignalAddr(mdns, node)
	if serr != nil {
		return nil, serr
	}
	sig, derr := mesh.DialDirectSignaler(ctx, peerSignal, cp.nodeID)
	if derr != nil {
		return nil, fmt.Errorf("直连信令失败（%s）: %w", peerSignal, derr)
	}
	sig.SetSecret(cp.secret) // --mdns-secret：offer 携带 HMAC 签名
	if cp.fp != "" {
		sig.SetFingerprint(cp.fp) // 身份指纹：接受侧白名单校验（双层认证）
	}
	target := &client.MeshService{Name: cp.service, Node: node, Addr: cp.service}
	res, rerr := mesh.DialDirect(ctx, sig, target)
	_ = sig.Close()
	if rerr != nil {
		return nil, rerr
	}
	return res.Conn, nil
}

// mdnsVIPResolveNode 有界等待 mDNS peers 表填充（S-2）并解析虚拟 IP → node-id。
// 超时（mdnsLookupTimeout）报错；ctx 取消返回取消错误。
func mdnsVIPResolveNode(ctx context.Context, mdns *mesh.MDNSServer, vip netip.Addr, subnet netip.Prefix, alloc hub.Allocator) (string, error) {
	deadline := time.Now().Add(mdnsLookupTimeout)
	for {
		vt := mesh.NewVipTable(subnet)
		for _, p := range mdns.Peers() {
			if p.VirtualIP.IsValid() && p.SignalAddr != "" {
				vt.AddVerified(p.VirtualIP, p.NodeID, "", alloc)
			}
		}
		if n, ok := vt.NodeByAddr(vip); ok {
			return n, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("虚拟 IP %s 未在 mDNS 节点列表中找到（等待 %v 超时；请确认目标 mesh node --mdns 已运行并广播虚拟 IP）", vip, mdnsLookupTimeout)
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// mdnsPeerSignalAddr 从 peers 表找目标节点的信令端点并校验（防 SSRF，同服务名分支）。
func mdnsPeerSignalAddr(mdns *mesh.MDNSServer, node string) (string, error) {
	var peerSignal string
	for _, p := range mdns.Peers() {
		if p.NodeID == node {
			peerSignal = p.SignalAddr
			break
		}
	}
	if peerSignal == "" {
		return "", fmt.Errorf("节点 %s 未广播信令端点", node)
	}
	if verr := mesh.ValidateSignalAddr(peerSignal); verr != nil {
		return "", fmt.Errorf("节点 %s 信令端点非法（%s）: %v", node, peerSignal, verr)
	}
	return peerSignal, nil
}

// mdnsForwardListen 端口转发模式：每个入站连接独立走 mDNS 解析 + 直连拨号 + 泵送。
func mdnsForwardListen(ctx context.Context, dial func(context.Context) (net.Conn, error), listenAddr, service string, ios cli.IOStreams) error {
	listenAddr = iostream.NormalizeListenAddr(listenAddr)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("监听本地端口失败: %w", err)
	}
	defer ln.Close()
	ios.WriteOutLine("端口转发: %s ⇄ mesh(mDNS %s)", listenAddr, service)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		local, aerr := ln.Accept()
		if aerr != nil {
			if ctx.Err() != nil {
				return nil
			}
			return aerr
		}
		go func(c net.Conn) {
			defer c.Close()
			conn, derr := dial(ctx)
			if derr != nil {
				ios.WriteErrLine("建立 mesh 流失败: %v", derr)
				return
			}
			defer conn.Close()
			ios.WriteOutLine("连接已建立（mDNS 直连）: %s ⇄ %s", local.RemoteAddr().String(), service)
			iostream.Pump(c, conn, iostream.PumpGrace)
		}(local)
	}
}

// mdnsStdioOnce 单次 stdin/stdout 模式（方向区分通道：对端断开即结束；stdin EOF 后
// 传播半关闭等待对端剩余响应）。
func mdnsStdioOnce(ctx context.Context, dial func(context.Context) (net.Conn, error), service string, ios cli.IOStreams) error {
	conn, err := dial(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	ios.WriteOutLine("已连接（mDNS 直连）: stdin/stdout ⇄ %s (Ctrl+D / EOF 断开)", service)
	inDone := make(chan struct{})
	outDone := make(chan struct{})
	go func() {
		defer close(inDone)
		_, _ = io.Copy(conn, ios.In)
		iostream.CloseWrite(conn)
	}()
	go func() {
		defer close(outDone)
		_, _ = io.Copy(ios.Out, conn)
	}()
	select {
	case <-outDone:
	case <-inDone:
		<-outDone
	}
	return nil
}
