// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package proxylog 提供代理会话访问日志的通用 helper（httpproxy / socks5 复用）：
// 每次成功/失败的代理请求（CONNECT / 转发）记录 目标地址 + 耗时 + 字节量，
// 供常驻 sclient（http-proxy / socks）确认「哪些请求经过」。
//
// 日志级别：
//   - 成功：Info（常驻默认可见——计划任务重定向日志文件即可查）
//   - 失败：Warn（含错误原因）
//   - 详细（头/方法等）：Debug（--verbose 或 handler 级别过滤）
package proxylog

import (
	"log/slog"
	"time"
)

// ProxyKind 是代理类型标识（日志字段 proxy=）。
type ProxyKind string

const (
	KindHTTPProxy ProxyKind = "http-proxy"
	KindSOCKS5    ProxyKind = "socks5"
)

// LogAccess 记录一次代理请求的访问日志。
//
//	成功（err == nil）：logger.Info("代理访问", proxy, target, dur, sent, recv)
//	失败（err != nil）：logger.Warn("代理访问失败", proxy, target, dur, error)
//
// target 是代理目标（如 "www.google.com:443"）；sent/recv 是双向字节量（CONNECT
// 隧道可能只统计到拨号前为 0——由调用方决定是否统计泵送阶段）。
func LogAccess(logger *slog.Logger, kind ProxyKind, target string, start time.Time, sent, recv int64, err error) {
	if logger == nil {
		logger = slog.Default()
	}
	attrs := []any{"proxy", kind, "target", target, "dur", time.Since(start).Round(time.Millisecond)}
	if err != nil {
		attrs = append(attrs, "error", err)
		logger.Warn("代理访问失败", attrs...)
		return
	}
	attrs = append(attrs, "sent", sent, "recv", recv)
	logger.Info("代理访问", attrs...)
}
