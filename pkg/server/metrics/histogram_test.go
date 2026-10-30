// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestDurationHistogram_ObserveBucketBoundary(t *testing.T) {
	t.Parallel()

	th := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond}
	h := NewDurationHistogram(th)
	h.observe(5 * time.Millisecond)                 // 恰在边界（<= 归首桶）
	h.observe(5*time.Millisecond + time.Nanosecond) // 越首桶 → 第二桶
	h.observe(0)                                    // 非正防御性落首桶
	h.observe(-time.Nanosecond)                     // 负时长落首桶

	buckets, _, count := h.snapshot()
	want := []int64{3, 4, 4, 4} // Prometheus 累计：首桶 3（含非正/负），第二桶起累计 4，+Inf 4
	if len(buckets) != len(want) {
		t.Fatalf("buckets 长度 = %d, want %d", len(buckets), len(want))
	}
	for i := range want {
		if buckets[i] != want[i] {
			t.Errorf("buckets[%d] = %d, want %d", i, buckets[i], want[i])
		}
	}
	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}
}

func TestDurationHistogram_CumulativeInvariant(t *testing.T) {
	t.Parallel()

	th := []time.Duration{
		5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
		50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
		500 * time.Millisecond, time.Second,
	}
	h := NewDurationHistogram(th)
	rng := rand.New(rand.NewSource(42))
	const n = 1000
	var sum int64
	for range n {
		d := time.Duration(rng.Int63n(int64(2 * time.Second)))
		h.observe(d)
		sum += int64(d)
	}

	buckets, sumNanos, count := h.snapshot()
	for i := 1; i < len(buckets); i++ {
		if buckets[i] < buckets[i-1] {
			t.Errorf("累计不变量破坏：buckets[%d]=%d < buckets[%d]=%d", i, buckets[i], i-1, buckets[i-1])
		}
	}
	if buckets[len(buckets)-1] != count {
		t.Errorf("+Inf 桶 = %d, want count %d", buckets[len(buckets)-1], count)
	}
	if sumNanos != sum {
		t.Errorf("sum = %d, want %d", sumNanos, sum)
	}
	if count != n {
		t.Errorf("count = %d, want %d", count, n)
	}
}

func TestDurationHistogram_PlusInfCatchesAll(t *testing.T) {
	t.Parallel()

	h := NewDurationHistogram([]time.Duration{5 * time.Millisecond, time.Second})
	h.observe(10 * time.Second) // 超最大桶（1s）→ +Inf 桶
	h.observe(time.Hour)        // 超长时长同样落 +Inf，不丢计数

	buckets, _, count := h.snapshot()
	if got := buckets[len(buckets)-1]; got != 2 {
		t.Errorf("+Inf 桶 = %d, want 2", got)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestWriteHistogram_TextFormat(t *testing.T) {
	t.Parallel()

	th := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}
	buckets := []int64{1, 3, 5} // 累计计数；末位 +Inf = 5
	var b strings.Builder
	WriteHistogram(&b, "sproxy_request_duration_seconds", "HTTP request duration histogram",
		th, buckets, 42_000_000, 5)

	want := "" +
		"# HELP sproxy_request_duration_seconds HTTP request duration histogram\n" +
		"# TYPE sproxy_request_duration_seconds histogram\n" +
		"sproxy_request_duration_seconds_bucket{le=\"0.005\"} 1\n" +
		"sproxy_request_duration_seconds_bucket{le=\"0.01\"} 3\n" +
		"sproxy_request_duration_seconds_bucket{le=\"+Inf\"} 5\n" +
		"sproxy_request_duration_seconds_sum 0.042\n" +
		"sproxy_request_duration_seconds_count 5\n\n"
	if got := b.String(); got != want {
		t.Errorf("WriteHistogram 输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}
