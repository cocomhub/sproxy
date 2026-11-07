// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remotefs

// accept_loop.go 是**只读面 / 写面共用的 accept 循环**。
//
// 两面刻意「反向同构」（同授权、同错误映射、同审计），但循环骨架只有一份实现：
// 此前 ReadListener / WriteListener 各持一份 `acceptLoop` + `handleAcceptError`
// （逐行相同，仅日志前缀与 serve 函数不同）——那正是「修一面忘另一面」的温床
// （本仓已有先例：R38 只扫 HandleFunc、R35-a 只扫 internal/）。
//
// 差异点收敛为两个参数：`site`（日志前缀，如 "remote_read"）与 `serve`（每连接的
// 处理函数）。**行为与日志文案逐字保持**：瞬时错误按 backoff 退避重试、致命错误先关
// listener 再退出、ctx 取消即刻退出、竞态窗口内的连接丢弃并退出。

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// acceptRunner 是一次 accept 循环的装配参数。
type acceptRunner struct {
	ln     net.Listener
	logger *slog.Logger
	// site 是日志前缀（"remote_read" / "remote_write"），保持既有日志文案不变。
	site string
	wg   *sync.WaitGroup
	// serve 处理单条已接受连接（两面各自的 mux + 路由表装配）。
	serve func(ctx context.Context, c net.Conn, id *tunnel.Identity, staticKey []byte, pins []string)
	// done 在循环退出时关闭（确定性信号：accept 已停止）。
	done chan struct{}
}

// run 执行 accept 循环，直到 ctx 取消或遇到致命错误。
func (r *acceptRunner) run(ctx context.Context, id *tunnel.Identity, staticKey []byte, pins []string) {
	defer close(r.done)
	// ctx 取消 → 关闭 listener（解除 accept 阻塞）。循环因其它原因退出时由 defer 关闭
	// stop 通道回收本 goroutine，不泄漏。
	stopCtxWatch := make(chan struct{})
	defer close(stopCtxWatch)
	go watchAcceptCtx(ctx, r.ln, stopCtxWatch)

	backoff := time.Duration(0)
	for {
		conn, err := r.ln.Accept()
		if err != nil {
			var stop bool
			if backoff, stop = r.handleError(ctx, err, backoff); stop {
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
		r.wg.Add(1)
		go r.serve(ctx, conn, id, staticKey, pins)
	}
}

// handleError 处理 accept 错误：按「预期停机 / 瞬时可退避 / 致命」分类。
// 返回 (backoff, stop)：stop=true 表示本次 accept 循环应退出（错误已处理）。
func (r *acceptRunner) handleError(ctx context.Context, err error, backoff time.Duration) (time.Duration, bool) {
	if ctx.Err() != nil {
		return backoff, true // 预期停机：ctx 已/将关闭 listener
	}
	if retryableAcceptError(err) {
		backoff = nextAcceptBackoff(backoff)
		r.logger.Warn(r.site+" accept 瞬时错误，退避重试", "error", err, "backoff", backoff)
		if !sleepCtx(ctx, backoff) {
			return backoff, true
		}
		return backoff, false
	}
	// 致命错误：先关闭 listener 再退出，避免「仍绑定但无人 accept」
	// （内核 backlog 会继续收握手，表现为端口仍可连却无人服务）。
	r.logger.Warn(r.site+" accept 退出", "error", err)
	_ = r.ln.Close()
	return backoff, true
}
