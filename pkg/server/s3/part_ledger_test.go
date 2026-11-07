// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
)

// TestS3PartLedger_ReserveCommitRelease 账本句语的纯语义：预留失败整体回滚、提交转已确认、
// 释放钳 0（不会把账本压成负数）。
func TestS3PartLedger_ReserveCommitRelease(t *testing.T) {
	t.Parallel()
	// 两侧账本分别建池：真实部署里 chunk 桶 Scope 属于 owner 配额树，卷池是另一个实例
	// （此处用两个独立 Pool 精确断言各自增量，避免同源池把两笔预留叠加看待）。
	scopePool := quota.NewPool(10)
	volPool := quota.NewPool(10)
	led := &PartLedger{Scope: scopePool.Scope("chunk", 10), Pool: volPool}

	resv := led.Reserve(4)
	if resv == nil {
		t.Fatalf("4 字节在 10 上限内应预留成功")
	}
	if got := scopePool.Reserved(); got != 4 {
		t.Fatalf("Scope 预留=%d, want 4", got)
	}
	if got := volPool.Reserved(); got != 4 {
		t.Fatalf("卷池预留=%d, want 4", got)
	}
	resv.Commit(led, 4)
	if got, reserved := scopePool.Usage(), scopePool.Reserved(); got != 4 || reserved != 0 {
		t.Fatalf("commit 后 scope usage=%d reserved=%d, want 4/0", got, reserved)
	}
	if got, reserved := volPool.Usage(), volPool.Reserved(); got != 4 || reserved != 0 {
		t.Fatalf("commit 后卷池 usage=%d reserved=%d, want 4/0", got, reserved)
	}

	if got := led.Reserve(100); got != nil {
		t.Fatalf("超额预留应失败（返回 nil）")
	}
	if got, reserved := scopePool.Usage(), scopePool.Reserved(); got != 4 || reserved != 0 {
		t.Fatalf("预留失败后不得改动 scope 账本: usage=%d reserved=%d", got, reserved)
	}
	if got, reserved := volPool.Usage(), volPool.Reserved(); got != 4 || reserved != 0 {
		t.Fatalf("预留失败后不得改动卷池账本: usage=%d reserved=%d", got, reserved)
	}

	led.ReleaseCommitted(1000) // 释放大于已确认 → 钳 0
	if got := scopePool.Usage(); got != 0 {
		t.Fatalf("超量释放后 scope usage=%d, want 0（钳 0）", got)
	}
	if got := volPool.Usage(); got != 0 {
		t.Fatalf("超量释放后卷池 usage=%d, want 0（钳 0）", got)
	}
	led.ReleaseCommitted(5) // 幂等：已为 0 不再下溢
	if got := scopePool.Usage(); got != 0 {
		t.Fatalf("重复释放后 scope usage=%d, want 0", got)
	}
	if !led.Active() {
		t.Fatalf("ledger 应视为已接线")
	}
	var empty PartLedger
	if empty.Active() {
		t.Fatalf("空 ledger 应 no-op")
	}
}
