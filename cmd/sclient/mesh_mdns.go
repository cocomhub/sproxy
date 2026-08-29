// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
	"github.com/spf13/cobra"
)

// mdnsLookupTimeout 是 mesh connect --mdns 单次服务发现的等待窗口。
var mdnsLookupTimeout = 5 * time.Second

// runMDNSConnect 纯 mDNS 直连（mesh connect --mdns，不经 hub）：
// 经 mDNS 发现局域网内宣告 service 的 mesh node（mesh node --mdns 运行），用直连
// 信令建立 webrtc 数据面，走对端出口拨号。listenAddr 非空时为端口转发模式，否则
// 单次 stdin/stdout 模式。
func runMDNSConnect(cmd *cobra.Command, service, listenAddr, nodeID string, ios cli.IOStreams) error {
	if nodeID == "" {
		nodeID = iostream.LocalHostname("mesh-node")
	}
	mdns, err := mesh.NewMDNS(mesh.MDNSConfig{NodeID: nodeID, BrowseOnly: true})
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
			sig, serr := mesh.DialDirectSignaler(dctx, peer.SignalAddr, nodeID)
			if serr != nil {
				lastErr = fmt.Errorf("直连信令失败（%s）: %w", peer.SignalAddr, serr)
				continue
			}
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
