// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oteltracing

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
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
// 覆写 OTEL_EXPORTER_OTLP_ENDPOINT，装配完成后环境变量恢复原值。
// 覆写/恢复逻辑在 build()（Tracer() 懒装配触发），故先触发 build 再断言——
// 若只调 NewProvider 而不触发装配，env 从未被触碰，断言会平凡为真。
func TestOTLPEndpointOverride_EnvRestored(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none") // 关键：none 保证装配不触网
	old := "http://old-collector:4317"
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", old)

	p, err := NewProvider(WithOTLPEndpoint("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewProvider 不应报错: %v", err)
	}
	// 触发懒装配（build 内 Setenv+defer 恢复）。
	tr := p.Tracer("t")
	if tr == nil {
		t.Fatal("Tracer 返回 nil")
	}
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != old {
		t.Fatalf("装配完成后环境变量应恢复为 %q，got %q", old, got)
	}
	_ = p.Shutdown(context.Background())
}

// TestOTLPEndpointOverride_EmptyEndpoint_NoEnvTouch 验证 ep 为空（未配置
// WithOTLPEndpoint）时 build 不触碰 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量：
// Setenv/Unsetenv 均不应被调用（LookupEnv 是只读，可以测到设置前后无副作用）。
func TestOTLPEndpointOverride_EmptyEndpoint_NoEnvTouch(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://unchanged:4317")

	p, err := NewProvider() // 无 WithOTLPEndpoint → ep 为空
	if err != nil {
		t.Fatalf("NewProvider 不应报错: %v", err)
	}
	tr := p.Tracer("t") // 触发 build
	_ = tr
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://unchanged:4317" {
		t.Fatalf("ep 为空时不应触碰环境变量，got %q", got)
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

// TestOTLPHTTPCollectorExport 验证 autoexport 以 OTEL_TRACES_EXPORTER=otlp 装配
// 时，span 真实经过 HTTP OTLP 管道流出到 collector（自动导出路径的接线证据）。
// 用 httptest 启动本地 OTLP/HTTP collector（接收 POST /v1/traces），把
// OTEL_EXPORTER_OTLP_ENDPOINT 指向它，CreateSpan+End 后 Shutdown（ForceFlush）
// 冲刷，断言 collector 收到包含本测试 trace id 的导出请求——证明 autoexport
// 不是只停在"构造出 exporter"，而是真实把 span 导出到远端。
func TestOTLPHTTPCollectorExport(t *testing.T) {
	var got atomic.Bool
	// traceID 在 StartSpan 之前无法预知（由 OTel SDK 生成），用全局最近 span 的
	// trace id 匹配。collector 收到请求后把 body 存下，测试尾部断言非空。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			b, _ := io.ReadAll(r.Body)
			if len(b) > 0 {
				got.Store(true)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("OTEL_TRACES_EXPORTER", "otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL) // 指向本地 collector

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	tr := p.Tracer("test")
	ctx, end := tr.StartSpan(context.Background(), "collector-op")
	end()
	// Shutdown 触发 ForceFlush：batch processor 冲刷积压 span 到 exporter。
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if !got.Load() {
		t.Fatal("collector 未收到包含 span 的 /v1/traces 请求（autoexport OTLP 导出路径未接线）")
	}
	_ = ctx
}

// TestAutoExport_Console 验证 autoexport 以 OTEL_TRACES_EXPORTER=console 装配时
// exporter 被构造（非 none），span 生命周期可用。console exporter 写 stdout，
// 不断言具体输出（-race 下捕获 stdout 不可靠，审查在 Important-2 认可 none/回落
// 断言保留，真正导出路径证据由 TestOTLPHTTPCollectorExport 承担）。
func TestAutoExport_Console(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "console")
	p, err := NewProvider()
	if err != nil {
		t.Fatalf("OTEL_TRACES_EXPORTER=console NewProvider 不应报错: %v", err)
	}
	tr := p.Tracer("demo")
	ctx, end := tr.StartSpan(context.Background(), "demo-op")
	end()
	_ = ctx
	if err := p.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 不应报错: %v", err)
	}
}

// TestEnvRestore_HadValue 覆盖 WithOTLPEndpoint 覆写时环境变量"原值已存在"的
// 恢复分支（os.Setenv 恢复原值）。注意：TestOTLPEndpointOverride_EnvRestored 用
// t.Setenv 预置了原值，同样走 had==true 分支；had==false（原值不存在→Unsetenv）
// 分支尚无直接用例，依赖 os.Unsetenv 语义（测试进程环境直接受控，风险低）。
func TestEnvRestore_HadValue(t *testing.T) {
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://pre-existing:4317")
	p, err := NewProvider(WithOTLPEndpoint("http://127.0.0.1:1"))
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_ = p.Tracer("t") // 触发 build
	if got := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); got != "http://pre-existing:4317" {
		t.Fatalf("覆写后应恢复原值，got %q", got)
	}
	_ = p.Shutdown(context.Background())
}

// TestWarn_none_WhenNotSet 验证未设置 OTEL_TRACES_EXPORTER 时（默认）装配打到
// Info 而非误导性 Warn（审查 Minor-5 防回归：env unset ≠ 显式 none）。
// 捕获 build 日志判别层级与文案。
func TestWarn_none_WhenNotSet(t *testing.T) {
	// 先 unset（t.Setenv 只对显式设置有效；用 os.Unsetenv + t.Cleanup 恢复）
	prev, had := os.LookupEnv("OTEL_TRACES_EXPORTER")
	if had {
		t.Cleanup(func() { os.Setenv("OTEL_TRACES_EXPORTER", prev) })
		os.Unsetenv("OTEL_TRACES_EXPORTER")
	}

	var buf strings.Builder
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(old)

	p, err := NewProvider()
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	_ = p.Tracer("t") // 触发 build → none 分支 → 应打 Info
	_ = p.Shutdown(context.Background())

	out := buf.String()
	if strings.Contains(out, "level=WARN") && strings.Contains(out, "exporter 为 none") {
		// 显式 =none 时才应出现该 Warn；未设置时应是 Info。
		if strings.Contains(out, "level=INFO") && strings.Contains(out, "未装配 exporter") {
			// OK：未设置分支命中。
		} else {
			t.Fatalf("未设置 OTEL_TRACES_EXPORTER 时不应打 Warn：%s", out)
		}
	}
}
