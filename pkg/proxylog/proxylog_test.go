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
	LogAccess(logger, "http-proxy", "www.google.com:443", start, 1024, 2048, nil)
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
	LogAccess(logger, "socks5", "example.com:443", start, 0, 0, errBoom)
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
