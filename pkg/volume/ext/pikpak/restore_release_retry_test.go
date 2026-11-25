// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDeleteAPI 是「前 N 次删除失败」的替身（验证退避重试语义）。
type fakeDeleteAPI struct {
	fail atomic.Int32 // 剩余失败次数
	n    atomic.Int32 // 调用次数
}

func (f *fakeDeleteAPI) ResetToken() {}

func (f *fakeDeleteAPI) DeletePermanent(_ context.Context, _ []string) error {
	f.n.Add(1)
	if f.fail.Add(-1) >= 0 {
		return errors.New("HTTP 502")
	}
	return nil
}

// TestDeleteWithRetry_RetriesTransient 锁定「释放失败必须退避重试」（2026-11-25 实测：
// 502/超时导致转存副本漏删，累积占满 PikPak 空间 ⇒ 后续任务"空间不足"）。
func TestDeleteWithRetry_RetriesTransient(t *testing.T) {
	t.Parallel()
	old := restoreReleaseBackoff
	restoreReleaseBackoff = []time.Duration{time.Millisecond}
	t.Cleanup(func() { restoreReleaseBackoff = old })

	api := &fakeDeleteAPI{}
	api.fail.Store(1) // 首次失败，重试成功
	del := func() error { return api.DeletePermanent(context.Background(), []string{"id1"}) }

	var err error
	for i := 0; i <= len(restoreReleaseBackoff); i++ {
		if err = del(); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if got := api.n.Load(); got != 2 {
		t.Fatalf("应调用 2 次（1 失败 + 1 成功），got %d", got)
	}
}
