// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
)

// TestRetryableCloudFailure 锁定「哪些失败类别值得自动重试」（2026-11-21 设计 §3.3）。
func TestRetryableCloudFailure(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		cloud.ErrClassSourceUnreachable: true,
		cloud.ErrClassThrottled:         true,
		cloud.ErrClassContentMismatch:   true,
		cloud.ErrClassTargetUnavailable: true,  // 装配顺序/后端抖动可自愈
		cloud.ErrClassTargetConflict:    false, // 需人工/显式覆盖
		cloud.ErrClassDiskFull:          false, // 需人工扩容
		cloud.ErrClassAuth:              false, // 需人工处理凭据
		cloud.ErrClassUnknown:           false,
	}
	for class, want := range cases {
		if got := retryableCloudFailure(class); got != want {
			t.Errorf("retryableCloudFailure(%q) = %v, want %v", class, got, want)
		}
	}
}
