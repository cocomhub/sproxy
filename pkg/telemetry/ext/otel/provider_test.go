// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oteltracing

import (
	"context"
	"os"
	"regexp"
	"testing"

	core "github.com/cocomhub/sproxy/pkg/telemetry"
)

// TestNewProvider_Default 验证默认装配（无任何 Option）：
//  1. NewProvider 不返回错误（"未配置 endpoint"是合法的仅进程内模式）；
//  2. Tracer("x") 返回非 nil 的 core.Tracer（Span 生命周期可用，不 panic）；
//  3. Shutdown 幂等（调用两次均不报错）。
func TestNewProvider_Default(t *testing.T) {
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider() 默认装配不应报错: %v", err)
	}
	tr := p.Tracer("x")
	if tr == nil {
		t.Fatal("Tracer(\"x\") 返回 nil")
	}
	ctx, end := tr.StartSpan(context.Background(), "op")
	end()
	_ = ctx

	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 第一次调用报错: %v", err)
	}
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 第二次调用应幂等: %v", err)
	}
}

// TestNewProvider_InvalidSampleRatio 验证非法采样率在 NewProvider 阶段报错：
// 0、负值与 1.5 越界；0.5 合法。
func TestNewProvider_InvalidSampleRatio(t *testing.T) {
	for _, ratio := range []float64{0, -0.1, 1.5} {
		if _, err := NewProvider(WithSampleRatio(ratio)); err == nil {
			t.Fatalf("WithSampleRatio(%v) 应报错", ratio)
		}
	}
	p, err := NewProvider(WithSampleRatio(0.5))
	if err != nil {
		t.Fatalf("WithSampleRatio(0.5) 应合法: %v", err)
	}
	_ = p.Shutdown(context.Background())
}

// TestNewProvider_InvalidEndpoint 验证 WithOTLPEndpoint 的 URL 校验：
// 非 URL → 报错（NewProvider 返回 error）；合法 http(s) URL 通过。
// URL 校验严格——"localhost:4318"（缺 scheme）应被拒绝。
func TestNewProvider_InvalidEndpoint(t *testing.T) {
	for _, ep := range []string{"not-a-url", "localhost:4318", "ftp://h:1", "http://"} {
		if _, err := NewProvider(WithOTLPEndpoint(ep)); err == nil {
			t.Fatalf("WithOTLPEndpoint(%q) 应报错", ep)
		}
	}
	p, err := NewProvider(WithOTLPEndpoint("http://localhost:4318"))
	if err != nil {
		t.Fatalf("WithOTLPEndpoint(http://localhost:4318) 应合法: %v", err)
	}
	_ = p.Shutdown(context.Background())
}

// TestStartSpan_WritesCoreSpanContext 验证 OTel tracer 的 StartSpan 把核心
// SpanContext（W3C 十六进制 trace_id/span_id）写入返回的 ctx：
// 经 telemetry.FromContext 读取，TraceID 32 hex、SpanID 16 hex。
// 该行为是 OTel ↔ slog 日志链路打通的关键（WithContextHandler 依赖 SpanContextKey）。
func TestStartSpan_WritesCoreSpanContext(t *testing.T) {
	p, err := NewProvider(WithSampleRatio(1.0))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()

	tr := p.Tracer("test")
	ctx, end := tr.StartSpan(context.Background(), "op")
	end()

	sc, ok := core.FromContext(ctx)
	if !ok {
		t.Fatal("StartSpan 返回的 ctx 未携带 core.SpanContext（SpanContextKey）")
	}
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(sc.TraceID) {
		t.Fatalf("TraceID = %q 不是 32 位十六进制", sc.TraceID)
	}
	if !regexp.MustCompile(`^[0-9a-f]{16}$`).MatchString(sc.SpanID) {
		t.Fatalf("SpanID = %q 不是 16 位十六进制", sc.SpanID)
	}
}

// TestAutoExport_None_NoError 验证 OTEL_TRACES_EXPORTER=none 时 provider 仍
// 正常装配（autoexport noop exporter，零错误）：span 可开始/结束，Shutdown 幂等。
func TestAutoExport_None_NoError(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("OTEL_TRACES_EXPORTER=none NewProvider 不应报错: %v", err)
	}
	tr := p.Tracer("demo")
	ctx, end := tr.StartSpan(context.Background(), "demo-op")
	end()
	_ = ctx
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 不应报错: %v", err)
	}
}

// TestOTLPEndpointOverride_EnvRestored 验证 WithOTLPEndpoint 在装配期间临时
// 覆写 OTEL_EXPORTER_OTLP_ENDPOINT，NewProvider 返回后环境变量恢复原值。
func TestOTLPEndpointOverride_EnvRestored(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none") // 关键：none 保证 NewProvider 不触网
	old := "http://old-collector:4317"
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", old)

	p, err := NewProvider(WithOTLPEndpoint("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewProvider 不应报错: %v", err)
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != old {
		t.Fatalf("装配后环境变量应恢复为 %q，got %q", old, got)
	}
	_ = p.Shutdown(context.Background())
}

// TestNewProvider_NoEndpoint_ValidInProcess 验证"未配置 endpoint"是合法的
// 仅进程内模式：NewProvider 不因无 endpoint 返回 error。
func TestNewProvider_NoEndpoint_ValidInProcess(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("未配置 endpoint 的 provider 应合法（仅进程内模式）: %v", err)
	}
	tr := p.Tracer("x")
	if tr == nil {
		t.Fatal("Tracer 返回 nil")
	}
	_ = p.Shutdown(context.Background())
}

// TestProvider_Shutdown_AfterTracer 验证多实例/重复使用场景下 Shutdown 与
// Tracer 交错调用不 panic（幂等 + 生命周期完整）。
func TestProvider_Shutdown_AfterTracer(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	tr := p.Tracer("t")
	ctx, end := tr.StartSpan(context.Background(), "a")
	end()
	_ = ctx
	_ = p.Shutdown(context.Background())
	_ = p.Shutdown(context.Background()) // 幂等
	_ = tr
}
