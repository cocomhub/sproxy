// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestStagingQuotaTrackerFor_CachedPerOwner P2 回归：同一 owner 必须复用同一
// StagingTracker 实例（含 sync.Cond）——否则每请求新建 cond，其它请求的 ReleaseUsage
// 广播无法唤醒等待者（排队固定罚满 deadline 甚至误报「空间不足」）。
func TestStagingQuotaTrackerFor_CachedPerOwner(t *testing.T) {
	t.Parallel()
	h := &Handlers{globalPool: quota.NewPool(1 << 20)}
	var cp atomic.Pointer[Config]
	cp.Store(Default())
	h.cfgPtr = &cp
	a1 := h.stagingQuotaTrackerFor("alice")
	a2 := h.stagingQuotaTrackerFor("alice")
	b1 := h.stagingQuotaTrackerFor("bob")
	if a1 == nil {
		t.Fatal("globalPool 非 nil 时应有 staging tracker")
	}
	if a1 != a2 {
		t.Fatal("同一 owner 必须复用同一 tracker 实例（跨请求唤醒）")
	}
	if b1 == nil || b1 == a1 {
		t.Fatal("不同 owner 应有独立 tracker")
	}
}
