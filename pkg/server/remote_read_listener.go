// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// RemoteReadListener 是 B 侧跨节点只读面的 loopback listener（Y 一期 AD-6）。
//
// 它把「本节点被授权读的 owner 命名空间」经一条**真握手、真加密、双向 pin** 的隧道
// 暴露给被授权的对端节点（A 侧见 T6 的 pkg/remote）。只读强制由 remoteReadHandler
// 的手写 GET/HEAD 白名单提供，本文件只负责传输与身份。
type RemoteReadListener struct {
	ln     net.Listener
	logger *slog.Logger
	cfg    *Config
	h      *Handlers

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// Addr 返回实际监听地址（listen 端口为 0 时供测试读取真实端口）。
func (l *RemoteReadListener) Addr() string { return l.ln.Addr().String() }

// Close 停止接受新连接并等待已派生的连接 goroutine 收敛。
//
// 已建立连接的收敛依赖建立它的 ctx 取消（调用方传入的 ctx）：ctx 取消后隧道 Serve
// 的 accept 循环立即返回，故 wg.Wait 不会等 TCP 读错误重试退避。
func (l *RemoteReadListener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.ln.Close() })
	l.wg.Wait()
	return err
}

// StartRemoteReadListener 按 cfg.RemoteRead 起只读面监听；未启用时返回 (nil, nil)。
//
// 装配要点：
//   - 监听强制 loopback（Validate 已保证；此处再以实际 Addr 断言，纵深防御）；
//   - B 侧身份复用既有服务端 xfer 身份（LoadXferIdentity，hub.xfer_identity_file），
//     A 侧 pin 的即该身份指纹——无新增配置键、无新增秘密；
//   - 静态密钥由 listener 自己的身份指纹派生（DeriveRemoteStaticKey）。该值由**公开**
//     指纹派生、不是秘密，且在 Tunnel 中兼作 dialer 侧握手失败的回退加密密钥——故
//     **双向 pin 是 fail-closed 硬前提**：本函数在无任何 mesh_readers 指纹时拒绝启动，
//     每连接建 Tunnel 时也恒传 WithPeerFingerprints(pins)（未配 pin 就接受任意对端会
//     让「静态密钥回退不可达」的安全论证失效）。**不得删除这两处**；
//   - 对端 pin 列表 = 全部卷 mesh_readers 指纹去重（任一被授权节点均可连入）。
func StartRemoteReadListener(ctx context.Context, cfg *Config, h *Handlers, log *slog.Logger) (*RemoteReadListener, error) {
	if cfg == nil || !cfg.RemoteRead.Enabled {
		return nil, nil
	}
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", cfg.RemoteRead.Listen)
	if err != nil {
		return nil, fmt.Errorf("remote_read 监听失败: %w", err)
	}
	if host, _, sErr := net.SplitHostPort(ln.Addr().String()); sErr != nil || !isLoopbackHost(host) {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 拒绝启动：实际监听地址非 loopback (%s)", ln.Addr())
	}
	id, err := LoadXferIdentity(cfg)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 身份加载失败: %w", err)
	}
	pins := meshReaderFingerprints(cfg)
	if len(pins) == 0 {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_read 拒绝启动：无任何 mesh_readers 指纹（fail-closed，无 pin 将接受任意对端）")
	}

	l := &RemoteReadListener{ln: ln, logger: log, cfg: cfg, h: h}
	staticKey := tunnel.DeriveRemoteStaticKey(id.Fingerprint())
	log.Info("remote_read 只读面已启动",
		"listen", ln.Addr().String(), "fingerprint", id.Fingerprint(), "pinned_readers", len(pins))

	l.wg.Go(func() {
		l.acceptLoop(ctx, id, staticKey, pins)
	})
	return l, nil
}

// acceptLoop 接受连接并为每连接建 mux + Tunnel（每连接一个只读路由表：指纹是连接级属性）。
func (l *RemoteReadListener) acceptLoop(ctx context.Context, id *tunnel.Identity, staticKey []byte, pins []string) {
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			if ctx.Err() == nil {
				l.logger.Warn("remote_read accept 退出", "error", err)
			}
			return
		}
		l.wg.Add(1)
		go func(c net.Conn) {
			defer l.wg.Done()
			defer func() { _ = c.Close() }()
			m := mux.New(builtin.FromNetConn(c), mux.RoleListener)
			defer func() { _ = m.Close() }()
			tun := tunnel.NewTunnel(m, staticKey,
				tunnel.WithIdentity(id),
				tunnel.WithPeerFingerprints(pins),
				tunnel.WithHandshakeTimeout(l.cfg.RemoteRead.HandshakeTimeout),
			)
			handler := l.h.newRemoteReadHandler(tun)
			if sErr := tun.Serve(ctx, handler); sErr != nil {
				if ctx.Err() == nil {
					l.logger.Warn("remote_read 连接结束", "remote", c.RemoteAddr().String(), "error", sErr)
				}
			}
		}(conn)
	}
}

// meshReaderFingerprints 返回所有卷 mesh_readers 的指纹（归一化后去重）。
//
// 归一化走 tunnel.ParseFingerprint（权威规范形）；解析失败的条目退回「去空白 + 小写」
// 原样保留——非法指纹已由 Config.Validate 响亮拒绝，此处不静默丢弃（丢弃会让
// 「配置了一条却未生效」变成静默失败）。
func meshReaderFingerprints(cfg *Config) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 4)
	for _, v := range cfg.Volumes {
		if v.ACL == nil {
			continue
		}
		for _, mr := range v.ACL.MeshReaders {
			fp := strings.ToLower(strings.TrimSpace(mr.Fingerprint))
			if norm, err := tunnel.ParseFingerprint(mr.Fingerprint); err == nil {
				fp = norm
			}
			if fp == "" {
				continue
			}
			if _, dup := seen[fp]; dup {
				continue
			}
			seen[fp] = struct{}{}
			out = append(out, fp)
		}
	}
	return out
}
