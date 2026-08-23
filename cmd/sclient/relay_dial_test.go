// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// fakeRelayDialClient 是 relayDialClient 的测试桩：RelayStream 从预置的 conns 通道
// 取一条 net.Conn（每次调用取一条），并记录 target/addr 调用。
type fakeRelayDialClient struct {
	mu    sync.Mutex
	calls []string
	err   error
	conns chan net.Conn
}

func (f *fakeRelayDialClient) RelayStream(ctx context.Context, target, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.calls = append(f.calls, target+"|"+addr)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	select {
	case c := <-f.conns:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (f *fakeRelayDialClient) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeRelayDialClient) call(i int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if i < 0 || i >= len(f.calls) {
		return ""
	}
	return f.calls[i]
}

// waitRelayDialCondition 轮询等待条件成立（timeout 护栏内），超时即测试失败。
func waitRelayDialCondition(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %s 超时", desc)
}

// syncBuffer 是并发安全的输出收集器：生产 goroutine 写、测试主 goroutine 读。
// strings.Builder 非线程安全，直接共享会在 -race 下报数据竞争。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRelayDialOnce_RemoteDisconnect_Returns 是 I41 挂起修复的单次 stdio 回归：
// 远端（server 端）先断开、本地 stdin 永不 EOF 时，relayDialOnce 必须返回。
// 旧实现 wg.Wait() 会在此场景永久挂起（CLI 假死）。
func TestRelayDialOnce_RemoteDisconnect_Returns(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// ios.In 用 io.Pipe：测试期间不关闭写端 pw，保证本地 stdin 永不 EOF。
	pr, pw := io.Pipe()
	defer pw.Close() // 测试结束时解除 in-goroutine 的阻塞读，避免 goroutine 泄漏

	var out strings.Builder
	ios := cli.IOStreams{In: pr, Out: &out, ErrOut: io.Discard}
	mock := &fakeRelayDialClient{conns: make(chan net.Conn, 1)}
	mock.conns <- client

	done := make(chan error, 1)
	go func() {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background()) // 生产经 Execute() 注入 ctx，测试需显式设置
		done <- relayDialOnce(cmd, mock, "node", "127.0.0.1:22", ios)
	}()

	// 远端先断：关闭 server 端 → client 端读 EOF → outDone 关闭。
	_ = server.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relayDialOnce returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relayDialOnce 挂起：远端断开后未返回（I41 回归）")
	}
}

// TestRelayDialOnce_EchoStdinEOF_WaitsForRemoteResponse 验证 `echo x | relay dial`
// 语义：本地 stdin EOF 后必须向对端传播半关闭（FIN）并等待远端把剩余响应写完
// 再返回，不得截断在途数据。
//
// 用 TCP 回环对而非 net.Pipe：*net.PipeConn 无 CloseWrite，closeWriteConn 会退化
// Close 整条连接，与远端写响应构成竞态（P0-5 修复后测试不确定）。TCPConn 支持
// CloseWrite（半关闭），才能确定性地验证「stdin EOF → FIN → 远端感知写完 →
// 响应写回不截断」的完整语义。
func TestRelayDialOnce_EchoStdinEOF_WaitsForRemoteResponse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverCh := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		serverCh <- c
	}()
	client, derr := net.Dial("tcp", ln.Addr().String())
	if derr != nil {
		t.Fatal(derr)
	}
	defer client.Close()
	server := <-serverCh
	defer server.Close()

	pr, pw := io.Pipe()
	defer pw.Close()

	var out strings.Builder
	ios := cli.IOStreams{In: pr, Out: &out, ErrOut: io.Discard}
	mock := &fakeRelayDialClient{conns: make(chan net.Conn, 1)}
	mock.conns <- client

	done := make(chan error, 1)
	go func() {
		cmd := &cobra.Command{}
		cmd.SetContext(context.Background()) // 生产经 Execute() 注入 ctx，测试需显式设置
		done <- relayDialOnce(cmd, mock, "node", "127.0.0.1:22", ios)
	}()

	// 读走 client→server 方向的输入；随后再次读，等待 stdin EOF 传播的 FIN
	//（半关闭后 server.Read 返回 io.EOF）——确认 P0-5 的 closeWriteConn 已生效。
	serverRead := make(chan struct{})
	go func() {
		defer close(serverRead)
		buf := make([]byte, 64)
		_, _ = server.Read(buf) // "ping\n"
		_, _ = server.Read(buf) // FIN → io.EOF
	}()

	// stdin EOF：写入一行输入后关闭读端。
	_, _ = pw.Write([]byte("ping\n"))
	_ = pw.Close()
	<-serverRead // 输入写完且 FIN 已传播（半关闭生效）

	// 远端随后写响应再关闭：relayDialOnce 必须等响应写完才返回（echo 语义），
	// 若在 stdin EOF 时 Close 整条连接（而非 CloseWrite）则会截断 "pong\n"。
	_ = server.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, _ = server.Write([]byte("pong\n"))
	_ = server.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relayDialOnce returned error: %v", err)
		}
		if !strings.Contains(out.String(), "pong\n") {
			t.Fatalf("stdin EOF 后应等待远端响应写完，got output: %q", out.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relayDialOnce 挂起：stdin EOF 后未返回（echo 语义回归）")
	}
}

// TestRelayDialListenOn_RemoteDisconnect_ClosesConnAndKeepsListening 是 I41 挂起修复
// 的端口转发回归：单条转发连接中远端先断时，per-conn goroutine 必须双侧关闭
// （本地连接收到 EOF/复位，不泄漏），且 listener 仍能接受下一条连接。
func TestRelayDialListenOn_RemoteDisconnect_ClosesConnAndKeepsListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mock := &fakeRelayDialClient{conns: make(chan net.Conn, 8)}
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}

	done := make(chan error, 1)
	go func() { done <- relayDialListenOn(ctx, mock, "node", "127.0.0.1:22", ln, ios) }()

	// 第一条连接：本地 TCP 客户端 ⇄ mock 返回的 net.Pipe client 端。
	remote1, client1 := net.Pipe()
	mock.conns <- client1
	defer remote1.Close()
	defer client1.Close()

	local, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial local listener: %v", err)
	}
	defer local.Close()

	waitRelayDialCondition(t, 3*time.Second, "RelayStream 被调用", func() bool { return mock.callCount() == 1 })
	if got := mock.call(0); got != "node|127.0.0.1:22" {
		t.Fatalf("unexpected RelayStream target: %q", got)
	}

	// 远端先断：关闭 remote1 → client1 读 EOF → per-conn 双侧 close → local 被关闭。
	_ = remote1.Close()

	_ = local.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 16)
	_, rerr := local.Read(buf)
	if rerr == nil {
		t.Fatal("远端断开后 local 连接应被关闭（读到 EOF/错误），但读到了数据")
	} else if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
		t.Fatal("远端断开后 local 连接应被关闭，但读取超时（连接未被关闭，goroutine 泄漏）")
	}

	// relayDialListenOn 仍应存活（accept 循环继续）。
	select {
	case err := <-done:
		t.Fatalf("relayDialListenOn 不应退出，got: %v", err)
	default:
	}

	// 第二条连接：验证 listener 仍接受新连接。
	remote2, client2 := net.Pipe()
	mock.conns <- client2
	defer remote2.Close()
	defer client2.Close()

	local2, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial local listener (2nd): %v", err)
	}
	defer local2.Close()

	waitRelayDialCondition(t, 3*time.Second, "第二条连接建立", func() bool { return mock.callCount() == 2 })

	// 收尾：远端断开使第二条连接 goroutine 正常退出。
	_ = remote2.Close()
}

// TestRelayDialListenOn_CtxCancel_ClosesListener 是 S58 回归：ctx 取消时 listener
// 被关闭、Accept 立即返回、relayDialListenOn 返回 nil（优雅停止端口转发）。
func TestRelayDialListenOn_CtxCancel_ClosesListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	mock := &fakeRelayDialClient{conns: make(chan net.Conn, 1)}
	ios := cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}

	done := make(chan error, 1)
	go func() { done <- relayDialListenOn(ctx, mock, "node", "127.0.0.1:22", ln, ios) }()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ctx 取消后 relayDialListenOn 应返回 nil，got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ctx 取消后 relayDialListenOn 未返回（S58 回归）")
	}
}

// TestRelayDialListen_NormalizesBarePortToLoopback 是 Windows 监听要求回归：
// `-l :0` 裸端口必须归一为 127.0.0.1:0（loopback 绑定，防 Windows Defender
// 防火墙弹窗），显式 0.0.0.0/具体 IP 保持原样（由 normalizeListenAddr 保证）。
func TestRelayDialListen_NormalizesBarePortToLoopback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := &cobra.Command{}
	cmd.SetContext(ctx)

	mock := &fakeRelayDialClient{conns: make(chan net.Conn, 1)}
	out := &syncBuffer{}
	ios := cli.IOStreams{Out: out, ErrOut: io.Discard}

	done := make(chan error, 1)
	go func() { done <- relayDialListen(cmd, mock, "node", "127.0.0.1:22", ":0", ios) }()

	// 横幅打印的是归一后的监听地址：裸 :0 → 127.0.0.1:0。
	waitRelayDialCondition(t, 3*time.Second, "端口转发横幅", func() bool {
		return strings.Contains(out.String(), "127.0.0.1:0")
	})

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("relayDialListen returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relayDialListen 未在 ctx 取消后返回")
	}
}

// TestPumpConns_HalfCloseKeepsInFlightResponse 验证 pumpConns 的半关闭语义：
// 本地写完并 CloseWrite（半关闭）后，远端在途响应仍可被读回，不截断
// （对齐 leaf.go pump 的 C1 修复；meshForwardListen / relayDialListenOn 共用）。
// 旧实现「首方向完成即双侧 Close」会在此场景截断远端响应。
func TestPumpConns_HalfCloseKeepsInFlightResponse(t *testing.T) {
	// 两对 TCP：pair1 的测试端 aPeer 控制 pumpConns 的 a，pair2 的 mock 远端
	// bPeer 控制 pumpConns 的 b。
	mkPair := func() (peer, srv *net.TCPConn) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		acceptCh := make(chan *net.TCPConn, 1)
		go func() {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			acceptCh <- c.(*net.TCPConn)
		}()
		dc, derr := net.Dial("tcp", ln.Addr().String())
		if derr != nil {
			t.Fatal(derr)
		}
		peer = dc.(*net.TCPConn)
		return peer, <-acceptCh
	}

	aPeer, a := mkPair()
	bPeer, b := mkPair()
	defer aPeer.Close()
	defer a.Close()
	defer bPeer.Close()
	defer b.Close()

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		pumpConns(a, b, pumpGracePeriod)
	}()

	// mock 远端：读到 "ping" 后回 "pong" 并半关闭（保留读方向，bPeer 仍可写）。
	remoteDone := make(chan struct{})
	go func() {
		defer close(remoteDone)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(bPeer, buf); err != nil {
			return
		}
		_, _ = bPeer.Write([]byte("pong"))
		_ = bPeer.CloseWrite()
	}()

	// 测试端：写 "ping" 并半关闭，随后必须能读回 "pong"（在途响应不截断）。
	if _, err := aPeer.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := aPeer.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	_ = aPeer.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, 4)
	if _, err := io.ReadFull(aPeer, got); err != nil {
		t.Fatalf("半关闭后应读到远端在途响应 pong，实际 %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("got %q, want %q", got, "pong")
	}

	<-remoteDone
	select {
	case <-pumpDone:
	case <-time.After(5 * time.Second):
		t.Fatal("pumpConns 未在双方向完成后返回")
	}
}
