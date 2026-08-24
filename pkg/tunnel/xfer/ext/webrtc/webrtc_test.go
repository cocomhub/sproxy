// Copyright 2026 The Cocomhub Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0 style license that
// can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package webrtc

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// TestWebrtcXferConn_CloseUnblocksBlockedSend（P0-2 回归）：
// Send 持 mu 阻塞在 raw.Write（对端存活但不消费，SCTP 流控窗口归零）时，
// Close 必须不经 mu 直接 raw.Close() 解除阻塞——否则 Close 等不到 mu、
// 阻塞中的 Write 永不解除，Send 与 Close 互相等待构成永久死锁。
func TestWebrtcXferConn_CloseUnblocksBlockedSend(t *testing.T) {
	raw := &blockingRaw{writeCh: make(chan struct{}), closeCh: make(chan struct{})}
	c := &webrtcXferConn{raw: raw}

	sendDone := make(chan error, 1)
	go func() {
		sendDone <- c.Send(context.Background(), []byte("hello"))
	}()

	// 等 Send 进入阻塞的 raw.Write（确定性时序：Close 必在 Write 阻塞后才开始）。
	select {
	case <-raw.writeCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Send 未进入 raw.Write")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- c.Close()
	}()

	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close 死锁：无法解除阻塞中的 Send")
	}

	select {
	case err := <-sendDone:
		if err == nil {
			t.Fatal("Send 应在 Close 后返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Send 未在 Close 后返回")
	}

	// 幂等：二次 Close 立即返回。
	if err := c.Close(); err != nil {
		t.Fatalf("二次 Close: %v", err)
	}
}

// blockingRaw 是阻塞写的底层连接桩：Write 阻塞直到 Close 关闭 closeCh。
type blockingRaw struct {
	writeCh chan struct{} // Write 被调用时关闭（标记已进入阻塞）
	closeCh chan struct{} // Close 时关闭，解除 Write
	once    sync.Once
}

func (r *blockingRaw) Read(_ []byte) (int, error) { return 0, io.EOF }
func (r *blockingRaw) Write(p []byte) (int, error) {
	close(r.writeCh)
	<-r.closeCh
	return 0, io.ErrClosedPipe
}
func (r *blockingRaw) Close() error {
	r.once.Do(func() { close(r.closeCh) })
	return nil
}

// idleSignaler 是等待即阻塞的 Signaler 桩：Wait* 阻塞到 ctx 取消。
type idleSignaler struct{}

func (idleSignaler) SendOffer(string, string) error  { return nil }
func (idleSignaler) SendAnswer(string, string) error { return nil }
func (idleSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}
func (idleSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	<-ctx.Done()
	return "", "", ctx.Err()
}

// TestListenWithSignaler_IdleTimeout_ReturnsSentinel（P1-11 回归）：
// 无对端在 signalingTimeout 内发起连接时，ListenWithSignalerCtx 必须返回
// ErrNoIncomingConnection 哨兵（供 p2p listen 区分"空闲"与"失败"，避免空闲时
// 无条件重注册 + per-node secret 轮换的注册抖动）。
func TestListenWithSignaler_IdleTimeout_ReturnsSentinel(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	SetSignalingTimeout(200 * time.Millisecond)
	t.Cleanup(ResetSignalingTimeout)

	_, err := ListenWithSignalerCtx(context.Background(), "peer", idleSignaler{})
	if !errors.Is(err, ErrNoIncomingConnection) {
		t.Fatalf("空闲超时应返回 ErrNoIncomingConnection，got: %v", err)
	}
}

// TestWebrtcRoundTrip verifies bidirectional message exchange.
func TestWebrtcRoundTrip(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()
	payload := []byte("Hello WebRTC!")

	type result struct {
		err  error
		data []byte
	}

	dialRes := make(chan result, 1)
	listenRes := make(chan result, 1)
	dialDone := make(chan struct{})

	// Listen goroutine.
	go func() {
		conn, err := Listen(signal)
		if err != nil {
			listenRes <- result{err: err}
			return
		}

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			conn.Close()
			listenRes <- result{err: err}
			return
		}

		if _, err := conn.Write(buf[:n]); err != nil {
			conn.Close()
			listenRes <- result{err: err}
			return
		}
		listenRes <- result{data: buf[:n]}
		<-dialDone
		conn.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	// Dial goroutine.
	go func() {
		conn, err := Dial(signal)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		defer conn.Close()

		if _, werr := conn.Write(payload); werr != nil {
			dialRes <- result{err: werr}
			return
		}

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			dialRes <- result{err: err}
			return
		}
		dialRes <- result{data: buf[:n]}
		close(dialDone)
	}()

	var dialR, listenR result

	select {
	case dialR = <-dialRes:
	case <-time.After(20 * time.Second):
		t.Fatal("dial timed out")
	}
	select {
	case listenR = <-listenRes:
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}

	if dialR.err != nil {
		t.Fatalf("Dial: %v", dialR.err)
	}
	if listenR.err != nil {
		t.Fatalf("Listen: %v", listenR.err)
	}

	if string(listenR.data) != string(payload) {
		t.Errorf("listen got %q, want %q", string(listenR.data), string(payload))
	}
	if string(dialR.data) != string(payload) {
		t.Errorf("dial got %q, want %q", string(dialR.data), string(payload))
	}
}

// TestWebrtcBasicConnect verifies one-way message delivery.
func TestWebrtcBasicConnect(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()
	payload := []byte("ping")

	listenRes := make(chan error, 1)
	var listenData []byte

	go func() {
		conn, err := Listen(signal)
		if err != nil {
			listenRes <- err
			return
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		if err != nil {
			listenRes <- err
			return
		}
		listenData = buf[:n]
		listenRes <- nil
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("Dial write: %v", err)
	}

	select {
	case err := <-listenRes:
		if err != nil {
			t.Fatalf("Listen: %v", err)
		}
		if string(listenData) != string(payload) {
			t.Errorf("listen got %q, want %q", string(listenData), string(payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}
}

// TestWebrtcConcurrentSends verifies concurrent writes from dialer to listener
// with content-set assertion（并发写顺序不保证，但内容必须完整一致）。
func TestWebrtcConcurrentSends(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()
	payloads := []string{"msg1", "msg2", "msg3"}

	received := make(chan string, len(payloads))
	listenDone := make(chan struct{})

	go func() {
		defer close(listenDone)
		conn, err := Listen(signal)
		if err != nil {
			t.Errorf("Listen: %v", err)
			return
		}
		defer conn.Close()

		buf := make([]byte, 4096)
		for range payloads {
			n, err := conn.Read(buf)
			if err != nil {
				t.Errorf("Read: %v", err)
				return
			}
			received <- string(buf[:n])
		}
	}()

	time.Sleep(50 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	var wg sync.WaitGroup
	for _, p := range payloads {
		wg.Add(1)
		go func(payload string) {
			defer wg.Done()
			if _, err := conn.Write([]byte(payload)); err != nil {
				t.Errorf("Write %q: %v", payload, err)
			}
		}(p)
	}
	wg.Wait()

	select {
	case <-listenDone:
	case <-time.After(10 * time.Second):
		t.Fatal("listen timed out")
	}
	close(received)

	got := make([]string, 0, len(payloads))
	for m := range received {
		got = append(got, m)
	}
	if len(got) != len(payloads) {
		t.Fatalf("received %d messages, want %d", len(got), len(payloads))
	}
	// 内容集合断言：每条 payload 都恰好收到一次（并发写顺序不保证）。
	want := make(map[string]int, len(payloads))
	for _, p := range payloads {
		want[p]++
	}
	for _, m := range got {
		want[m]--
	}
	for p, n := range want {
		if n != 0 {
			t.Errorf("message %q: 收到次数偏差 %d（应为 0）", p, n)
		}
	}
}

// TestWebrtcCloseBeforeRead 验证 Close 后 Read 在短超时内确定性返回错误。
// 修复前 pion detached DataChannel 在 pc.Close 后 Read 可能阻塞数秒甚至更久，
// 原测试"data or error both acceptable"实为死测试。
func TestWebrtcCloseBeforeRead(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()

	listenReady := make(chan struct{})
	listenErr := make(chan error, 1)

	go func() {
		conn, err := Listen(signal)
		if err != nil {
			listenErr <- err
			return
		}
		close(listenReady)
		// 连接建立后先 Close，再 Read —— Read 必须快速返回错误，不得阻塞。
		conn.Close()
		buf := make([]byte, 4096)
		_, err = conn.Read(buf)
		listenErr <- err
	}()

	time.Sleep(50 * time.Millisecond)
	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// 等待监听侧连接完全建立（dc 已开）再关闭，避免过早关闭让监听侧 Listen 挂起。
	select {
	case <-listenReady:
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not establish connection")
	}
	conn.Close()

	select {
	case err := <-listenErr:
		if err == nil {
			t.Fatal("Read after Close returned nil error, want error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read after Close did not return within 2s")
	}
}

// TestWebrtcLargeMessage verifies 64 KiB message transfer.
func TestWebrtcLargeMessage(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()
	payload := make([]byte, 65536)
	for i := range payload {
		payload[i] = byte(i % 256)
	}

	listenDone := make(chan []byte, 1)
	go func() {
		conn, err := Listen(signal)
		if err != nil {
			t.Errorf("Listen: %v", err)
			listenDone <- nil
			return
		}
		defer conn.Close()

		buf := make([]byte, 131072)
		n, err := conn.Read(buf)
		if err != nil {
			t.Errorf("Read: %v", err)
			listenDone <- nil
			return
		}
		listenDone <- buf[:n]
	}()

	time.Sleep(100 * time.Millisecond)

	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("Write: %v", err)
	}

	select {
	case got := <-listenDone:
		if len(got) != len(payload) {
			t.Errorf("received %d bytes, want %d", len(got), len(payload))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("listen timed out")
	}
}

// TestWebrtcSignal verifies Signal channel capacity.
func TestWebrtcSignal(t *testing.T) {
	signal := NewSignal()
	if signal.Offer == nil || signal.Answer == nil {
		t.Fatal("NewSignal channels are nil")
	}
	if cap(signal.Offer) != 1 || cap(signal.Answer) != 1 {
		t.Errorf("channel capacity = %d/%d, want 1/1", cap(signal.Offer), cap(signal.Answer))
	}
}

// TestWebrtcAddr verifies webrtcAddr satisfies net.Addr.
func TestWebrtcAddr(t *testing.T) {
	addr := webrtcAddr{}
	if addr.Network() != "webrtc" {
		t.Errorf("Network = %q, want webrtc", addr.Network())
	}
	if addr.String() != "webrtc" {
		t.Errorf("String = %q, want webrtc", addr.String())
	}
}

// TestWebrtcConnDeadlines verifies deadline methods are no-ops.
func TestWebrtcConnDeadlines(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()

	listenReady := make(chan struct{})
	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		conn, err := Listen(signal)
		if err != nil {
			return
		}
		defer conn.Close()
		close(listenReady)

		// Deadline methods should be no-ops
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
	}()

	time.Sleep(50 * time.Millisecond)
	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	// 等待监听侧连接建立再关闭，避免监听侧 Listen 挂起 30s。
	select {
	case <-listenReady:
	case <-time.After(10 * time.Second):
		t.Fatal("listener did not establish connection")
	}
	conn.Close()
	<-listenDone
}

// TestWebrtcXferConn_ClosedSemantics 验证 xfer 包装的 Send/Receive 关闭语义：
//   - Send 超 maxFrameBytes 返回错误（大小上限防御）
//   - Close 后 Send/Receive 返回 xfer.ErrConnClosed（与 tcp 传输对齐）
func TestWebrtcXferConn_ClosedSemantics(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	signal := NewSignal()

	peerRes := make(chan xfer.Conn, 1)
	go func() {
		conn, err := Listen(signal)
		if err != nil {
			t.Errorf("Listen: %v", err)
			return
		}
		peerRes <- ConnAsXfer(conn)
	}()

	time.Sleep(50 * time.Millisecond)
	conn, err := Dial(signal)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	xc := ConnAsXfer(conn)

	// 超大小上限 → 报错
	if sendErr := xc.Send(context.Background(), make([]byte, maxFrameBytes+1)); sendErr == nil {
		t.Fatal("Send 超过 maxFrameBytes 应返回错误")
	}

	// 正常发送一条，对端应能收到
	msg := []byte("hello xfer")
	if sendErr := xc.Send(context.Background(), msg); sendErr != nil {
		t.Fatalf("Send: %v", sendErr)
	}

	peer := <-peerRes
	defer peer.Close()
	got, err := peer.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}

	// Close 后 Send/Receive 返回 ErrConnClosed
	if err := xc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := xc.Send(context.Background(), []byte("after close")); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("Send after Close = %v, want xfer.ErrConnClosed", err)
	}
	if _, err := xc.Receive(context.Background()); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("Receive after Close = %v, want xfer.ErrConnClosed", err)
	}
}

// TestWebrtcListener_Close 验证 xfer listener Close 后 Accept 即时返回错误，
// 且 Close 幂等（重复调用不 panic）。
// 直接构造 webrtcListener（不起常驻 acceptLoop goroutine，避免测试改全局
// useHostOnly 时与后台 goroutine 读全局产生数据竞争）。
func TestWebrtcListener_Close(t *testing.T) {
	ln := &webrtcListener{
		signal:   NewSignal(),
		addr:     "test-addr-close",
		acceptCh: make(chan *webrtcXferConn, 1),
		done:     make(chan struct{}),
	}

	ln.Close()
	start := time.Now()
	_, err := ln.Accept(context.Background())
	if err == nil {
		t.Fatal("Accept after Close 应返回错误")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Accept after Close 耗时 %v，应即时返回", elapsed)
	}
	// 幂等：重复 Close 不 panic
	ln.Close()
}

// remotePeerSignaler 是一个保留 From 身份的内存信号器（测 Conn.RemotePeerID）：
// 拨号方视角 answerFrom 须等于 Dial 的 peer（Dial 内部校验 from==peer），
// 监听方视角 offerFrom 是拨号方临时身份（如 "disc-node-a-123"）。
type remotePeerSignaler struct {
	offerFrom  string
	answerFrom string
	offer      chan string
	answer     chan string
}

func (s *remotePeerSignaler) SendOffer(_ string, sdp string) error { s.offer <- sdp; return nil }
func (s *remotePeerSignaler) WaitOffer(ctx context.Context) (string, string, error) {
	select {
	case sdp := <-s.offer:
		return s.offerFrom, sdp, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}
func (s *remotePeerSignaler) SendAnswer(_ string, sdp string) error { s.answer <- sdp; return nil }
func (s *remotePeerSignaler) WaitAnswer(ctx context.Context) (string, string, error) {
	select {
	case sdp := <-s.answer:
		return s.answerFrom, sdp, nil
	case <-ctx.Done():
		return "", "", ctx.Err()
	}
}

// TestConn_RemotePeerID：Dial 侧 RemotePeerID 为目标 peer；Listen 侧为 offer 发送方
// （offerFrom，供 mesh accept 侧恢复 discovery 拨号者真实 node-id）。
func TestConn_RemotePeerID(t *testing.T) {
	// loopback 收敛 + 禁用 mDNS，避免 Windows 测试弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	SetHostOnly(true)
	t.Cleanup(func() { SetHostOnly(false) })
	sig := &remotePeerSignaler{
		offerFrom:  "disc-node-a-123", // 模拟拨号方临时身份（含真实 base "node-a"）
		answerFrom: "node-b",          // 模拟监听方真实 ID（= 拨号方 peer）
		offer:      make(chan string, 1),
		answer:     make(chan string, 1),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	listenID := make(chan string, 1)
	go func() {
		conn, err := ListenWithSignalerCtx(ctx, "node-b", sig)
		if err != nil {
			listenID <- "<err>"
			return
		}
		defer conn.Close()
		listenID <- conn.RemotePeerID()
	}()

	conn, err := DialWithSignalerCtx(ctx, "node-b", sig)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if conn.RemotePeerID() != "node-b" {
		t.Fatalf("dial RemotePeerID = %q, want node-b", conn.RemotePeerID())
	}
	select {
	case id := <-listenID:
		if id != "disc-node-a-123" {
			t.Fatalf("listen RemotePeerID = %q, want disc-node-a-123", id)
		}
	case <-ctx.Done():
		t.Fatal("等待 listen RemotePeerID 超时")
	}
}
