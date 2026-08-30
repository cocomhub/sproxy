// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/socks5"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
	"golang.org/x/net/proxy"
)

// TestMeshSocks5_Exit（DoD）：sclient socks 的 mesh 路由核心——SOCKS5 服务器用
// mDNS 直连信令路由到出口节点（node-exit），CONNECT 目标写 dial 帧由出口出站拨号。
// 官方 x/net/proxy SOCKS5 客户端经本代理 CONNECT 到出口的 echo 服务，数据双向往返。
//
// SSRF 边界（安全审查）：CONNECT 到出口未宣告的内网/loopback 目标应被出口 dial
// 策略拒绝（防代理被用作任意内网扫描）。
func TestMeshSocks5_Exit(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback + mDNS 组播 loopback，避免防火墙弹窗。
	testMDNSLoopback(t)
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15370 // mDNS 测试端口
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

	// 出口节点本地 echo 服务（出口 dial 放行目标）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 echo: %v", err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cc net.Conn) {
				defer cc.Close()
				_, _ = io.Copy(cc, cc)
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	logger := testMDNSLogger()
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	nodeErr := make(chan error, 1)
	go func() {
		nodeErr <- RunNode(nodeCtx, NodeConfig{
			NodeID:         "node-exit",
			Services:       []hub.Service{{Name: "echo", Addr: echoAddr}},
			ServiceAddrs:   []string{echoAddr},
			DialAllow:      true,
			EnableMDNS:     true,
			MDNSOnly:       true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0",
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         logger,
		})
	}()

	// SOCKS5 代理侧 mDNS 浏览：发现出口节点信令端点。
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer browseCancel()
	mdnsSrv, err := NewMDNS(MDNSConfig{NodeID: "node-socks", BrowseOnly: true, Port: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	if serr := mdnsSrv.Start(browseCtx); serr != nil {
		t.Fatalf("mDNS start: %v", serr)
	}
	defer mdnsSrv.Close()

	// mesh 路由 Dial：CONNECT 目标经 mDNS 直连信令到出口节点，写 dial 帧由出口拨号。
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		peer, perr := mdnsSrv.LookupPeer(ctx, "node-exit", 15*time.Second)
		if perr != nil {
			return nil, perr
		}
		if verr := ValidateSignalAddr(peer.SignalAddr); verr != nil {
			return nil, verr
		}
		sig, serr := DialDirectSignaler(ctx, peer.SignalAddr, "node-socks")
		if serr != nil {
			return nil, serr
		}
		target := &client.MeshService{Name: "socks", Node: "node-exit", Addr: addr}
		res, derr := DialDirect(ctx, sig, target)
		_ = sig.Close()
		if derr != nil {
			return nil, derr
		}
		return res.Conn, nil
	}

	socksLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 socks: %v", err)
	}
	defer socksLn.Close()
	ss := socks5.New(socks5.Config{Dial: dial, Logger: logger})
	socksCtx, socksCancel := context.WithCancel(context.Background())
	defer socksCancel()
	go func() { _ = ss.Serve(socksCtx, socksLn) }()

	// 官方 SOCKS5 客户端经代理 CONNECT 到出口 echo，数据往返。
	proxyDialer, err := proxy.SOCKS5("tcp", socksLn.Addr().String(), nil, nil)
	if err != nil {
		t.Fatalf("proxy.SOCKS5: %v", err)
	}
	conn, err := proxyDialer.Dial("tcp", echoAddr)
	if err != nil {
		t.Fatalf("经 mesh SOCKS5 CONNECT 到 echo 失败: %v", err)
	}
	defer conn.Close()
	if _, werr := conn.Write([]byte("ping-socks-mesh")); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	if serr := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); serr != nil {
		t.Fatal(serr)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("读 echo 失败: %v", err)
	}
	if string(buf[:n]) != "ping-socks-mesh" {
		t.Fatalf("echo = %q, want ping-socks-mesh", buf[:n])
	}

	// SSRF 边界（安全审查）：CONNECT 到出口未宣告的内网/loopback 目标应被出口
	// dial 策略拒绝（NewServiceDialPolicy 拒绝 loopback，除非在 ServiceAddrs）。
	// SOCKS5 握手先回「成功」（mesh relay 无结果帧，代理无法预知出口拨号结果），
	// 但出口拒绝拨号 → 流立即关闭，无数据可达（目标从未建立）。
	conn2, err := proxyDialer.Dial("tcp", "127.0.0.1:1")
	if err == nil {
		defer conn2.Close()
		_ = conn2.SetReadDeadline(time.Now().Add(5 * time.Second))
		if _, rerr := conn2.Read(make([]byte, 1)); rerr == nil {
			t.Fatal("经 mesh SOCKS5 不应能到达出口未放行的内网目标（SSRF 边界）")
		}
	}
}
