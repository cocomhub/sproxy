// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/telemetry"
)

// captureClientLog 接管全局 slog default（FileClient 的默认 logger 取自 slog.Default()，
// 见 tracingLogger）并把 handler 级别设为 level，返回该级别下实际落地的日志。
func captureClientLog(t *testing.T, level slog.Level, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(telemetry.WithContextHandler(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: level}))))
	defer slog.SetDefault(old)
	fn()
	return buf.String()
}

// newTraceparentProbe 启动一个回环探测服务：把收到的 traceparent 经 buffered channel 回传
// （单元素 channel，容量足够，断言方不做额外同步等待）。
func newTraceparentProbe(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	received := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Traceparent")
		w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	return srv, received
}

// TestClientTracer_DefaultSilentAtInfoLevel 钉住客户端追踪的默认行为：FileClient 的默认
// tracer（telemetry.New）以 Debug 记录 span ⇒ 默认 Info 级下**静默**（此前每请求一行 INFO）；
// 但静默 ≠ 关闭——traceparent 仍注入，服务端 requestLogMiddleware 照旧能关联链路。
func TestClientTracer_DefaultSilentAtInfoLevel(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default 才能断言「默认级别下不打日志」。
	srv, received := newTraceparentProbe(t)

	output := captureClientLog(t, slog.LevelInfo, func() {
		c := NewFileClient(srv.URL)
		resp, err := c.doRequest(context.Background(), "GET", "/echo", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	})

	if strings.Contains(output, "[trace") {
		t.Fatalf("默认 Info 级下不应出现客户端 span 行，实际输出: %s", output)
	}
	tp := <-received
	if _, _, ok := telemetry.ParseTraceparent(tp); !ok {
		t.Fatalf("静默不等于关闭：默认仍应注入 traceparent，实际 %q", tp)
	}
}

// TestClientTracer_VisibleAtDebugLevel 是上一条的对照：能力没丢——把默认 logger 的级别
// 调到 Debug（sclient 即 `-v` / log_level: debug）就能看到 span 行。
func TestClientTracer_VisibleAtDebugLevel(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default（捕获取自全局 default）。
	srv, received := newTraceparentProbe(t)

	output := captureClientLog(t, slog.LevelDebug, func() {
		c := NewFileClient(srv.URL)
		resp, err := c.doRequest(context.Background(), "GET", "/echo", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	})
	<-received

	if !strings.Contains(output, "[trace") {
		t.Fatalf("Debug 级下应能看到客户端 span 行，实际输出: %q", output)
	}
	if !strings.Contains(output, "level=DEBUG") {
		t.Fatalf("span 行应为 DEBUG 级，实际输出: %s", output)
	}
	// 重复包装 context handler 会注入两份 trace_id/span_id；span 行必须恰好一份
	// （sclient 的 initLogger 已包装 slog.Default()，客户端默认 logger 又包一层）。
	for line := range strings.SplitSeq(output, "\n") {
		if !strings.Contains(line, "[trace") {
			continue
		}
		if n := strings.Count(line, "trace_id="); n != 1 {
			t.Fatalf("span 行的 trace_id 应恰好一份（重复包装会注入两份），实际 %d 份: %s", n, line)
		}
		if n := strings.Count(line, "span_id="); n != 1 {
			t.Fatalf("span 行的 span_id 应恰好一份，实际 %d 份: %s", n, line)
		}
	}
}

// TestClientTracer_UsesInjectedLogger 验证 WithLogger 能把默认 tracer 的落地点换成注入的
// logger（否则用户注入的 logger 关不掉默认 INFO span 行）。
func TestClientTracer_UsesInjectedLogger(t *testing.T) {
	t.Parallel()
	srv, received := newTraceparentProbe(t)

	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c := NewFileClient(srv.URL, WithLogger(lg))
	resp, err := c.doRequest(context.Background(), "GET", "/echo", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	<-received

	if !strings.Contains(buf.String(), "[trace") {
		t.Fatalf("注入的 logger 应收到 span 行，实际 buffer: %q", buf.String())
	}
}

// TestClientTracer_WithTracerNilIsNoop 钉住 WithTracer(nil) 的新语义：完全关闭追踪——
// 不 panic、不打 span 日志、不注入 traceparent（此前 nil 会回退默认 slog tracer）。
// 覆盖两种 Option 顺序（WithLogger 在前/在后），防止默认 tracer 被 WithLogger 复活。
func TestClientTracer_WithTracerNilIsNoop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts func(*slog.Logger) []Option
	}{
		{
			name: "WithTracer(nil) 在 WithLogger 之前",
			opts: func(lg *slog.Logger) []Option { return []Option{WithTracer(nil), WithLogger(lg)} },
		},
		{
			name: "WithTracer(nil) 在 WithLogger 之后",
			opts: func(lg *slog.Logger) []Option { return []Option{WithLogger(lg), WithTracer(nil)} },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv, received := newTraceparentProbe(t)

			var buf bytes.Buffer
			lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
			c := NewFileClient(srv.URL, tc.opts(lg)...)
			resp, err := c.doRequest(context.Background(), "GET", "/echo", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()

			if tp := <-received; tp != "" {
				t.Fatalf("WithTracer(nil) 不应注入 traceparent，实际 %q", tp)
			}
			if strings.Contains(buf.String(), "[trace") {
				t.Fatalf("WithTracer(nil) 不应打 span 日志，实际输出: %s", buf.String())
			}
		})
	}
}
