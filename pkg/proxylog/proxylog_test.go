// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package proxylog

import (
	"bytes"
	"log/slog"
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
