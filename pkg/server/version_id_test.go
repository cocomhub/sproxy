// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"sync"
	"testing"
	"time"
)

// TestNewVersionID_Positive 断言版本 ID 生成恒为正。
//
// 回归：旧实现 time.Now().UnixNano()*1000 中 UnixNano()≈1.76e18，×1000 后≈1.76e21
// 远超 int64 上限 9.22e18 → 回绕（约 213.5 天一轮、符号各半），当前时段生成的 ID 恒为负。
func TestNewVersionID_Positive(t *testing.T) {
	t.Parallel()

	const n = 10000
	for i := range n {
		if id := newVersionID(); id <= 0 {
			t.Fatalf("第 %d 次生成 version_id = %d，应为正数（int64 溢出回归）", i, id)
		}
	}
}

// TestNewVersionID_Unique 断言高频连续生成下版本 ID 不重复。
//
// 毫秒时间戳 ×1000 只提供同一毫秒内 1000 个随机后缀槽位，10000 次无节流调用会落在
// 同一毫秒内 → 纯随机后缀必然重复（实测 10000 次仅 1000 个唯一值、9000 次重复），
// 故生成器须在进程内保证单调唯一。
func TestNewVersionID_Unique(t *testing.T) {
	t.Parallel()

	const n = 10000
	seen := make(map[int64]struct{}, n)
	for i := range n {
		id := newVersionID()
		if _, dup := seen[id]; dup {
			t.Fatalf("第 %d 次生成 version_id = %d 与既有 ID 重复", i, id)
		}
		seen[id] = struct{}{}
	}
}

// TestNewVersionID_ConcurrentUnique 断言并发调用下版本 ID 仍唯一（-race 下同时校验数据竞争）。
func TestNewVersionID_ConcurrentUnique(t *testing.T) {
	t.Parallel()

	const (
		workers   = 8
		perWorker = 2000
	)
	results := make([][]int64, workers)
	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			ids := make([]int64, 0, perWorker)
			for range perWorker {
				ids = append(ids, newVersionID())
			}
			results[w] = ids
		})
	}
	wg.Wait()

	seen := make(map[int64]struct{}, workers*perWorker)
	for _, ids := range results {
		for _, id := range ids {
			if id <= 0 {
				t.Fatalf("并发生成的 version_id = %d，应为正数", id)
			}
			if _, dup := seen[id]; dup {
				t.Fatalf("并发生成的 version_id = %d 重复", id)
			}
			seen[id] = struct{}{}
		}
	}
}

// TestVersionIDTime_NewIDNearNow 断言由新生成的 versionID 还原出的时间接近当前，
// 而不是被误当纳秒解析成 1970/1969（改毫秒制后的 created_at 回归）。
func TestVersionIDTime_NewIDNearNow(t *testing.T) {
	t.Parallel()

	for range 100 {
		before := time.Now()
		got := versionIDTime(newVersionID(), time.Time{})
		if got.IsZero() {
			t.Fatal("由新 versionID 还原的时间为零值")
		}
		if got.Year() < 2000 {
			t.Fatalf("还原时间 %v 落在 1970/1969（版本 ID 被误当纳秒解析）", got)
		}
		// 精度为毫秒（versionID/1000 截断），正常应在同一毫秒内；留宽窗口容忍调度抖动。
		if d := got.Sub(before); d < -5*time.Second || d > 5*time.Second {
			t.Fatalf("还原时间 %v 与当前 %v 相差 %v，超出合理窗口", got, before, d)
		}
	}
}

// TestVersionIDTime_LegacyFallback 断言历史非正 ID 无法还原时间时回落 fallback，
// 且不会被当作合法时间（负数/零）。
func TestVersionIDTime_LegacyFallback(t *testing.T) {
	t.Parallel()

	fallback := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, id := range []int64{0, -1, -269429080180906331} {
		if got := versionIDTime(id, fallback); !got.Equal(fallback) {
			t.Errorf("versionIDTime(%d) = %v, want fallback %v", id, got, fallback)
		}
	}
}
