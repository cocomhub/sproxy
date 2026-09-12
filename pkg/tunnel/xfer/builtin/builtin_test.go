// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package builtin_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	builtin "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 命名导入：注册内置传输层（tcp/tcp+tls）并暴露配置桥
)

// TestBuiltinRegistersTCP 验证 blank import xfer/builtin 后内置 TCP 传输层
// 已注册（供 hub 裸 TCP 中继与 sclient relay --transport tcp 使用）。
func TestBuiltinRegistersTCP(t *testing.T) {
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered after importing xfer/builtin")
	}
	if tp.Name != "tcp" {
		t.Fatalf("expected name 'tcp', got %q", tp.Name)
	}
	if tp.Dial == nil || tp.Listen == nil {
		t.Fatal("tcp transport Dial/Listen should be non-nil")
	}
}

// TestBuiltinTCPRoundTrip 通过 xfer/builtin 注册的 TCP 传输做一次真实消息往返，
// 证明该传输可用于 relay 注册/数据面（hub 侧 Listen / 叶子侧 Dial）。
func TestBuiltinTCPRoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.(interface{ Addr() net.Addr }).Addr().String()

	var serverConn xfer.Conn
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		serverConn, err = ln.Accept(ctx)
	}()
	clientConn, derr := tp.Dial(ctx, addr)
	if derr != nil {
		t.Fatal(derr)
	}
	defer clientConn.Close()
	<-acceptDone
	if err != nil {
		t.Fatal(err)
	}
	if serverConn == nil {
		t.Fatal("expected accepted server conn")
	}
	defer serverConn.Close()

	msg := []byte("builtin-tcp-ping")
	if serr := clientConn.Send(ctx, msg); serr != nil {
		t.Fatal(serr)
	}
	got, rerr := serverConn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(msg) {
		t.Fatalf("expected %q, got %q", msg, got)
	}
}

// TestBuiltinSetDefaultTLSConfig 验证 SetDefaultTLSConfig 桥：外部调用方可经 builtin
// 设置 tcp+tls 传输的默认 TLS 配置（绕过 internal 可见性约束），设置后 tcp+tls 变体
// Listen 可用；未设置时明确报错（fail-closed，防无凭据明文承载）。
func TestBuiltinSetDefaultTLSConfig(t *testing.T) {
	tp := xfer.Get("tcp+tls")
	if tp == nil {
		t.Fatal("tcp+tls transport not registered after importing xfer/builtin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// 未设置 → Listen 报错（fail-closed）。
	builtin.SetDefaultTLSConfig(nil)
	if _, err := tp.Listen(ctx, "127.0.0.1:0"); err == nil {
		t.Fatal("未设置默认 TLS 配置时 tcp+tls Listen 应报错")
	}

	// 设置自签证书配置 → Listen 成功。
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("LoadX509KeyPair: %v", err)
	}
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })
	builtin.SetDefaultTLSConfig(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})

	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp+tls Listen（经 builtin 设置默认配置后）: %v", err)
	}
	defer ln.Close()
}

// TestFromNetConn_RoundTrip 验证 builtin.FromNetConn 桥接出的 xfer.Conn 真的可用：
// 一条消息经 net.Pipe 往返后逐字节一致。
//
// 说明：本用例**不**证明帧定界（单条短消息在同步 pipe 上，裸透传实现同样会绿）；
// 「确实复用 tcpConn 的 4B 长度前缀帧协议」由 TestFromNetConn_WireFormatIsLengthPrefixed 钉住。
//
// 用途背景（Y 一期 AD-6）：mesh 数据面（webrtc 直连 / hub 中继）交付的是字节流
// net.Conn，上层 mux 需要消息语义的 xfer.Conn。
func TestFromNetConn_RoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	ca, cb := builtin.FromNetConn(a), builtin.FromNetConn(b)
	want := []byte("hello-xfer-netconn")
	go func() { _ = ca.Send(context.Background(), want) }()

	// 带 deadline 的 ctx：桥接若退化为「对端永不可达」，Receive 必须限时报错而非
	// 让整个包挂到 go test 全局超时（回归时报错要快且指向明确）。
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := cb.Receive(ctx)
	if err != nil {
		t.Fatalf("Receive: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("往返内容不符: %q", got)
	}
}

// TestFromNetConn_WireFormatIsLengthPrefixed 直接钉住「桥接 == 复用 tcpConn 帧定界」：
// Send 在线上的字节必须是 4B 大端长度前缀 + 裸 payload。
//
// 为什么值得单独钉：往返测试对「不做帧定界的裸透传」实现同样会绿（单条短消息在
// net.Pipe 上碰巧等价），只有直接断言线上字节才能证明它确实走了 tcpConn 的帧协议——
// 而帧协议正是 mux 依赖的互操作契约（对端按同一格式解析），不是实现细节。
func TestFromNetConn_WireFormatIsLengthPrefixed(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	cb := builtin.FromNetConn(b)
	payload := []byte("abc")
	go func() { _ = cb.Send(context.Background(), payload) }()

	// 裸端是同步 pipe：帧格式不对时 ReadFull 会等满 7 字节而永不返回，加读 deadline
	// 让回归在 5s 内以错误收场（而不是把包挂到 go test 全局超时）。
	if err := a.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	wire := make([]byte, 4+len(payload))
	if _, err := io.ReadFull(a, wire); err != nil {
		t.Fatalf("读取线上字节: %v", err)
	}
	if got := binary.BigEndian.Uint32(wire[:4]); got != uint32(len(payload)) {
		t.Fatalf("长度前缀应为 %d（大端）, got %d", len(payload), got)
	}
	if !bytes.Equal(wire[4:], payload) {
		t.Fatalf("payload 应为 %q, got %q", payload, wire[4:])
	}
}

// TestFromNetConn_RejectsOversizedMessage 验证桥接复用 tcpConn 的**接收侧**单条上限
// （maxMessageBytes = 1 MiB）：对端发超大长度前缀时必须报错（fail-closed），既不
// 触发巨型分配，也不静默截断。
//
// 为什么不从 xfer.Conn 侧构造这个场景：tcpConn.Send **没有**长度校验（上限只在
// Receive 侧），经 Send 发超大消息的语义是「照写」，在 net.Pipe（同步、无缓冲）
// 上还会直接阻塞到 60s 写超时——那样测到的是写超时，不是上限拒绝。故此处按
// tcp_test.go:TestTcpReceive_RejectsOversizedMessage 的既有做法，直接向裸 net.Conn
// 写 4B 超大长度前缀（不写 body），钉住的才是接收端的上限检查。
//
// 注：简报原稿断言的 `ca.Send(ctx, make([]byte, 1<<20+1))` 会返回错误——该断言会绿，
// 但绿在「对端 60s 写超时/连接关闭」而非「超限拒绝」，属断言与实测不符，已改为本用例。
func TestFromNetConn_RejectsOversizedMessage(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	cb := builtin.FromNetConn(b)
	go func() {
		frame := make([]byte, 4)
		binary.BigEndian.PutUint32(frame, 0xFFFFFFFF)
		_, _ = a.Write(frame)
	}()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := cb.Receive(ctx)
	if err == nil {
		t.Fatal("超大长度前缀应返回错误（fail-closed），不得静默分配/截断")
	}
	// 帧协议已破坏，连接必须被标记关闭：mux readLoop 依赖 errors.Is(err, ErrConnClosed)
	// 判定终态并干净退出（否则会把坏帧当瞬时错误重试）。
	if !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("超限错误应包装 ErrConnClosed（供 mux 判终态）, got %v", err)
	}
}

// TestFromNetConn_CloseFailsSubsequentOps 验证桥接后的关闭语义：Close 幂等，
// 且关闭后 Send/Receive 立即返回 xfer.ErrConnClosed（不触碰底层管道）。
func TestFromNetConn_CloseFailsSubsequentOps(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	cb := builtin.FromNetConn(b)
	if err := cb.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := cb.Close(); err != nil {
		t.Fatalf("Close 应幂等（二次调用返回 nil）: %v", err)
	}
	if _, err := cb.Receive(context.Background()); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("关闭后 Receive 应返回 ErrConnClosed, got %v", err)
	}
	if err := cb.Send(context.Background(), []byte("x")); !errors.Is(err, xfer.ErrConnClosed) {
		t.Fatalf("关闭后 Send 应返回 ErrConnClosed, got %v", err)
	}
}
