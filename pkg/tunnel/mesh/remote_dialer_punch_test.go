// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

// remote_dialer_punch_test.go 是 **S4a** 的**真打洞**用例：进程内起直连信令服务器 + 真 mux +
// 真 relay 出口拨号（对端 `relay.Serve`），拨号侧用 `DialWebRTC` 打通 WebRTC 数据通道并命中
// 本机 TCP 假服务。
//
// 为什么重要：CI 无公网/无 STUN ⇒ 之前只能覆盖「回落中继」；本用例用 `webrtctest`（UDP 候选收敛
// 到 loopback）+ `webrtc.SetHostOnly`（测试逃生舱）把**真数据通道握手**纳入必检项。
// 前提（本片补齐）：`DialWebRTC`/`Dial`/`RemoteDialerConfig` 的信令入参放宽为 `webrtc.Signaler`
// 接口——原先固定 `*hub.HubSignaler`，导致无 hub 的进程内真打洞无法测试。

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// startEchoService 起一个本机 TCP echo 服务（充当对端要拨达的「服务」）。
func startEchoService(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("监听假服务失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(conn net.Conn) {
				defer func() { _ = conn.Close() }()
				_, _ = io.Copy(conn, conn)
			}(c)
		}
	}()
	return ln.Addr().String()
}

// TestDialWebRTC_RealPunchAndDialOut 真打洞 → 对端 relay 出口拨号 → 命中本机假服务（echo 往返）。
func TestDialWebRTC_RealPunchAndDialOut(t *testing.T) {
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	serviceAddr := startEchoService(t)

	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go srv.Serve(ctx)

	const peerID = "node-listener"
	const localID = "node-dialer"

	// 对端：真握手（listener 侧）→ mux → relay.Serve（出口拨号到 target.Addr）。
	peerErr := make(chan error, 1)
	go func() {
		conn, lErr := webrtc.ListenWithSignalerOptsCtx(ctx, peerID, srv.NewSignaler(), nil)
		if lErr != nil {
			peerErr <- lErr
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer func() { _ = m.Close() }()
		// localAddr 传空：出口地址完全由拨号帧决定。
		// DialPolicy 用生产同款（NewServiceDialPolicy：**精确放行本节点宣告的服务地址**，
		// 其余回落公网/白名单）——默认策略只允许公网目标，会拒掉本用例的 loopback 假服务。
		peerErr <- relay.Serve(ctx, m, "", true, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{serviceAddr})})
	}()

	// 拨号侧信令：直连信令（无 hub），与 CLI 的 mDNS 直连路径同构。
	dsig, err := DialDirectSignaler(ctx, srv.Addr().String(), localID)
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer func() { _ = dsig.Close() }()

	conn, err := DialWebRTC(ctx, dsig, &client.MeshService{Node: peerID, Addr: serviceAddr}, nil, nil)
	if err != nil {
		t.Fatalf("DialWebRTC（真打洞）失败: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// echo 往返：证明数据面真的通到本机假服务。
	if _, werr := conn.Write([]byte("ping-through-webrtc")); werr != nil {
		t.Fatalf("写: %v", werr)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, rErr := conn.Read(buf)
	if rErr != nil {
		t.Fatalf("读: %v", rErr)
	}
	if got := string(buf[:n]); got != "ping-through-webrtc" {
		t.Fatalf("echo 内容=%q want %q", got, "ping-through-webrtc")
	}
}

// TestRemoteDialer_RealPunchThroughConfig 钉住拨号器**用配置里的 Signaler 走真打洞**：
// 打洞成功后返回可用连接，且**不触碰中继**（Relay fallback 开关不起作用）。
func TestRemoteDialer_RealPunchThroughConfig(t *testing.T) {
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	serviceAddr := startEchoService(t)
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	go srv.Serve(ctx)

	const peerID = "node-listener"
	go func() {
		conn, lErr := webrtc.ListenWithSignalerOptsCtx(ctx, peerID, srv.NewSignaler(), nil)
		if lErr != nil {
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer func() { _ = m.Close() }()
		_ = relay.Serve(ctx, m, "", true, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{serviceAddr})})
	}()

	dsig, err := DialDirectSignaler(ctx, srv.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer func() { _ = dsig.Close() }()

	// hub 客户端替身：服务表里只有目标节点；**若发生中继调用则测试失败**（证明打洞真的成功）。
	hub := &fakeHubClient{services: []client.MeshService{{Node: peerID, Name: "volread", Addr: serviceAddr}}}
	d := NewRemoteDialer(RemoteDialerConfig{
		Client:             hub,
		Signaler:           dsig,
		Service:            "volread",
		AllowRelayFallback: false, // 显式 webrtc 语义：打洞失败即错
	})
	conn, err := d.Dial(ctx, peerID)
	if err != nil {
		t.Fatalf("打洞失败（不允许回落，故直接报错）: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if got := hub.relayCalls(); len(got) != 0 {
		t.Fatalf("打洞成功时不得调用中继: %v", got)
	}
	if _, werr := conn.Write([]byte("dialer-punch")); werr != nil {
		t.Fatalf("写: %v", werr)
	}
	buf := make([]byte, 64)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, rErr := conn.Read(buf)
	if rErr != nil {
		t.Fatalf("读: %v", rErr)
	}
	if string(buf[:n]) != "dialer-punch" {
		t.Fatalf("echo 内容=%q", buf[:n])
	}
}
