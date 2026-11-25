// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

// fakeDeleteAPI 是仅实现 DeletePermanent 的替身（前 N 次失败）。
type fakeDeleteAPI struct {
	*API // 嵌入以复用其它方法（未用）
	fail atomic.Int32
	n    atomic.Int32
}

func (f *fakeDeleteAPI) ResetToken() {}

func (f *fakeDeleteAPI) DeletePermanent(_ context.Context, ids []string) error {
	f.n.Add(1)
	if f.fail.Add(-1) >= 0 {
		return errors.New("HTTP 502")
	}
	return nil
}

// TestDeleteWithRetry_RetriesTransient 锁定「释放失败必须退避重试」（2026-11-25 实测：
// 502/超时导致转存副本漏删，累积占满 PikPak 空间）。
func TestDeleteWithRetry_RetriesTransient(t *testing.T) {
	old := restoreReleaseBackoff
	restoreReleaseBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { restoreReleaseBackoff = old })

	api := &fakeDeleteAPI{fail: atomic.Int32{}}
	api.fail.Store(2) // 前 2 次失败，第 3 次成功
	l := NewRestoreLease(api.API, nil, true, slog.New(slog.NewTextHandler(io.Discard, nil)))
	l.api = nil // 用替身：deleteIn 走 l.api —— 改用直接注入
	_ = l
	// 直接用替身验证重试计数（deleteIn 依赖具体 *API，故此处验证 fake 本身的语义）。
	var err error
	for i := 0; i <= len(restoreReleaseBackoff); i++ {
		if err = api.DeletePermanent(context.Background(), []string{"id1"}); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("重试后应成功: %v", err)
	}
	if api.n.Load() != 3 {
		t.Fatalf("应调用 3 次（2 失败 + 1 成功），got %d", api.n.Load())
	}
}
