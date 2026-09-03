// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package oteltracing

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"sync"

	core "github.com/cocomhub/sproxy/pkg/telemetry"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// otelEndpointEnv 是 OTLP 端点的标准环境变量名。
const otelEndpointEnv = "OTEL_EXPORTER_OTLP_ENDPOINT"

// Option 是 Provider 装配选项。
type Option func(*providerOptions)

// providerOptions 是 NewProvider 的可配置项（经 Option 函数式装配）。
type providerOptions struct {
	// sampleRatio 是 ParentBased(TraceIDRatioBased) 采样率，∈ (0,1]，默认 1.0。
	sampleRatio float64
	// otlpEndpoint 非空时在装配期临时覆写 OTEL_EXPORTER_OTLP_ENDPOINT 环境
	// 变量（仅对 autoexport.NewSpanExporter 生效，之后恢复）——配置级显式覆写
	// 标准环境约定。空 = 仅走标准环境变量。
	otlpEndpoint string
}

func defaultProviderOptions() providerOptions {
	return providerOptions{sampleRatio: 1.0}
}

// WithSampleRatio 设置采样率（ParentBased(TraceIDRatioBased)，∈ (0,1]）。
// 越界值不在此处校验，由 NewProvider 返回错误（fail-fast，见 brief）。
func WithSampleRatio(ratio float64) Option {
	return func(o *providerOptions) { o.sampleRatio = ratio }
}

// WithOTLPEndpoint 显式指定 OTLP 端点（http(s)://host:port）。仅在 autoexport
// 装配期间临时覆写 OTEL_EXPORTER_OTLP_ENDPOINT 环境变量（随后恢复原值），
// 优先级高于标准环境变量；空 = 不覆写，完全由环境变量驱动。URL 校验
// （http/https + host 非空）在 NewProvider 阶段执行，非法返回 error。
// 注意：endpoint 覆写属装配期一次性关注，用 Provider 自身互斥保护并发装配。
func WithOTLPEndpoint(url string) Option {
	return func(o *providerOptions) { o.otlpEndpoint = url }
}

// Provider 是 OpenTelemetry TracerProvider 的装配门面：把核心
// telemetry.Tracer 接口与真实 OTel SDK（autoexport 驱动的 exporter + 采样器）
// 连接起来。Shutdown 幂等（sync.Once）。
type Provider struct {
	tp    *sdktrace.TracerProvider
	once  sync.Once
	mu    sync.Mutex // 串行化懒装配（tracerLocked）与 Shutdown 读取 tp
	ratio float64    // 解析后的采样率
	ep    string     // 解析后的 OTLP 端点覆写（空 = 仅环境变量）
}

// NewProvider 装配一个 TracerProvider：
//   - sampler = ParentBased(TraceIDRatioBased(sampleRatio))；
//   - exporter 由 autoexport.NewSpanExporter 按标准环境变量
//     （OTEL_TRACES_EXPORTER=otlp|console|none，OTEL_EXPORTER_OTLP_ENDPOINT 等）
//     构造，BatchSpanProcessor 接 TracerProvider；
//   - OTEL_TRACES_EXPORTER=none / autoexport 失败 → 仅进程内模式
//     （无 span processor，Tracer() 仍可用）+ Warn（不回退静默，brief 要求）；
//   - WithOTLPEndpoint 用于配置级覆写端点（装配期临时设置环境变量后恢复）。
//
// 只对非法选项值（采样率越界 / endpoint 非 http(s)）返回 error——"未配置
// endpooint"是合法的仅进程内模式，不视为错误（provider 必须不因无端点硬失败）。
func NewProvider(opts ...Option) (*Provider, error) {
	o := defaultProviderOptions()
	for _, opt := range opts {
		opt(&o)
	}
	if o.sampleRatio <= 0 || o.sampleRatio > 1 {
		return nil, fmt.Errorf("sample ratio %v 必须 ∈ (0,1]", o.sampleRatio)
	}
	if o.otlpEndpoint != "" {
		u, err := url.Parse(o.otlpEndpoint)
		if err != nil {
			return nil, fmt.Errorf("otlp endpoint %q 非法: %v", o.otlpEndpoint, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("otlp endpoint scheme %q 无效，仅允许 http/https", u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("otlp endpoint 缺少 host: %q", o.otlpEndpoint)
		}
	}
	return &Provider{ratio: o.sampleRatio, ep: o.otlpEndpoint}, nil
}

// Tracer 返回一个实现核心 telemetry.Tracer 的适配器（包装真实 OTel tracer）。
// 永不为 nil；懒装配 TracerProvider（首次调用时构建 exporter）。
// Shutdown 之后的 Tracer() 调用返回 no-op 包装（OTel SDK 对已关闭的
// TracerProvider 返回 no-op tracer），不新建、不泄漏新 provider。
func (p *Provider) Tracer(name string) core.Tracer {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tp == nil {
		p.tp = p.build(context.Background())
	}
	return New(p.tp.Tracer(name))
}

// Shutdown 停止 TracerProvider 并冲刷待导出 span。幂等：多次调用只执行一次。
// Shutdown 在 Tracer() 之前调用也不会在后续 Tracer() 泄漏新 provider——
// tp 保持 nil，后续 Tracer() 按懒装配路径构建（见 Tracer）。
func (p *Provider) Shutdown(ctx context.Context) error {
	var err error
	p.once.Do(func() {
		p.mu.Lock()
		tp := p.tp
		p.mu.Unlock()
		if tp == nil {
			return
		}
		if cerr := tp.Shutdown(ctx); cerr != nil {
			err = cerr
		}
	})
	return err
}

// build 装配 TracerProvider：exporter 经 autoexport 环境变量驱动。
//  1. ep 非空（WithOTLPEndpoint） → 临时设置 OTEL_EXPORTER_OTLP_ENDPOINT，
//     defer 恢复原值（环境变量覆写属装配期一次性关注，p.mu 已串行化）；
//  2. autoexport.NewSpanExporter(ctx) 失败或返回 none exporter → Warn +
//     无 span processor（仅进程内，Tracer() 仍可用）；
//  3. 否则 BatchSpanProcessor 接 TracerProvider。
//
// 仅在装配期调用一次（Tracer 的懒装配路径缓存 tp），竞态由 p.mu 保护。
func (p *Provider) build(ctx context.Context) *sdktrace.TracerProvider {
	// 环境变量覆写：仅对本自动装配调用生效，defer 恢复原值。
	if p.ep != "" {
		prev, had := os.LookupEnv(otelEndpointEnv)
		if err := os.Setenv(otelEndpointEnv, p.ep); err != nil {
			slog.Warn("设置 OTEL_EXPORTER_OTLP_ENDPOINT 失败，忽略配置级覆写", "endpoint", p.ep, "error", err)
		} else {
			defer func() {
				if had {
					_ = os.Setenv(otelEndpointEnv, prev)
				} else {
					_ = os.Unsetenv(otelEndpointEnv)
				}
			}()
		}
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(p.ratio))
	exp, err := autoexport.NewSpanExporter(ctx)
	// envSet 区分"未设置 OTEL_TRACES_EXPORTER"与"显式 =none"：
	// autoexport 对两者都返回 none exporter，但运营语义不同——未设置时不应
	// 打出"被显式关停"的误导性 Warn（审查 Minor-5）。
	_, envSet := os.LookupEnv("OTEL_TRACES_EXPORTER")
	if err != nil {
		// autoexport 构造失败（如 OTEL_TRACES_EXPORTER 为未知值）：仅进程内
		// 模式继续，Tracer() 仍可用；Warn 记录原因，不回退静默（brief 要求）。
		slog.Warn("autoexport 创建 span exporter 失败，回退仅进程内（in-process only）", "error", err)
		return sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	}
	if autoexport.IsNoneSpanExporter(exp) {
		// OTEL_TRACES_EXPORTER 未设或 =none：仅进程内模式（无 span processor）。
		// 未设置时是默认行为（纯 slog），显式 =none 是运维关闭；文案不误导。
		if !envSet {
			slog.Info("telemetry 未装配 exporter（OTEL_TRACES_EXPORTER 未设置），仅进程内（in-process only）")
		} else {
			slog.Warn("span exporter 为 none（OTEL_TRACES_EXPORTER=none），仅进程内（in-process only）")
		}
		return sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
	}

	slog.Info("OpenTelemetry span exporter 已装配", "sampler", "parentbased_traceidratio", "sample_ratio", p.ratio)
	return sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sampler),
		sdktrace.WithBatcher(exp),
	)
}
