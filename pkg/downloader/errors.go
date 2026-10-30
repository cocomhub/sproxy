// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"errors"
	"fmt"
	"time"
)

// RetryableError 表示可重试的瞬时错误（网络中断、读取超时、5xx 等）。
// 管理器在重试循环中通过 errors.As 识别该类型；确定性错误（4xx、SSRF、
// 路径/写入错误等）不应包装为 RetryableError，避免无意义的重试。
type RetryableError struct {
	Err error
	// RetryAfter 是服务端建议的等待时长（`Retry-After` 头；0 = 未指定）。
	// 管理器据此替代固定退避，避免限流场景被固定延迟加剧或过早判定失败。
	RetryAfter time.Duration
}

// Error 实现 error 接口。
func (e *RetryableError) Error() string {
	if e.Err == nil {
		return "retryable download error"
	}
	return "retryable download error: " + e.Err.Error()
}

// Unwrap 支持 errors.Is/errors.As 展开。
func (e *RetryableError) Unwrap() error { return e.Err }

// retryablef 包装一个可重试错误。
func retryablef(format string, args ...any) error {
	return &RetryableError{Err: fmt.Errorf(format, args...)}
}

// retryableAfterf 包装可重试错误并携带服务端建议等待时长（Retry-After）。
func retryableAfterf(after time.Duration, format string, args ...any) error {
	return &RetryableError{Err: fmt.Errorf(format, args...), RetryAfter: after}
}

// RetryAfterOf 返回可重试错误携带的 Retry-After 建议等待时长（0 = 未指定/非可重试错误）。
func RetryAfterOf(err error) time.Duration {
	var re *RetryableError
	if errors.As(err, &re) {
		return re.RetryAfter
	}
	return 0
}
