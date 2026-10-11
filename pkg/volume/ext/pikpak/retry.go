// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"net/http"
	"time"
)

// 幂等请求（GET/HEAD）的退避重试默认值。背景（2026-10-11 实测）：
// 取 FETCH 直链的 DownloadLink 单次 TLS 握手超时（DefaultTransport 的
// TLSHandshakeTimeout=10s）会让整个多 GB 混合下载任务失败——控制面请求必须容忍瞬断。
const (
	defaultRetryCount   = 3                      // 默认重试次数（总尝试 = 1 + 该值）
	defaultRetryBackoff = 400 * time.Millisecond // 退避基数（指数增长）
	maxRetryBackoff     = 5 * time.Second        // 单次退避上限
	retryJitterDiv      = 4                      // 抖动比例分母（1/4 内抖动，防并发齐步重试）
)

// retryPolicy 是幂等请求的退避重试策略（零值 = 默认：3 次 / 400ms 基数）。
// MaxRetries 是**重试次数**（不含首次）；<=0 → 默认；Backoff <=0 → 默认。
type retryPolicy struct {
	MaxRetries int
	Backoff    time.Duration
}

// attempts 返回总尝试次数（含首次）。
func (p retryPolicy) attempts() int {
	n := p.MaxRetries
	if n <= 0 {
		n = defaultRetryCount
	}
	return n + 1
}

// delay 返回第 retry 次重试前的退避时长（retry 从 1 起）：指数增长 + 抖动，封顶。
func (p retryPolicy) delay(retry int) time.Duration {
	base := p.Backoff
	if base <= 0 {
		base = defaultRetryBackoff
	}
	d := base << (retry - 1)
	if retry <= 0 || d <= 0 || d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d + time.Duration(rand.Int63n(int64(d/retryJitterDiv)+1)) //nolint:gosec // 退避抖动非安全用途
}

// retryableRequestErr 判断请求错误是否值得重试：传输层错误（net.Error/url.Error
// 包装的 TLS 握手超时、连接重置、EOF）可重试；ctx 结束与业务错误不可重试。
func retryableRequestErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[net.Error](err); ok {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

// retryableStatus 判断 HTTP 状态码是否值得重试（限流 + 服务端临时故障）。
func retryableStatus(code int) bool {
	return code == http.StatusTooManyRequests || code >= http.StatusInternalServerError
}

// sleepCtx 是可被 ctx 中断的睡眠；返回 false 表示 ctx 已结束（调用方立即返回）。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// doWithRetry 执行幂等请求并退避重试。fn 返回 (retryable, err)：
//   - err == nil → 成功返回；
//   - retryable=false 或 ctx 结束 → 立即返回该错误（不重试）；
//   - retryable=true → 退避后重试，直到尝试次数用尽（返回最后一次错误）。
//
// what 仅用于日志（如 "GET /drive/v1/files/x"）；log 可为 nil（不记日志）。
func doWithRetry(ctx context.Context, p retryPolicy, log *slog.Logger, what string, fn func() (bool, error)) error {
	// 首轮前先检查 ctx：已取消/超时时不发起注定失败的请求（也避免无谓的握手消耗）。
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	total := p.attempts()
	var lastErr error
	for attempt := 1; attempt <= total; attempt++ {
		if attempt > 1 && !sleepCtx(ctx, p.delay(attempt-1)) {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		retryable, err := fn()
		if err == nil {
			return nil
		}
		lastErr = err
		// ctx 已结束（用户中断/超时）→ 上报 ctx 语义，不把中断伪装成网络错误。
		if ctx.Err() != nil {
			return fmt.Errorf("%s: %w", what, ctx.Err())
		}
		if !retryable {
			return err
		}
		if log != nil {
			log.Warn("pikpak request retry", "what", what, "attempt", attempt, "of", total, "err", err)
		}
	}
	return lastErr
}
