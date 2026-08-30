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
	// 多个节点宣告同一服务时逐个尝试（首个信令/拨号失败继续下一个），避免单一节点
	// 陈旧/不可达即失败。
	dial := func(dctx context.Context) (net.Conn, error) {
		// 虚拟 IP 寻址（S-1）：host ∈ 虚拟子网 → 从 mDNS peers 的 VirtualIP 表
		// （AddVerified 校验与确定性分配一致）解析 node-id，DialDirect 到对端。
		if host, _, herr := net.SplitHostPort(service); herr == nil {
			if vip, ok := mesh.ParseVirtualAddr(host); ok && mesh.IsVirtualAddr(vip, vipSubnet) {
				return dialMDNSVirtualIP(dctx, mdns, vip, service, vipSubnet, alloc, nodeID, secret)
			}
		}
		peers, lerr := mdns.LookupService(dctx, service, mdnsLookupTimeout)
		if lerr != nil {
			return nil, fmt.Errorf("mDNS 服务发现失败: %w", lerr)
		}
		if len(peers) == 0 {
			return nil, mesh.ErrMDNSServiceNotFound
		}
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
			// 校验 mDNS 发现的信令端点（防 SSRF：拒绝 loopback/link-local 等，
			// 安全审查 B/D）。
			if verr := mesh.ValidateSignalAddr(peer.SignalAddr); verr != nil {
				lastErr = fmt.Errorf("节点 %s 信令端点非法（%s）: %v", peer.NodeID, peer.SignalAddr, verr)
				continue
			}
			sig, serr := mesh.DialDirectSignaler(dctx, peer.SignalAddr, nodeID)
			if serr != nil {
				lastErr = fmt.Errorf("直连信令失败（%s）: %w", peer.SignalAddr, serr)
				continue
			}
			sig.SetSecret(secret) // --mdns-secret：offer 携带 HMAC 签名
			target := &client.MeshService{Name: service, Node: peer.NodeID, Addr: svcAddr}
			res, derr := mesh.DialDirect(dctx, sig, target)
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

	if listenAddr != "" {
		return mdnsForwardListen(ctx, dial, listenAddr, service, ios)
	}
	return mdnsStdioOnce(ctx, dial, service, ios)
}

// dialMDNSVirtualIP 经 mDNS peers 的 VirtualIP 表（AddVerified 校验确定性）解析
// 虚拟 IP → node-id，建立直连信令拨号到对端（Addr 保持 <vip>:<port>，出口策略
// 改写本机端口）。
func dialMDNSVirtualIP(ctx context.Context, mdns *mesh.MDNSServer, vip netip.Addr, service string, subnet netip.Prefix, alloc hub.Allocator, nodeID, secret string) (net.Conn, error) {
	vt := mesh.NewVipTable(subnet)
	for _, p := range mdns.Peers() {
		if p.VirtualIP.IsValid() && p.SignalAddr != "" {
			vt.AddVerified(p.VirtualIP, p.NodeID, "", alloc)
		}
	}
	node, ok := vt.NodeByAddr(vip)
	if !ok {
		return nil, fmt.Errorf("虚拟 IP %s 未在 mDNS 节点列表中找到对应节点（请确认目标 mesh node --mdns 已运行且已广播虚拟 IP）", vip)
	}
	// 找对端信令端点并校验（防 SSRF，同服务名分支）。
	var peerSignal string
	for _, p := range mdns.Peers() {
		if p.NodeID == node {
			peerSignal = p.SignalAddr
			break
		}
	}
	if peerSignal == "" {
		return nil, fmt.Errorf("节点 %s 未广播信令端点", node)
	}
	if verr := mesh.ValidateSignalAddr(peerSignal); verr != nil {
		return nil, fmt.Errorf("节点 %s 信令端点非法（%s）: %v", node, peerSignal, verr)
	}
	sig, serr := mesh.DialDirectSignaler(ctx, peerSignal, nodeID)
	if serr != nil {
		return nil, fmt.Errorf("直连信令失败（%s）: %w", peerSignal, serr)
	}
	sig.SetSecret(secret) // --mdns-secret：offer 携带 HMAC 签名
	target := &client.MeshService{Name: service, Node: node, Addr: service}
	res, derr := mesh.DialDirect(ctx, sig, target)
	_ = sig.Close()
	if derr != nil {
		return nil, derr
	}
	return res.Conn, nil
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
