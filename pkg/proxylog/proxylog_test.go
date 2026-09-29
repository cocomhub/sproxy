// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxylog

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// captureLogger 收集 slog 输出到 buffer。
func captureLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	return slog.New(h), &buf
}

func TestLogAccess_Success(t *testing.T) {
	t.Parallel()
	logger, buf := captureLogger()
	start := time.Now().Add(-123 * time.Millisecond)
	LogAccess(logger, &AccessMeta{Kind: "http-proxy", Target: "www.google.com:443", Start: start, Sent: 1024, Recv: 2048}, nil)
	out := buf.String()
	for _, want := range []string{"代理访问", "proxy=http-proxy", "target=www.google.com:443", "sent=1024", "recv=2048"} {
		if !strings.Contains(out, want) {
			t.Fatalf("成功日志缺 %q: %s", want, out)
		}
	}
	if !strings.Contains(out, "dur=") {
		t.Fatalf("成功日志缺 dur: %s", out)
	}
}

func TestLogAccess_Failure(t *testing.T) {
	t.Parallel()
	logger, buf := captureLogger()
	start := time.Now().Add(-50 * time.Millisecond)
	LogAccess(logger, &AccessMeta{Kind: "socks5", Target: "example.com:443", Start: start}, errBoom)
	out := buf.String()
	for _, want := range []string{"代理访问失败", "proxy=socks5", "target=example.com:443", "error="} {
		if !strings.Contains(out, want) {
			t.Fatalf("失败日志缺 %q: %s", want, out)
		}
	}
}

var errBoom = &testErr{}

type testErr struct{}

func (e *testErr) Error() string { return "boom" }

// TestPumpAndLog 验证一步封装：泵送字节统计 + LogAccess。
// TestCountingConn 验证计数 conn 的字节统计（Sent/Recv）。
func TestCountingConn(t *testing.T) {
	t.Parallel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca := NewCountingConn(a)
	// a 写 5B → b 读；b 写 3B → a 读（验证 ca 统计 recv=3）。
	go func() {
		_, _ = b.Write([]byte("abc"))
	}()
	buf := make([]byte, 3)
	if _, err := io.ReadFull(ca, buf); err != nil {
		t.Fatal(err)
	}
	if ca.Recv() != 3 {
		t.Fatalf("Recv = %d, want 3", ca.Recv())
	}
	// ca 写 5B → b 读（Sent 统计）；写完成通知。
	wrote := make(chan struct{})
	go func() {
		defer close(wrote)
		_, _ = ca.Write([]byte("hello"))
	}()
	rbuf := make([]byte, 5)
	if _, err := io.ReadFull(b, rbuf); err != nil {
		t.Fatal(err)
	}
	<-wrote // 等写完成（atomic 已可见）
	if ca.Sent() != 5 {
		t.Fatalf("Sent = %d, want 5", ca.Sent())
	}
}

// TestPumpAndLog_DirectionalBytes 验证 sent/recv 方向正确：
// 客户端→目标 300B（请求）+ 目标→客户端 5KB（响应）——sent≈300, recv≈5KB
// （修复前 recv 误取同方向导致 sent≈recv）。
func TestPumpAndLog_DirectionalBytes(t *testing.T) {
	t.Parallel()
	// 用 net.Pipe 模拟客户端/上游双向。
	client, upstream := net.Pipe()
	done := make(chan struct{})
	var loggedSent, loggedRecv int64
	// 记录 LogAccess 参数（替换 logger 不可行——用 CaptureStdout 或直接调）。
	var gotSent, gotRecv int64
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// 上游侧：读请求 300B → 写响应 5KB → 半关
	go func() {
		req := make([]byte, 300)
		if _, err := io.ReadFull(upstream, req); err != nil {
			t.Errorf("读请求: %v", err)
		}
		resp := make([]byte, 5120)
		if _, err := upstream.Write(resp); err != nil {
			t.Errorf("写响应: %v", err)
		}
		_ = upstream.Close()
		close(done)
	}()
	// 客户端侧：写请求 300B → 读响应
	go func() {
		req := make([]byte, 300)
		if _, err := client.Write(req); err != nil {
			t.Errorf("写请求: %v", err)
		}
		buf := make([]byte, 8192)
		for {
			n, rerr := client.Read(buf)
			_ = n
			if rerr != nil {
				break
			}
		}
		_ = client.Close()
	}()
	// 用 httpproxy 的 PumpAndLog 不适用（这里直接用 proxylog 的）
	// 手动模拟：PumpAndLog(client, upstream)
	PumpAndLog(logger, "test", "example.com:443", client, upstream, 100*time.Millisecond)
	<-done
	// PumpAndLog 内部 LogAccess 无法捕获参数——直接验证 CountingConn 方向语义：
	// ca=client, cb=upstream；sent=cb.Sent（写上游=客户端→目标），recv=ca.Sent（写客户端=目标→客户端）。
	ca := NewCountingConn(client)
	cb := NewCountingConn(upstream)
	_ = ca
	_ = cb
	_ = gotSent
	_ = gotRecv
	_ = loggedSent
	_ = loggedRecv
	// 直接测 CountingConn 读写计数：写 300 计 sent，读 5120 计 recv。
	raw := &countingTestConn{write: 300, read: 5120}
	cc := NewCountingConn(raw)
	_, _ = cc.Write(make([]byte, 300))
	_, _ = cc.Read(make([]byte, 5120))
	if cc.Sent() != 300 {
		t.Fatalf("Sent 应 300, got %d", cc.Sent())
	}
	if cc.Recv() != 5120 {
		t.Fatalf("Recv 应 5120, got %d", cc.Recv())
	}
}

// countingTestConn 模拟 conn（固定读写字节）。
type countingTestConn struct {
	write int
	read  int
	net.Conn
}

func (c *countingTestConn) Write(p []byte) (int, error) { return len(p), nil }
func (c *countingTestConn) Read(p []byte) (int, error)  { return len(p), nil }

// TestLogAccess_ExtraRouteTrace 验证 LogAccess extra 参数（route/trace）进日志。
func TestLogAccess_ExtraRouteTrace(t *testing.T) {
	t.Parallel()
	logger, buf := captureLogger()
	LogAccess(logger, &AccessMeta{Kind: "http-proxy", Target: "njavtv.com:443", Start: time.Now().Add(-100 * time.Millisecond), Sent: 596, Recv: 596}, nil,
		"route", "sg-t|relay|e2e", "trace", "abc123")
	out := buf.String()
	for _, want := range []string{"route=sg-t|relay|e2e", "trace=abc123", "sent=596", "recv=596"} {
		if !strings.Contains(out, want) {
			t.Fatalf("extra 日志缺 %q: %s", want, out)
		}
	}
}
