// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestTracerSpanLog_SilentAtInfoLevel 钉住「客户端追踪默认静默」：span 结束行以 Debug
// 记录 ⇒ 默认 Info 级 handler 下不产生任何输出（此前为 Info 级，SDK 每请求一行，
// 见 docs/architecture.md「客户端追踪（`pkg/client` + `pkg/telemetry`）」小节）。
func TestTracerSpanLog_SilentAtInfoLevel(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default（captureLogAt）才能断言默认级别下的输出。
	output := captureLogAt(t, slog.LevelInfo, func() {
		tr := New()
		_, end := tr.StartSpan(context.Background(), "silent-op")
		end()
	})
	if strings.Contains(output, "silent-op") {
		t.Fatalf("默认 Info 级下 span 不应打日志（默认静默），实际输出: %s", output)
	}
}

// TestTracerSpanLog_VisibleAtDebugLevel 是上一条的对照：能力没丢——handler 级别调到
// Debug 就能看到 span 行，且带 trace_id（链路关联信息保留）。
func TestTracerSpanLog_VisibleAtDebugLevel(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default（captureLogAt）才能断言 Debug 级下的输出。
	output := captureLogAt(t, slog.LevelDebug, func() {
		tr := New()
		_, end := tr.StartSpan(context.Background(), "visible-op")
		end()
	})
	if !strings.Contains(output, "visible-op") {
		t.Fatalf("Debug 级下应能看到 span 行，实际输出: %q", output)
	}
	if !strings.Contains(output, "trace_id=") {
		t.Fatalf("span 行应带 trace_id（链路关联），实际输出: %s", output)
	}
}

// TestTracer_WithLogger_Injected 验证 New(WithLogger(lg)) 把 span 日志落到注入的 logger，
// 而不是全局 slog.Default()——客户端 WithLogger 要能借此生效。
func TestTracer_WithLogger_Injected(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	tr := New(WithLogger(lg))
	_, end := tr.StartSpan(context.Background(), "injected-op")
	end()

	if !strings.Contains(buf.String(), "injected-op") {
		t.Fatalf("注入的 logger 未被使用，buffer: %q", buf.String())
	}
}

// TestTracer_WithLoggerNil_FallsBackToDefault 验证 WithLogger(nil) 回退 slog.Default()，
// 而不是 panic 或静默丢弃日志。
func TestTracer_WithLoggerNil_FallsBackToDefault(t *testing.T) {
	// sproxy:serial: 需要接管全局 slog default（captureLogAt）才能断言回退落地点。
	output := captureLogAt(t, slog.LevelDebug, func() {
		tr := New(WithLogger(nil))
		_, end := tr.StartSpan(context.Background(), "fallback-op")
		end()
	})
	if !strings.Contains(output, "fallback-op") {
		t.Fatalf("nil logger 应回退 slog.Default()，实际输出: %q", output)
	}
}

// TestNop_CompletelyOff 钉住 Nop() 是「完全关闭」：StartSpan 不建立 span（ctx 原样返回、
// 无 SpanContext）、end 可安全调用、Inject 不写 traceparent。
// 需要「静默但仍透传 traceparent」的场景用 New()（默认级别下静默），见 Nop 注释。
func TestNop_CompletelyOff(t *testing.T) {
	t.Parallel()
	tr := Nop()
	parent := context.Background()

	ctx, end := tr.StartSpan(parent, "nop-op")
	end()
	end() // 幂等，不应 panic

	if ctx != parent {
		t.Fatalf("Nop.StartSpan 应原样返回入参 ctx，实际被替换")
	}
	if sc, ok := FromContext(ctx); ok {
		t.Fatalf("Nop.StartSpan 不应注入 SpanContext，实际 %+v", sc)
	}
	var c mapCarrier = map[string]string{}
	tr.Inject(ctx, c)
	if tp, ok := c["traceparent"]; ok {
		t.Fatalf("Nop.Inject 不应写 traceparent，实际 %q", tp)
	}
}
