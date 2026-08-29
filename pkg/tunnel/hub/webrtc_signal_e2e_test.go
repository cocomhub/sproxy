// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"strconv"
	"testing"
	"time"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// TestWebrtcDialListen_WithHubSignaler 端到端验证 C1 修复：
// 真实 pion PeerConnection 经 fake hub + HubSignaler 完成跨"节点"信令握手，
// DataChannel 建立后双向传输。这是 p2p connect/listen 的核心路径回归测试。
//
// 使用 SetHostOnly(true) 使 ICE 仅用本机 host 候选（无 STUN 依赖，CI 可跑）。
func TestWebrtcDialListen_WithHubSignaler(t *testing.T) {
	// 备份并还原全局 hostOnly 状态
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	// I11/S12：信令整体预算 60s > 单 poll 25s，消除末段错过。
	// SetSignalingTimeout 是包级全局变量，需 t.Cleanup 恢复默认（S69）。
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(func() { webrtc.SetSignalingTimeout(30 * time.Second) })

	hub := fakeSignalHub(t)
	defer hub.Close()

	sigDialer := NewHubSignaler(hub.URL, "", "node-A")
	sigListener := NewHubSignaler(hub.URL, "", "node-B")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// listener 先就绪：收 ping → 写 pong → 收 done → 关闭
	listenDone := make(chan error, 1)
	go func() {
		conn, err := webrtc.ListenWithSignaler("node-B", sigListener)
		if err != nil {
			listenDone <- err
			return
		}
		defer conn.Close()
		readLine := func() ([]byte, error) {
			buf := make([]byte, 32)
			n, rerr := conn.Read(buf)
			return buf[:n], rerr
		}
		ping, rerr := readLine()
		if rerr != nil {
			listenDone <- rerr
			return
		}
		if string(ping) != "ping" {
			listenDone <- &errUnexpected{got: string(ping)}
			return
		}
		if _, werr := conn.Write([]byte("pong")); werr != nil {
			listenDone <- werr
			return
		}
		done, rerr2 := readLine()
		if rerr2 != nil {
			listenDone <- rerr2
			return
		}
		if string(done) != "done" {
			listenDone <- &errUnexpected{got: string(done)}
			return
		}
		listenDone <- nil
	}()

	// 拨号方连接并写入 ping
	dialConn, err := webrtc.DialWithSignaler("node-B", sigDialer)
	if err != nil {
		t.Fatalf("DialWithSignaler: %v", err)
	}
	defer dialConn.Close()

	if _, werr := dialConn.Write([]byte("ping")); werr != nil {
		t.Fatalf("write ping: %v", werr)
	}
	readLine := func() ([]byte, error) {
		buf := make([]byte, 32)
		n, rerr := dialConn.Read(buf)
		return buf[:n], rerr
	}
	pong, rerr := readLine()
	if rerr != nil {
		t.Fatalf("read pong: %v", rerr)
	}
	if string(pong) != "pong" {
		t.Fatalf("got %q, want pong", pong)
	}
	// 通知 listener 可以关闭
	if _, werr := dialConn.Write([]byte("done")); werr != nil {
		t.Fatalf("write done: %v", werr)
	}

	// 确认 listener 侧也成功
	select {
	case err := <-listenDone:
		if err != nil {
			t.Fatalf("listener 侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("listener 侧未在超时内完成")
	}
}

// TestWebrtcXferConn_FramingLargeMessage 验证 webrtcXferConn 的 [4B len][payload]
// 分帧能完整传输超过单次 Read 缓冲（65536）的大消息——I6 回归测试。
// 直接对同一 peer 的两端做 xfer.Send/Receive，不经过 mux。
func TestWebrtcXferConn_FramingLargeMessage(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	// I11/S12：与主 e2e 测试一致的信令预算（全局变量需 t.Cleanup 恢复）。
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(func() { webrtc.SetSignalingTimeout(30 * time.Second) })

	hub := fakeSignalHub(t)
	defer hub.Close()

	sigDialer := NewHubSignaler(hub.URL, "", "node-A")
	sigListener := NewHubSignaler(hub.URL, "", "node-B")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// listener 建立 xfer.Conn 并 Receive 大消息
	recvDone := make(chan error, 1)
	go func() {
		conn, err := webrtc.ListenWithSignaler("node-B", sigListener)
		if err != nil {
			recvDone <- err
			return
		}
		defer conn.Close()
		xc := webrtc.ConnAsXfer(conn)
		got, rerr := xc.Receive(ctx)
		if rerr != nil {
			recvDone <- rerr
			return
		}
		want := 70000 // 超过 65536，旧实现会截断
		if len(got) != want {
			recvDone <- &errUnexpected{got: "len=" + strconv.Itoa(len(got))}
			return
		}
		recvDone <- nil
	}()

	dialConn, err := webrtc.DialWithSignaler("node-B", sigDialer)
	if err != nil {
		t.Fatalf("DialWithSignaler: %v", err)
	}
	defer dialConn.Close()
	xc := webrtc.ConnAsXfer(dialConn)

	big := make([]byte, 70000)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := xc.Send(ctx, big); err != nil {
		t.Fatalf("Send big message: %v", err)
	}

	select {
	case err := <-recvDone:
		if err != nil {
			t.Fatalf("listener 侧接收失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("listener 侧未在超时内完成")
	}
}

type errUnexpected struct{ got string }

func (e *errUnexpected) Error() string { return "unexpected data: " + e.got }
