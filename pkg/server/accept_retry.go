// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"
)

// accept 退避参数：瞬时错误（fd/内存耗尽类）下按指数退避重试，上限 1s。
const (
	minAcceptBackoff = 5 * time.Millisecond
	maxAcceptBackoff = time.Second
)

// retryableAcceptError 判定 Accept 返回的错误是否属于「可重试的瞬时错误」。
//
// 为什么需要：Go 的 net 层只在内部重试 EINTR / EAGAIN / ECONNABORTED
// （见 internal/poll/fd_unix.go），**EMFILE / ENFILE / ENOBUFS / ENOMEM 会原样上抛**。
// 若 accept 循环把这些当成致命错误直接退出且不关闭 listener，listener 会保持绑定、
// 内核 backlog 继续完成握手，但再没有任何 Accept——表现为「端口仍可连、服务已死」。
// 该模式与 net/http.Server.Serve 的处理一致（瞬时错误退避重试，其余退出）。
//
// net.ErrClosed 明确不算瞬时：它是 listener 被关闭的正常退出信号。
func retryableAcceptError(err error) bool {
	if err == nil || errors.Is(err, net.ErrClosed) {
		return false
	}
	switch {
	case errors.Is(err, syscall.EMFILE),
		errors.Is(err, syscall.ENFILE),
		errors.Is(err, syscall.ENOBUFS),
		errors.Is(err, syscall.ENOMEM):
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// nextAcceptBackoff 返回下一次退避时长（5ms 起，逐次翻倍，1s 封顶）。
func nextAcceptBackoff(prev time.Duration) time.Duration {
	if prev <= 0 {
		return minAcceptBackoff
	}
	next := prev * 2
	if next > maxAcceptBackoff {
		return maxAcceptBackoff
	}
	return next
}

// sleepCtx 等待 d 后返回 true；ctx 取消则立即返回 false（供退避重试提前收敛）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
