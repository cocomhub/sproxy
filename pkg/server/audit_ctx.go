// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_ctx.go 提供审计条目需要的**请求维度**提取器（评审 R13 P2）。
//
// 设计 §5.7 把 `request_id` / `source_ip` 列为审计读面的敏感字段，但此前生产装配只注入了
// Actor/Owner/Mesh ⇒ 条目里这两个字段恒为空（矩阵里却标 ✅，属虚标）。

import (
	"context"
	"net"
	"strings"

	"github.com/cocomhub/sproxy/pkg/telemetry"
)

// auditSourceIPCtxKey 是请求 ctx 中「客户端 IP」的私有 key（由 requestLog 中间件写入）。
type auditSourceIPCtxKey struct{}

// withAuditSourceIP 把客户端 IP 放进 ctx（供审计提取；空 IP 不写）。
func withAuditSourceIP(ctx context.Context, ip string) context.Context {
	if ip == "" {
		return ctx
	}
	return context.WithValue(ctx, auditSourceIPCtxKey{}, ip)
}

// auditSourceIPFrom 从 ctx 取客户端 IP（未注入返回空串）。
func auditSourceIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(auditSourceIPCtxKey{}).(string)
	return ip
}

// auditRequestIDFrom 从 ctx 取 trace id 作为 request_id（未启用 tracing 时返回空串）。
func auditRequestIDFrom(ctx context.Context) string {
	if sc, ok := telemetry.FromContext(ctx); ok {
		return sc.TraceID
	}
	return ""
}

// clientIPFromRemoteAddr 归一化 `host:port` 为 host（解析失败返回原值）。
func clientIPFromRemoteAddr(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return strings.TrimSpace(remoteAddr)
	}
	return host
}
