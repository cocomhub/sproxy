// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package verify

import (
	"context"
	"testing"
)

// TestVerifyOnce_HookRunsBeforePass 测试钩子在持 busy 后、巡检前调用；
// 巡检返回时必须已释放单飞标记（否则后续巡检恒 Concurrent）。
func TestVerifyOnce_HookRunsBeforePass(t *testing.T) {
	t.Parallel()
	f := newTenantFixture(t, "alice", map[string]string{"user/a.txt": "aaa"})
	d := f.deps()
	var calls int
	d.Hook = func() { calls++ }
	svc := New(d)

	rep := svc.VerifyOnce(context.Background(), "", "alice")
	if calls != 1 {
		t.Fatalf("Hook 调用次数 = %d，want 1", calls)
	}
	if rep.Total != 1 || rep.Ok != 1 {
		t.Fatalf("rep=%+v，want total=1 ok=1", rep)
	}

	again := svc.VerifyOnce(context.Background(), "", "alice")
	if again.Concurrent || again.Ok != 1 {
		t.Fatalf("首次巡检后应已释放单飞标记，got %+v", again)
	}
	if calls != 2 {
		t.Fatalf("第二次巡检 Hook 调用累计 = %d，want 2", calls)
	}
}
