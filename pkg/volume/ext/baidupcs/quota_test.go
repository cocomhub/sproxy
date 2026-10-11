// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestQuota_ReserveRelease 预留 → 释放 → 用量归零。
func TestQuota_ReserveRelease(t *testing.T) {
	t.Parallel()
	q := NewQuota(1024)
	if err := q.Reserve(400); err != nil {
		t.Fatalf("Reserve(400): %v", err)
	}
	if err := q.Reserve(600); err != nil {
		t.Fatalf("Reserve(600): %v", err)
	}
	if got := q.Used(); got != 1000 {
		t.Fatalf("Used = %d, want 1000", got)
	}
	q.Release(400)
	q.Release(600)
	if got := q.Used(); got != 0 {
		t.Fatalf("Used after release = %d, want 0", got)
	}
}

// TestQuota_ExceedLimit 超限返回错误（拒绝继续写 staging）。
func TestQuota_ExceedLimit(t *testing.T) {
	t.Parallel()
	q := NewQuota(100)
	if err := q.Reserve(100); err != nil {
		t.Fatalf("Reserve(100): %v", err)
	}
	if err := q.Reserve(1); err == nil {
		t.Fatal("超限 Reserve 应报错")
	}
}

// TestQuota_Unlimited 上限 0 = 不限。
func TestQuota_Unlimited(t *testing.T) {
	t.Parallel()
	q := NewQuota(0)
	for range 1000 {
		if err := q.Reserve(1024); err != nil {
			t.Fatalf("不限上限应全部允许: %v", err)
		}
	}
}

// TestQuota_CacheCap cache 独立上限：超 cap 拒绝。
func TestQuota_CacheCap(t *testing.T) {
	t.Parallel()
	cacheQuota := NewQuota(500)
	if err := cacheQuota.Reserve(500); err != nil {
		t.Fatalf("Reserve(500): %v", err)
	}
	if err := cacheQuota.Reserve(1); err == nil {
		t.Fatal("cache 超 cap 应拒绝")
	}
}

// TestQuota_ReleaseOverflow 释放超过已用量不 panic、归零。
func TestQuota_ReleaseOverflow(t *testing.T) {
	t.Parallel()
	q := NewQuota(1024)
	_ = q.Reserve(10)
	q.Release(9999)
	if got := q.Used(); got != 0 {
		t.Fatalf("Used = %d, want 0", got)
	}
}

// fakeQuotaTracker 是 QuotaTracker 的内存实现（测试记账断言）。
type fakeQuotaTracker struct {
	reserved atomic.Int64
	released atomic.Int64
	failRes  atomic.Bool // true = ReserveUsage 返回错误
}

func (q *fakeQuotaTracker) ReserveUsage(_ context.Context, size int64) error {
	if q.failRes.Load() {
		return errQuotaTestFail
	}
	q.reserved.Add(size)
	return nil
}

func (q *fakeQuotaTracker) ReleaseUsage(size int64) {
	q.released.Add(size)
}

var errQuotaTestFail = &testQuotaError{}

type testQuotaError struct{}

func (*testQuotaError) Error() string { return "quota reserve failed" }

func TestQuota_StagingReserveRelease(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	qt := &fakeQuotaTracker{}
	// 2026-10-10：staging 配额由 caller 侧 per-request 包装实例记账（卷共享单例不再注入）。
	g := syncpkg.WrapStagingQuota(fs, qt)

	if err := g.WriteFile(context.Background(), "f.txt", strings.NewReader("data"), 4, 0); err != nil {
		t.Fatal(err)
	}
	if qt.reserved.Load() != 4 {
		t.Fatalf("Reserved = %d, want 4", qt.reserved.Load())
	}
	if qt.released.Load() != 4 {
		t.Fatalf("Released = %d, want 4", qt.released.Load())
	}
}

func TestQuota_ReserveFail_Aborts(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	qt := &fakeQuotaTracker{}
	qt.failRes.Store(true)
	g := syncpkg.WrapStagingQuota(fs, qt)

	err := g.WriteFile(context.Background(), "f.txt", strings.NewReader("data"), 4, 0)
	if err == nil {
		t.Fatal("ReserveUsage 失败时 WriteFile 应报错")
	}
	// 未预留未释放（幂等）
	if qt.reserved.Load() != 0 || qt.released.Load() != 0 {
		t.Fatalf("reserve fail 后不应有记账: reserved=%d released=%d", qt.reserved.Load(), qt.released.Load())
	}
	// 预留失败 = 未落本地 staging（网盘不应出现该文件）。
	if e, serr := fs.Stat(context.Background(), "f.txt"); serr != nil || e != nil {
		t.Fatalf("预留失败不应上传, got e=%+v err=%v", e, serr)
	}
}

// 变异验证：去掉 gate 的 quota 释放 → 本测试应红。
func TestSyncFS_WriteFile_Quota_Tracked(t *testing.T) {
	t.Parallel()
	fs := newTestStorageFS(t)
	qt := &fakeQuotaTracker{}
	g := syncpkg.WrapStagingQuota(fs, qt)

	if err := g.WriteFile(context.Background(), "a/b.txt", strings.NewReader("hello"), 5, 0); err != nil {
		t.Fatal(err)
	}
	if qt.released.Load() != 5 {
		t.Fatalf("Released = %d, want 5（上传成功后本地占用应释放）", qt.released.Load())
	}
	// 文件确实上传（fake 里可见）
	e, err := fs.Stat(context.Background(), "a/b.txt")
	if err != nil || e == nil {
		t.Fatalf("上传后 Stat 应可见，got e=%+v err=%v", e, err)
	}
}
