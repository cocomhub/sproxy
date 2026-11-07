// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// retry_delay_internal_test.go 钉住重试等待时长的选择（2026-10-30）：
// 服务端 `Retry-After` 优先（封顶 maxRetryAfter），否则用配置的固定 RetryDelay。

import (
	"testing"
	"time"
)

// TestRetryDelay_PrefersRetryAfter：Retry-After 优先且封顶。
//
// 变异：忽略 retryAfter（恒返回 base）⇒ 本用例红。
func TestRetryDelay_PrefersRetryAfter(t *testing.T) {
	t.Parallel()
	const base = 30 * time.Second
	cases := []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"未指定用配置", 0, base},
		{"服务端更短", 5 * time.Second, 5 * time.Second},
		{"服务端更长", 2 * time.Minute, 2 * time.Minute},
		{"异常超长封顶", time.Hour, maxRetryAfter},
	}
	for _, tc := range cases {
		if got := retryDelay(base, tc.retryAfter); got != tc.want {
			t.Fatalf("%s: retryDelay(%v, %v)=%v want %v", tc.name, base, tc.retryAfter, got, tc.want)
		}
	}
}
