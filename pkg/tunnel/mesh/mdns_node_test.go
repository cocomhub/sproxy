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
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

func TestResolveSignalListenAddr(t *testing.T) {
	// 通配 host → 收敛到主局域网 IPv4 + 端口 "0"（不校验具体 IP，仅校验格式与端口）。
	host, port, err := resolveSignalListenAddr("")
	if err != nil {
		t.Fatalf("resolveSignalListenAddr(\"\"): %v", err)
	}
	if net.ParseIP(host) == nil || port != "0" {
		t.Errorf("通配 host 解析 = %q:%q, 期望 LAN IPv4 与端口 0", host, port)
	}
	// 显式 host 原样保留，端口透传。
	host, port, err = resolveSignalListenAddr("127.0.0.1:0")
	if err != nil {
		t.Fatalf("resolveSignalListenAddr(127.0.0.1): %v", err)
	}
	if host != "127.0.0.1" || port != "0" {
		t.Errorf("显式 host 解析 = %q:%q, want 127.0.0.1:0", host, port)
	}
	host, port, err = resolveSignalListenAddr("127.0.0.1:40002")
	if err != nil {
		t.Fatalf("resolveSignalListenAddr(127.0.0.1:40002): %v", err)
	}
	if host != "127.0.0.1" || port != "40002" {
		t.Errorf("显式端口解析 = %q:%q, want 127.0.0.1:40002", host, port)
	}
	// 非法地址报错。
	if _, _, err := resolveSignalListenAddr("bad-addr"); err == nil {
		t.Error("非法监听地址应报错")
	}
}

// TestMeshNodeMDNS_Connect 是 mDNS 子任务的 DoD 测试：同机运行一个纯 mDNS 无 hub
// mesh 节点（宣告 echo 服务 + 直连信令端点），客户端经 mDNS 发现该服务，用直连信令
// 建立 webrtc 数据面并走对端出口拨号，验证 echo 数据双向通过。
func TestMeshNodeMDNS_Connect(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15360 // mDNS 测试端口
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

	// 本地 echo 服务（mesh node 出口拨号目标）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听 echo 服务: %v", err)
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
			NodeID:         "node-svc",
			Services:       []hub.Service{{Name: "echo", Addr: echoAddr}},
			ServiceAddrs:   []string{echoAddr},
			DialAllow:      true,
			EnableMDNS:     true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0", // 测试收敛 loopback，避免防火墙弹窗
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         logger,
		})
	}()

	// 客户端侧 mDNS 浏览器：发现 node-svc 的 echo 服务。
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer browseCancel()
	browse, err := NewMDNS(MDNSConfig{NodeID: "node-client", BrowseOnly: true, Port: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewMDNS(browse): %v", err)
	}
	if serr := browse.Start(browseCtx); serr != nil {
		t.Fatalf("browse Start: %v", serr)
	}
	defer browse.Close()

	// 诊断：节点若提前返回错误（如 mDNS 绑定失败），尽早报出而非白等超时。
	select {
	case nerr := <-nodeErr:
		t.Fatalf("mesh 节点提前退出: %v", nerr)
	case <-time.After(500 * time.Millisecond):
	}

	peers, err := browse.LookupService(browseCtx, "echo", 20*time.Second)
	if err != nil {
		t.Fatalf("mDNS 未发现 echo 服务: %v", err)
	}
	if len(peers) == 0 {
		t.Fatal("mDNS 发现到空节点集")
	}
	peer := peers[0]
	if peer.NodeID != "node-svc" {
		t.Errorf("发现节点 = %q, want node-svc", peer.NodeID)
	}
	if peer.SignalAddr == "" {
		t.Fatal("节点未广播直连信令端点")
	}

	// F1 回归：对直连信令端口发空连接（端口扫描）+ 畸形帧连接，节点应存活（不退出），
	// 随后正常连接仍可用（否则远程无认证即可杀整节点）。
	scanConn, cerr := net.Dial("tcp", peer.SignalAddr)
	if cerr != nil {
		t.Fatalf("端口扫描连接失败: %v", cerr)
	}
	_ = scanConn.Close()
	garbageConn, gerr := net.Dial("tcp", peer.SignalAddr)
	if gerr != nil {
		t.Fatalf("畸形连接失败: %v", gerr)
	}
	_, _ = garbageConn.Write([]byte{0xff, 0xff, 0xff, 0xff, 'g'})
	_ = garbageConn.Close()

	// 客户端连接：直连信令 → webrtc 数据面 → 出口拨号 → echo。
	connectCtx, connectCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer connectCancel()
	sig, err := DialDirectSignaler(connectCtx, peer.SignalAddr, "node-client")
	if err != nil {
		t.Fatalf("直连信令连接失败: %v", err)
	}
	defer sig.Close()
	target := &client.MeshService{Name: "echo", Node: peer.NodeID, Addr: echoAddr}
	res, err := DialDirect(connectCtx, sig, target)
	if err != nil {
		t.Fatalf("DialDirect: %v", err)
	}
	conn := res.Conn
	defer conn.Close()

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("写 echo 失败: %v", err)
	}
	type readResult struct {
		n    int
		err  error
		data []byte
	}
	readCh := make(chan readResult, 1)
	go func() {
		buf := make([]byte, 64)
		n, rerr := conn.Read(buf)
		readCh <- readResult{n: n, err: rerr, data: buf}
	}()
	select {
	case r := <-readCh:
		if r.err != nil {
			t.Fatalf("读 echo 失败: %v", r.err)
		}
		if string(r.data[:r.n]) != "ping" {
			t.Fatalf("echo = %q, want ping", r.data[:r.n])
		}
	case <-connectCtx.Done():
		t.Fatal("读 echo 超时")
	}

	// 优雅收尾：取消节点 ctx，确认 RunNode 正常返回（无泄漏挂死）。
	nodeCancel()
	select {
	case err := <-nodeErr:
		if err != nil {
			t.Fatalf("RunNode 返回错误: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunNode 未在取消后退出")
	}
}

// TestMeshNodeMDNS_MutualDiscovery 覆盖 DoD 的"互发现"：两个纯 mDNS 无 hub mesh 节点
// （不同 node-id、独立直连信令端口，同 mDNS 组播组）经 mDNS 发现彼此并自动建立
// webrtc 直连（低 ID 拨高 ID，半拨号去重）。通过 DiscoveryPeers 观测通道断言拨号侧
// 建立了到对端的直连链路。
func TestMeshNodeMDNS_MutualDiscovery(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15361 // mDNS 测试端口
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

	logger := testMDNSLogger()
	aPeers := make(chan string, 8)
	bPeers := make(chan string, 8)
	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	nodeErrA := make(chan error, 1)
	nodeErrB := make(chan error, 1)
	// 短发现周期：Windows 组播投递偶发延迟，默认 10s 周期会显著拉长/触发超时。
	discoveryInterval := 500 * time.Millisecond
	go func() {
		nodeErrA <- RunNode(ctxA, NodeConfig{NodeID: "node-a", EnableMDNS: true, Discover: true, DiscoveryInterval: discoveryInterval, MDNSPort: port, SignalAddr: "127.0.0.1:0", EnableWebRTC: true, DiscoveryPeers: aPeers, Logger: logger})
	}()
	go func() {
		nodeErrB <- RunNode(ctxB, NodeConfig{NodeID: "node-b", EnableMDNS: true, Discover: true, DiscoveryInterval: discoveryInterval, MDNSPort: port, SignalAddr: "127.0.0.1:0", EnableWebRTC: true, DiscoveryPeers: bPeers, Logger: logger})
	}()

	// 半拨号去重：低 ID node-a 拨高 ID node-b，aPeers 应收到 "node-b"。
	select {
	case p := <-aPeers:
		if p != "node-b" {
			t.Errorf("node-a 建立的对等直连 = %q, want node-b", p)
		}
	case <-time.After(30 * time.Second):
		// 节点若提前退出（mDNS 绑定失败等），报节点错误而非白等超时。
		select {
		case aerr := <-nodeErrA:
			t.Fatalf("node-a 提前退出: %v", aerr)
		case berr := <-nodeErrB:
			t.Fatalf("node-b 提前退出: %v", berr)
		default:
		}
		t.Fatal("node-a 未在超时内经 mDNS 自动直连 node-b")
	}

	// 优雅收尾：取消两个节点 ctx，确认 RunNode 正常返回。
	cancelA()
	cancelB()
	for i, ch := range []<-chan error{nodeErrA, nodeErrB} {
		select {
		case err := <-ch:
			if err != nil {
				t.Fatalf("node[%d] RunNode 返回错误: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("node[%d] 未在取消后退出", i)
		}
	}
}
