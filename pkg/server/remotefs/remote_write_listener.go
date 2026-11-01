// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remotefs

// remote_write_listener.go 是 B 侧跨节点**写面**的 loopback listener（Y 二期 P3-b2）。
//
// 与只读 listener（remote_read_listener.go）**同构**，差别只有三处、且都是为了写安全：
//  1. 开关与监听地址独立（`remote_write` 段）——写面可单独关闭而不影响只读同步；
//  2. **pin 列表只取 scope 授予写的指纹**（只读对端连写面的握手都过不了：物理上不给出
//     写通路，而非「连上再拒」）；
//  3. 路由表换成 `newRemoteWriteHandler`（另一张白名单：只注册 4 条 POST 写 op）。

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/config"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
)

// WriteListener 是 B 侧写面的 loopback listener。
type WriteListener struct {
	ln     net.Listener
	logger *slog.Logger
	cfg    *config.Config
	d      Deps

	// acceptDone 在 acceptLoop 退出时关闭（确定性信号：accept 已停止）。
	acceptDone chan struct{}

	wg        sync.WaitGroup
	closeOnce sync.Once
}

// Addr 返回实际监听地址（listen 端口为 0 时供测试读取真实端口）。
func (l *WriteListener) Addr() string { return l.ln.Addr().String() }

// Close 停止接受新连接并等待已派生的连接 goroutine 收敛（语义同 RemoteReadListener）。
func (l *WriteListener) Close() error {
	var err error
	l.closeOnce.Do(func() { err = l.ln.Close() })
	l.wg.Wait()
	return err
}

// StartWriteListener 按 cfg.RemoteWrite 起写面监听；未启用时返回 (nil, nil)。
//
// 装配要点（与只读面逐条对应）：
//   - 监听强制 loopback（Validate 已保证；此处再以实际 Addr 断言，纵深防御）；
//   - 身份复用既有服务端 xfer 身份（LoadXferIdentity）——无新增配置键、无新增秘密；
//   - 静态密钥由 listener 自己的身份指纹派生（DeriveRemoteStaticKey）。**双向 pin 是
//     fail-closed 硬前提**：本函数在无任何「授写」指纹时拒绝启动，每连接亦恒传
//     WithPeerFingerprints(writePins)（未配 pin 就接受任意对端会让静态密钥回退的安全
//     论证失效）。**不得删除这两处**；
//   - pin 列表 = **scope 授予写**（write/rw）的条目指纹去重。只读条目**不进**写面 pin：
//     只读对端因此连握手都建立不了（写通路物理上不存在）。
func StartWriteListener(ctx context.Context, d Deps) (*WriteListener, error) {
	cfg := d.Config
	if cfg == nil || !cfg.RemoteWrite.Enabled {
		return nil, nil
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", cfg.RemoteWrite.Listen)
	if err != nil {
		return nil, fmt.Errorf("remote_write 监听失败: %w", err)
	}
	if host, _, sErr := net.SplitHostPort(ln.Addr().String()); sErr != nil || !config.IsLoopbackHost(host) {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_write 拒绝启动：实际监听地址非 loopback (%s)", ln.Addr())
	}
	id, err := d.Runtime.Identity()
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_write 身份加载失败: %w", err)
	}
	pins := config.MeshWriterFingerprints(cfg)
	if len(pins) == 0 {
		_ = ln.Close()
		return nil, fmt.Errorf("remote_write 拒绝启动：没有任何 mesh_readers 条目的 scope 授予写（fail-closed，无 pin 将接受任意对端）")
	}

	l := &WriteListener{ln: ln, logger: log, cfg: cfg, d: d, acceptDone: make(chan struct{})}
	staticKey := tunnel.DeriveRemoteStaticKey(id.Fingerprint())
	log.Info("remote_write 写面已启动",
		"listen", ln.Addr().String(), "fingerprint", id.Fingerprint(), "pinned_writers", len(pins),
		"identity_file", d.Runtime.IdentityPath())

	l.wg.Go(func() {
		l.acceptLoop(ctx, id, staticKey, pins)
	})
	return l, nil
}

// acceptLoop 接受连接并为每连接建 mux + Tunnel（与只读面同构；路由表换写 handler）。
func (l *WriteListener) acceptLoop(ctx context.Context, id *tunnel.Identity, staticKey []byte, pins []string) {
	defer close(l.acceptDone)
	// ctx 取消 → 关闭 listener（解除 Accept 阻塞）。acceptLoop 因其它原因退出时由
	// defer 关闭 stop 通道回收本 goroutine，不泄漏。
	stopCtxWatch := make(chan struct{})
	defer close(stopCtxWatch)
	go watchAcceptCtx(ctx, l.ln, stopCtxWatch)

	backoff := time.Duration(0)
	for {
		conn, err := l.ln.Accept()
		if err != nil {
			var stop bool
			if backoff, stop = l.handleAcceptError(ctx, err, backoff); stop {
				return
			}
			continue
		}
		backoff = 0
		if ctx.Err() != nil {
			// 竞态窗口：ctx 已取消但 watcher 尚未 Close 时 accept 到的连接——丢弃并退出。
			_ = conn.Close()
			return
		}
		l.wg.Add(1)
		go l.serveWriteConn(ctx, conn, id, staticKey, pins)
	}
}

// handleAcceptError 处理 accept 错误：按「预期停机 / 瞬时可退避 / 致命」分类。
// 返回 (backoff, stop)：stop=true 表示本次 accept 循环应退出（错误已处理）。
func (l *WriteListener) handleAcceptError(ctx context.Context, err error, backoff time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return backoff, true // 预期停机：ctx 已/将关闭 listener
	}
	if retryableAcceptError(err) {
		backoff = nextAcceptBackoff(backoff)
		l.logger.Warn("remote_write accept 瞬时错误，退避重试", "error", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return backoff, true
		}
		return backoff, false
	}
	// 致命错误：先关闭 listener 再退出，避免「仍绑定但无人 accept」。
	l.logger.Warn("remote_write accept 退出", "error", err)
	_ = l.ln.Close()
	return backoff, true
}

// serveWriteConn 为单条已接受连接建 mux + Tunnel（写路由表）并 serve。
func (l *WriteListener) serveWriteConn(ctx context.Context, c net.Conn, id *tunnel.Identity, staticKey []byte, pins []string) {
	defer l.wg.Done()
	defer func() { _ = c.Close() }()
	m := mux.NewWithOpts(builtin.FromNetConn(c), mux.RoleListener, l.d.Runtime.MuxOptions()...)
	defer func() { _ = m.Close() }()
	tun := tunnel.NewTunnel(m, staticKey,
		tunnel.WithIdentity(id),
		tunnel.WithPeerFingerprints(pins),
		tunnel.WithHandshakeTimeout(l.cfg.RemoteWrite.HandshakeTimeout),
	)
	handler := NewWriteHandler(l.d, tun)
	if sErr := tun.Serve(ctx, handler); sErr != nil {
		if ctx.Err() == nil {
			l.logger.Warn("remote_write 连接结束", "remote", c.RemoteAddr().String(), "error", sErr)
		}
	}
}
