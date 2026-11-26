// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"testing"
	"time"
)

// TestExpiredCloudTaskTTL_CancelledUsesTaskTTL 锁定取消任务的保留期（2026-11-26）：
// 取消 = 中断 ⇒ 断点与配额保留，但只保留 `TaskTTL`（默认 24h）；failed 才用 FailedTaskTTL（7d）。
//
// 理由：几十 GiB 断点若按 7d 占配额，会把后续任务挤成 507；取消是主动动作，通常当天就会
// 续传或删除。
func TestExpiredCloudTaskTTL_CancelledUsesTaskTTL(t *testing.T) {
	t.Parallel()
	m := &CloudDownloadManager{config: &CloudDownloadConfig{
		TaskTTL:       24 * time.Hour,
		FailedTaskTTL: 7 * 24 * time.Hour,
	}}
	if got, ok := m.expiredCloudTaskTTL(&CloudTask{Status: "cancelled"}); !ok || got != 24*time.Hour {
		t.Fatalf("cancelled TTL = %v (ok=%v), want 24h", got, ok)
	}
	if got, ok := m.expiredCloudTaskTTL(&CloudTask{Status: "failed"}); !ok || got != 7*24*time.Hour {
		t.Fatalf("failed TTL = %v (ok=%v), want 168h", got, ok)
	}
	if got, ok := m.expiredCloudTaskTTL(&CloudTask{Status: "completed"}); !ok || got != 24*time.Hour {
		t.Fatalf("completed TTL = %v (ok=%v), want 24h", got, ok)
	}
}
