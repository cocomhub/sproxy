// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

// errors_retry_internal_test.go 钉住 HTTP 状态码的重试分类与 Retry-After 建议
// （2026-10-30）：5xx / 429（限流）/ 408（请求超时）是瞬时条件，必须可重试；
// 429/503 的 `Retry-After` 随错误携带，供管理器替代固定退避。

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func respWith(code int, retryAfter string) *http.Response {
	h := http.Header{}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: code, Header: h}
}

// TestStatusError_RetryClassification：5xx/429/408 可重试；4xx 其余不可重试。
//
// 变异：删掉 429/408 分支（只留 5xx）⇒ 本用例红。
func TestStatusError_RetryClassification(t *testing.T) {
	t.Parallel()
	d := &HTTPDownloader{}
	retryable := []int{
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable,
		http.StatusGatewayTimeout, http.StatusTooManyRequests, http.StatusRequestTimeout,
	}
	for _, code := range retryable {
		err := d.statusError(respWith(code, ""))
		if _, ok := errors.AsType[*RetryableError](err); !ok {
			t.Fatalf("status %d 应可重试, got %v", code, err)
		}
	}
	permanent := []int{
		http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden,
		http.StatusNotFound, http.StatusGone,
	}
	for _, code := range permanent {
		err := d.statusError(respWith(code, ""))
		if _, ok := errors.AsType[*RetryableError](err); ok {
			t.Fatalf("status %d 不应可重试, got %v", code, err)
		}
	}
}

// TestStatusError_RetryAfterHeader：`Retry-After` 秒数被解析并随错误携带；无效值 ⇒ 0。
func TestStatusError_RetryAfterHeader(t *testing.T) {
	t.Parallel()
	d := &HTTPDownloader{}
	if got := RetryAfterOf(d.statusError(respWith(http.StatusTooManyRequests, "2"))); got != 2*time.Second {
		t.Fatalf("Retry-After=2 应解析为 2s, got %v", got)
	}
	if got := RetryAfterOf(d.statusError(respWith(http.StatusServiceUnavailable, "bogus"))); got != 0 {
		t.Fatalf("无效 Retry-After 应为 0, got %v", got)
	}
	if got := RetryAfterOf(d.statusError(respWith(http.StatusBadRequest, "2"))); got != 0 {
		t.Fatalf("不可重试错误不应带 RetryAfter, got %v", got)
	}
}
