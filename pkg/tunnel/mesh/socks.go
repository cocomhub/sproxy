// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"

	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/socks5"
)

// newLocalSocks 创建本地 SOCKS5 出口的监听器与服务器（mesh node --socks）：
// 本节点作为出口，CONNECT 目标由节点本机拨号（本地网络出口）。拆分供测试取实际
// 监听地址（SocksAddr 用 127.0.0.1:0 随机端口时）。
//
// 安全边界（对齐 mesh 网关 loopback-only + 认证）：
//   - 监听默认 loopback（NormalizeListenAddr，裸 :port → 127.0.0.1:port）；
//   - user 非空时要求 RFC 1929 用户名/密码认证（配置了才要求）。
func newLocalSocks(addr, user, pass string, logger *slog.Logger) (net.Listener, *socks5.Server, error) {
	addr = iostream.NormalizeListenAddr(addr)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, nil, fmt.Errorf("mesh socks: 监听失败: %w", err)
	}
	var auth func(u, p string) bool
	if user != "" || pass != "" {
		// 任一凭据配置即要求认证（防只配密码被静默禁用）。恒时比较对齐网关。
		auth = func(u, p string) bool {
			return subtle.ConstantTimeCompare([]byte(u), []byte(user)) == 1 &&
				subtle.ConstantTimeCompare([]byte(p), []byte(pass)) == 1
		}
	}
	return ln, socks5.New(socks5.Config{Auth: auth, Logger: logger}), nil
}

// serveLocalSocks 启动本地 SOCKS5 出口（阻塞直到 ctx 取消/监听器关闭）。
func serveLocalSocks(ctx context.Context, addr, user, pass string, logger *slog.Logger) error {
	ln, ss, err := newLocalSocks(addr, user, pass, logger)
	if err != nil {
		return err
	}
	// Accept 返回真实错误（非 ctx 取消）时 Serve 不关 ln，defer 保证 FD 释放。
	defer func() { _ = ln.Close() }()
	logger.Info("mesh SOCKS5 出口就绪", "addr", ln.Addr().String(), "auth_required", user != "")
	return ss.Serve(ctx, ln)
}
