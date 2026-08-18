// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import "fmt"

// RetryableError 表示可重试的瞬时错误（网络中断、读取超时、5xx 等）。
// 管理器在重试循环中通过 errors.As 识别该类型；确定性错误（4xx、SSRF、
// 路径/写入错误等）不应包装为 RetryableError，避免无意义的重试。
type RetryableError struct {
	Err error
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
