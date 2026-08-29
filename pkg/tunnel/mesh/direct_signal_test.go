// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestDirectSignaler_OfferAnswer 直连信令协议级测试：拨号侧发 offer、监听侧读 offer
// 并回 answer、拨号侧读 answer，且监听侧能恢复拨号方 node-id。
func TestDirectSignaler_OfferAnswer(t *testing.T) {
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go srv.Serve(ctx)
	defer srv.Close()

	serverSig := srv.NewSignaler()
	listenerErr := make(chan error, 1)
	go func() {
		from, offer, werr := serverSig.WaitOffer(ctx)
		if werr != nil {
			listenerErr <- werr
			return
		}
		if from != "node-dialer" {
			listenerErr <- fmt.Errorf("from = %q, want node-dialer", from)
			return
		}
		if offer != "offer-sdp" {
			listenerErr <- fmt.Errorf("offer = %q, want offer-sdp", offer)
			return
		}
		listenerErr <- serverSig.SendAnswer("node-dialer", "answer-sdp")
	}()

	client, err := DialDirectSignaler(ctx, srv.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer client.Close()
	if serr := client.SendOffer("node-listener", "offer-sdp"); serr != nil {
		t.Fatalf("SendOffer: %v", serr)
	}
	from, answer, err := client.WaitAnswer(ctx)
	if err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if from != "" {
		t.Errorf("answer from = %q, want 空（拨号侧不区分）", from)
	}
	if answer != "answer-sdp" {
		t.Errorf("answer = %q, want answer-sdp", answer)
	}
	select {
	case err := <-listenerErr:
		if err != nil {
			t.Fatalf("监听侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("等待监听侧完成超时")
	}
}

// TestDirectSignaler_MalformedConnNonFatal（F1 回归）：直连信令端口收到空/畸形连接
// （端口扫描 / curl 误连）应返回 errDirectSignalConn 并关闭该连接，**不**使监听器失效；
// 随后正常的 offer/answer 交换仍能成功（否则远程无认证即可杀整节点）。
func TestDirectSignaler_MalformedConnNonFatal(t *testing.T) {
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go srv.Serve(ctx)
	defer srv.Close()

	sig := srv.NewSignaler()
	// 场景 1：连上即关（端口扫描）→ WaitOffer 返回 errDirectSignalConn。
	conn, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = conn.Close()
	if _, _, werr := sig.WaitOffer(ctx); !errors.Is(werr, errDirectSignalConn) {
		t.Fatalf("空连接应返回 errDirectSignalConn, got %v", werr)
	}
	// 场景 2：畸形长度前缀（0xFFFFFFFF > 上限）→ 同样非致命。
	conn2, err := net.Dial("tcp", srv.Addr().String())
	if err != nil {
		t.Fatalf("dial2: %v", err)
	}
	_, _ = conn2.Write([]byte{0xff, 0xff, 0xff, 0xff, 'x', 'x'})
	_ = conn2.Close()
	if _, _, werr := sig.WaitOffer(ctx); !errors.Is(werr, errDirectSignalConn) {
		t.Fatalf("畸形帧应返回 errDirectSignalConn, got %v", werr)
	}
	// 场景 3：监听器仍存活——正常 offer/answer 成功。
	client, err := DialDirectSignaler(ctx, srv.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer client.Close()
	listenerErr := make(chan error, 1)
	go func() {
		from, offer, werr := sig.WaitOffer(ctx)
		if werr != nil {
			listenerErr <- werr
			return
		}
		if from != "node-dialer" || offer != "offer-sdp" {
			listenerErr <- fmt.Errorf("from=%q offer=%q", from, offer)
			return
		}
		listenerErr <- sig.SendAnswer("node-dialer", "answer-sdp")
	}()
	if serr := client.SendOffer("node-listener", "offer-sdp"); serr != nil {
		t.Fatalf("SendOffer: %v", serr)
	}
	_, answer, aerr := client.WaitAnswer(ctx)
	if aerr != nil {
		t.Fatalf("WaitAnswer: %v", aerr)
	}
	if answer != "answer-sdp" {
		t.Errorf("answer = %q, want answer-sdp", answer)
	}
	select {
	case err := <-listenerErr:
		if err != nil {
			t.Fatalf("监听侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("等待监听侧完成超时")
	}
}

// TestDirectSignaler_WaitAnswerCtxAware（N1 回归）：对端信令端点可达但不回 answer 时，
// WaitAnswer 应在 ctx 到期时及时返回 ctx.Err()（而非卡满 directSignalTimeout 30s），
// 保证 WebRTCProbeTimeout(10s) 与用户中断/节点关停的 ctx 语义真实生效。
func TestDirectSignaler_WaitAnswerCtxAware(t *testing.T) {
	// 伪信令端点：接受 offer 帧后挂起（永不回 answer）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		defer c.Close()
		// 读完完整 offer 帧后挂起（不回复 answer），直到对端关闭连接。
		var lenBuf [4]byte
		if _, rerr := io.ReadFull(c, lenBuf[:]); rerr != nil {
			return
		}
		payload := make([]byte, binary.BigEndian.Uint32(lenBuf[:]))
		if _, rerr := io.ReadFull(c, payload); rerr != nil {
			return
		}
		var one [1]byte
		_, _ = c.Read(one[:]) // 阻塞：对端不会再发数据；连接关闭即返回
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := DialDirectSignaler(ctx, ln.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer client.Close()
	if serr := client.SendOffer("node-listener", "offer-sdp"); serr != nil {
		t.Fatalf("SendOffer: %v", serr)
	}

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer shortCancel()
	start := time.Now()
	_, _, werr := client.WaitAnswer(shortCtx)
	elapsed := time.Since(start)
	if !errors.Is(werr, context.DeadlineExceeded) {
		t.Fatalf("WaitAnswer = %v, want context.DeadlineExceeded", werr)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("WaitAnswer 未在 ctx 到期时及时返回，耗时 %v", elapsed)
	}
}

// TestWebRTCOverDirectSignaling 全链路：经直连信令（非 hub）建立 WebRTC 连接，
// 数据面可双向传输——这是 mDNS 无 hub 场景 `mesh connect` 的核心链路。
func TestWebRTCOverDirectSignaling(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback，避免防火墙弹窗。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go srv.Serve(ctx)
	defer srv.Close()

	// 监听侧：WaitOffer 接一个连接 → ListenWithSignalerCtx 完成握手 → 读写数据。
	// 收尾用二次握手（监听侧读 "bye"）避免"写完即关"与拨号侧读之间的竞态
	// （pion 在 close 后可能返回 abort 而非缓冲数据）。
	serverSig := srv.NewSignaler()
	listenerErr := make(chan error, 1)
	go func() {
		conn, lerr := webrtc.ListenWithSignalerCtx(ctx, "node-listener", serverSig)
		if lerr != nil {
			listenerErr <- lerr
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, rerr := conn.Read(buf)
		if rerr != nil {
			listenerErr <- fmt.Errorf("监听侧读失败: %w", rerr)
			return
		}
		if string(buf[:n]) != "hello" {
			listenerErr <- fmt.Errorf("监听侧收到 %q, want hello", buf[:n])
			return
		}
		if _, werr := conn.Write([]byte("world")); werr != nil {
			listenerErr <- fmt.Errorf("监听侧写失败: %w", werr)
			return
		}
		n, rerr = conn.Read(buf)
		if rerr != nil {
			listenerErr <- fmt.Errorf("监听侧读 bye 失败: %w", rerr)
			return
		}
		if string(buf[:n]) != "bye" {
			listenerErr <- fmt.Errorf("监听侧收到 %q, want bye", buf[:n])
			return
		}
		listenerErr <- nil
	}()

	client, err := DialDirectSignaler(ctx, srv.Addr().String(), "node-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer client.Close()
	conn, err := webrtc.DialWithSignalerCtx(ctx, "node-listener", client)
	if err != nil {
		t.Fatalf("DialWithSignalerCtx: %v", err)
	}
	defer conn.Close()
	if conn.RemotePeerID() != "node-listener" {
		t.Errorf("RemotePeerID = %q, want node-listener", conn.RemotePeerID())
	}
	if _, werr := conn.Write([]byte("hello")); werr != nil {
		t.Fatalf("拨号侧写失败: %v", werr)
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("拨号侧读失败: %v", err)
	}
	if string(buf[:n]) != "world" {
		t.Fatalf("拨号侧收到 %q, want world", buf[:n])
	}
	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatalf("拨号侧写 bye 失败: %v", err)
	}
	select {
	case err := <-listenerErr:
		if err != nil {
			t.Fatalf("监听侧失败: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("等待监听侧完成超时")
	}
}
