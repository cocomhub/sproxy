// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestMeshUDPMap_Bidirectional（DoD）：sclient udp map 的 mesh 路由核心——本地 UDP
// 数据报经 mesh（mux FrameDatagram）到出口节点，出口转发到远程 UDP echo，响应原路
// 回传本地（双向 UDP 转发确认）。
func TestMeshUDPMap_Bidirectional(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback + mDNS 组播 loopback，避免防火墙弹窗。
	testMDNSLoopback(t)
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15380 // mDNS 测试端口
	probe, err := net.ListenMulticastUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP(mDNSIPv4), Port: port})
	if err != nil {
		t.Skipf("mDNS 组播不可用: %v", err)
	}
	probe.Close()

	// 出口节点本地 UDP echo 服务（出口转发目标）。
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("监听 UDP echo: %v", err)
	}
	defer udpEcho.Close()
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, rerr := udpEcho.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			_, _ = udpEcho.WriteToUDP(buf[:n], addr) // echo
		}
	}()
	udpEchoAddr := udpEcho.LocalAddr().String()

	logger := testMDNSLogger()
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	nodeErr := make(chan error, 1)
	go func() {
		nodeErr <- RunNode(nodeCtx, NodeConfig{
			NodeID:         "node-exit",
			DialAllow:      true,                  // UDP 映射与 TCP dial 同属出口模式
			ServiceAddrs:   []string{udpEchoAddr}, // 拨号策略放行 UDP 目标
			EnableMDNS:     true,
			MDNSOnly:       true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0",
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         logger,
		})
	}()

	// 客户端侧 mDNS 浏览：发现出口节点信令端点。
	browseCtx, browseCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer browseCancel()
	mdnsSrv, err := NewMDNS(MDNSConfig{NodeID: "node-udp", BrowseOnly: true, Port: port, Logger: logger})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	if serr := mdnsSrv.Start(browseCtx); serr != nil {
		t.Fatalf("mDNS start: %v", serr)
	}
	defer mdnsSrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	peer, perr := mdnsSrv.LookupPeer(ctx, "node-exit", 15*time.Second)
	if perr != nil {
		t.Fatalf("未发现出口节点: %v", perr)
	}
	if verr := ValidateSignalAddr(peer.SignalAddr); verr != nil {
		t.Fatalf("信令端点校验失败: %v", verr)
	}
	sig, serr := DialDirectSignaler(ctx, peer.SignalAddr, "node-udp")
	if serr != nil {
		t.Fatalf("直连信令失败: %v", serr)
	}
	defer sig.Close()

	// 建立 UDP 映射 mux + 控制流。
	m, control, oerr := OpenUDPMux(ctx, sig, "node-exit", udpEchoAddr)
	if oerr != nil {
		t.Fatalf("OpenUDPMux: %v", oerr)
	}
	defer m.Close()
	defer control.Close()

	// 本地 UDP 监听 + 出口响应回传。
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("监听本地 UDP: %v", err)
	}
	defer local.Close()
	var mu sync.Mutex
	var clientAddr *net.UDPAddr
	m.SetDatagramHandler(func(flowID uint32, data []byte) {
		mu.Lock()
		a := clientAddr
		mu.Unlock()
		if a != nil {
			_, _ = local.WriteToUDP(data, a)
		}
	})
	go func() {
		buf := make([]byte, 65535)
		for {
			n, addr, rerr := local.ReadFromUDP(buf)
			if rerr != nil {
				return
			}
			mu.Lock()
			clientAddr = addr
			mu.Unlock()
			_ = m.SendDatagram(0, buf[:n])
		}
	}()

	// 测试客户端：经本地 UDP 端口发送，等待 echo 回传（重试容忍出口 setup 时序）。
	testClient, err := net.DialUDP("udp", nil, local.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("连接本地 UDP: %v", err)
	}
	defer testClient.Close()
	payload := []byte("udp-bidirectional-hello")
	deadline := time.Now().Add(20 * time.Second)
	got := make([]byte, 256)
	for {
		if _, werr := testClient.Write(payload); werr != nil {
			t.Fatalf("写本地 UDP: %v", werr)
		}
		_ = testClient.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, rerr := testClient.Read(got)
		if rerr == nil && string(got[:n]) == string(payload) {
			break // 双向确认
		}
		if time.Now().After(deadline) {
			t.Fatalf("双向 UDP 转发未在超时内确认（最后错误: %v）", rerr)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
