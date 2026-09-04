// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package telemetry 提供轻量级 OpenTelemetry 式观测骨架：核心 trace 类型
// （SpanContext/Span/Tracer/Carrier）内置基于标准库 log/slog 的实现，
// 零外部依赖；同时为未来扩展 metric/log 观测类型预留命名空间
// （telemetry 是比 tracing 更广的 umbrella，server/client 均可消费）。
package telemetry
