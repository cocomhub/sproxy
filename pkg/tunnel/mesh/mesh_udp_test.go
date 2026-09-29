// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestMeshUDPMap_Bidirectional（DoD）：sclient udp map 的 mesh 路由核心——本地 UDP
// 数据报经 mesh（mux FrameDatagram）到出口节点，出口转发到远程 UDP echo，响应原路
// 回传本地（双向 UDP 转发确认）。
func TestMeshUDPMap_Bidirectional(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗；mDNS 组播的 loopback
	// 收敛路径在 Windows 上绑单播回环地址（见 listenMDNSLoopback），实测不弹窗。
	testMDNSLoopback(t)
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(15 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	port := 15380 // mDNS 测试端口
	probeMDNSLoopback(t, port)

	// 出口节点本地 UDP echo 服务（出口转发目标）。
	udpEchoAddr := startMDNSUDPEchoService(t)

	startUDPExitNode(t, port, udpEchoAddr)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 本地 UDP 监听 + 出口响应回传（mDNS 发现 → 直连信令 → OpenUDPMux）。
	local := startUDPClientMux(t, ctx, port, udpEchoAddr)

	// 测试客户端：经本地 UDP 端口发送，等待 echo 回传（重试容忍出口 setup 时序）。
	mdnsUDPRoundTrip(t, local)
}

// startMDNSUDPEchoService 起出口节点本地 UDP echo（出口转发目标），返回其地址。
func startMDNSUDPEchoService(t *testing.T) string {
	t.Helper()
	udpEcho, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("监听 UDP echo: %v", err)
	}
	t.Cleanup(func() { _ = udpEcho.Close() })
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
	return udpEcho.LocalAddr().String()
}

// startUDPExitNode 起出口 mesh 节点（node-exit）：放行 UDP 出口目标 + 出口拨号。
func startUDPExitNode(t *testing.T, port int, udpAddr string) {
	t.Helper()
	nodeCtx := t.Context()
	go func() {
		_ = RunNode(nodeCtx, NodeConfig{
			NodeID:         "node-exit",
			DialAllow:      true,              // UDP 映射与 TCP dial 同属出口模式
			ServiceAddrs:   []string{udpAddr}, // 拨号策略放行 UDP 目标
			EnableMDNS:     true,
			MDNSOnly:       true,
			MDNSPort:       port,
			SignalAddr:     "127.0.0.1:0",
			EnableWebRTC:   true,
			DiscoveryPeers: make(chan string, 8),
			Logger:         testMDNSLogger(),
		})
	}()
}

// startUDPClientMux 建立客户端 UDP 映射：mDNS 发现出口 → 直连信令 → OpenUDPMux →
// 本地 UDP 监听 + 出口响应回传 handler。返回本地 UDP 监听 socket。
func startUDPClientMux(t *testing.T, ctx context.Context, port int, udpAddr string) *net.UDPConn {
	t.Helper()
	// 客户端侧 mDNS 浏览：发现出口节点信令端点。
	mdnsSrv, err := NewMDNS(MDNSConfig{NodeID: "node-udp", BrowseOnly: true, Port: port, Logger: testMDNSLogger()})
	if err != nil {
		t.Fatalf("NewMDNS: %v", err)
	}
	if serr := mdnsSrv.Start(ctx); serr != nil {
		t.Fatalf("mDNS start: %v", serr)
	}
	t.Cleanup(func() { _ = mdnsSrv.Close() })

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
	t.Cleanup(func() { _ = sig.Close() })

	// 建立 UDP 映射 mux + 控制流。
	m, control, oerr := OpenUDPMux(ctx, sig, "node-exit", udpAddr)
	if oerr != nil {
		t.Fatalf("OpenUDPMux: %v", oerr)
	}
	t.Cleanup(func() { _ = m.Close() })
	t.Cleanup(func() { _ = control.Close() })

	// 本地 UDP 监听 + 出口响应回传。
	local, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("监听本地 UDP: %v", err)
	}
	t.Cleanup(func() { _ = local.Close() })
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
	return local
}

// mdnsUDPRoundTrip 经本地 UDP 端口发送 payload，等待 echo 回传统一确认。
func mdnsUDPRoundTrip(t *testing.T, local *net.UDPConn) {
	t.Helper()
	testClient, err := net.DialUDP("udp", nil, local.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatalf("连接本地 UDP: %v", err)
	}
	defer testClient.Close()
	payload := []byte("udp-bidirectional-hello")
	got := make([]byte, 256)
	var lastReadErr error
	// 轮询至双向 UDP 转发确认（原 deadline + 退避循环 → 条件等待；超时信息带最后读错误）。
	testutil.WaitFor(t, 30*time.Second, func() bool {
		if _, werr := testClient.Write(payload); werr != nil {
			t.Fatalf("写本地 UDP: %v", werr)
		}
		_ = testClient.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, rerr := testClient.Read(got)
		if rerr == nil && string(got[:n]) == string(payload) {
			return true // 双向确认
		}
		lastReadErr = rerr
		return false
	}, func() string {
		return fmt.Sprintf("双向 UDP 转发未在超时内确认（最后读错误: %v）", lastReadErr)
	})
}
