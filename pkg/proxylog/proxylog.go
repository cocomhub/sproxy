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
	"net"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/iostream"
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

// CountingConn 统计读写字节的 net.Conn 包装（代理访问日志 sent/recv 用）。
// 并发安全（atomic）；读/写侧各自累计，泵送双向同时进行。
// httpproxy / socks5 共用（避免两处重复实现）。
type CountingConn struct {
	net.Conn
	sent atomic.Int64
	recv atomic.Int64
}

// NewCountingConn 包装 conn 并返回计数版。
func NewCountingConn(conn net.Conn) *CountingConn { return &CountingConn{Conn: conn} }

func (c *CountingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.recv.Add(int64(n))
	return n, err
}

func (c *CountingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.sent.Add(int64(n))
	return n, err
}

// Sent 是写向目标的字节量（上游视角）；Recv 是目标返回的字节量。
func (c *CountingConn) Sent() int64 { return c.sent.Load() }
func (c *CountingConn) Recv() int64 { return c.recv.Load() }

// PumpAndLog 是「双向泵送 + 访问日志」一步封装（httpproxy/socks5 通用）：
// 用 CountingConn 包 a/b 后 iostream.Pump，结束后 LogAccess（sent/recv）。
// 供代理命令记录「哪些请求经过」——统一入口避免调用方各自组装。
func PumpAndLog(logger *slog.Logger, kind ProxyKind, target string, a, b net.Conn, grace time.Duration) {
	start := time.Now()
	ca := NewCountingConn(a)
	cb := NewCountingConn(b)
	iostream.Pump(ca, cb, grace)
	// sent = 本端写到目标的字节（cb.Sent），recv = 目标返回本端的（ca.Recv）。
	LogAccess(logger, kind, target, start, cb.Sent(), ca.Recv(), nil)
}
