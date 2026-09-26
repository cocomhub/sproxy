// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// usage_store_test.go 验证计量报告 P1（roadmap 11.10-⑩）usageStore 纯逻辑层：
//  1. 日轮转：跨日 RecordUsage 生成新日桶（同日累计，跨日分桶）。
//  2. 月轮转：跨月 RecordUsage 桶归属正确（月边界不混桶）。
//  3. 跨周期求和：Summary 跨日/跨月聚合正确（含单日子集）。
//  4. 快照往返：Flush 落盘后新实例载入可查（重启恢复，验收核心）。
//  5. 内存环：超过保留窗口的最旧日桶被弹出（保留最近 N 周期）。
//  6. CSV 转义：逗号/引号/换行注入用例被正确包裹/翻倍。

// newTestUsageStore 创建指向 t.TempDir 的 usageStore（helper）。
func newTestUsageStore(t *testing.T) *usageStore {
	t.Helper()
	return newUsageStore(t.TempDir(), testLogger())
}

// TestUsageStore_DailyRotation 验证日轮转：同日累计进同一日桶，跨日生成新日桶。
func TestUsageStore_DailyRotation(t *testing.T) {
	t.Parallel()
	s := newTestUsageStore(t)
	base := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

	// 同日两次记录 → 累计。
	s.RecordUsageAt("owner-a", "requests", 3, base)
	s.RecordUsageAt("owner-a", "requests", 4, base.Add(13*time.Hour))

	d1 := s.Daily("owner-a", "2026-09-01")
	if d1["requests"] != 7 {
		t.Fatalf("Daily(2026-09-01)[requests] = %d, want 7（同日累计）", d1["requests"])
	}

	// 跨日轮转 → 新日桶，旧桶不受影响。
	s.RecordUsageAt("owner-a", "requests", 5, base.Add(24*time.Hour))
	if d := s.Daily("owner-a", "2026-09-01"); d["requests"] != 7 {
		t.Fatalf("轮转后 Daily(2026-09-01)[requests] = %d, want 7", d["requests"])
	}
	if d := s.Daily("owner-a", "2026-09-02"); d["requests"] != 5 {
		t.Fatalf("轮转后 Daily(2026-09-02)[requests] = %d, want 5（跨日建新桶）", d["requests"])
	}
}

// TestUsageStore_MonthRotation 验证月轮转：跨月记录各自归入所属日桶，不混桶。
func TestUsageStore_MonthRotation(t *testing.T) {
	t.Parallel()
	s := newTestUsageStore(t)

	s.RecordUsageAt("owner-a", "upload_bytes", 10, time.Date(2026, 9, 30, 23, 59, 0, 0, time.UTC))
	s.RecordUsageAt("owner-a", "upload_bytes", 20, time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC))
	s.RecordUsageAt("owner-a", "download_bytes", 5, time.Date(2026, 10, 1, 0, 1, 0, 0, time.UTC))

	if d := s.Daily("owner-a", "2026-09-30"); d["upload_bytes"] != 10 {
		t.Fatalf("Daily(2026-09-30)[upload_bytes] = %d, want 10", d["upload_bytes"])
	}
	d2 := s.Daily("owner-a", "2026-10-01")
	if d2["upload_bytes"] != 20 {
		t.Fatalf("Daily(2026-10-01)[upload_bytes] = %d, want 20", d2["upload_bytes"])
	}
	if d2["download_bytes"] != 5 {
		t.Fatalf("Daily(2026-10-01)[download_bytes] = %d, want 5", d2["download_bytes"])
	}
}

// TestUsageStore_SummaryRange 验证跨周期求和：from/to 含端点，跨日跨月聚合。
func TestUsageStore_SummaryRange(t *testing.T) {
	t.Parallel()
	s := newTestUsageStore(t)

	s.RecordUsageAt("owner-a", "upload_bytes", 10, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	s.RecordUsageAt("owner-a", "upload_bytes", 15, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	s.RecordUsageAt("owner-a", "requests", 7, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	s.RecordUsageAt("owner-a", "upload_bytes", 25, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

	sum := s.Summary("owner-a", "2026-09-01", "2026-10-01")
	if sum.Kinds["upload_bytes"] != 50 {
		t.Fatalf("Summary upload_bytes = %d, want 50（跨月求和）", sum.Kinds["upload_bytes"])
	}
	if sum.Kinds["requests"] != 7 {
		t.Fatalf("Summary requests = %d, want 7", sum.Kinds["requests"])
	}
	if sum.Days != 3 {
		t.Fatalf("Summary Days = %d, want 3", sum.Days)
	}

	// 单日子集（from == to）。
	one := s.Summary("owner-a", "2026-09-02", "2026-09-02")
	if one.Kinds["upload_bytes"] != 15 || one.Kinds["requests"] != 7 || one.Days != 1 {
		t.Fatalf("Summary 单日 = %+v, want upload=15 requests=7 days=1", one)
	}
}

// TestUsageStore_SnapshotRoundTrip 验证快照往返（验收核心）：Flush 落盘后
// 新实例载入同一持久化目录，历史可查。
func TestUsageStore_SnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	s1 := newUsageStore(dir, testLogger())
	s1.RecordUsageAt("owner-a", "upload_bytes", 100, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	s1.RecordUsageAt("owner-a", "download_bytes", 50, time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC))
	if err := s1.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// 月快照文件已落盘（<persistDir>/<owner>/<YYYY-MM>.json）。
	if _, err := os.Stat(filepath.Join(dir, "owner-a", "2026-09.json")); err != nil {
		t.Fatalf("月快照文件缺失: %v", err)
	}

	// 重启：新实例载入同一持久化目录。
	s2 := newUsageStore(dir, testLogger())
	got := s2.Summary("owner-a", "2026-09-01", "2026-09-02")
	if got.Kinds["upload_bytes"] != 100 {
		t.Fatalf("重载后 upload_bytes = %d, want 100", got.Kinds["upload_bytes"])
	}
	if got.Kinds["download_bytes"] != 50 {
		t.Fatalf("重载后 download_bytes = %d, want 50", got.Kinds["download_bytes"])
	}
	if got.Days != 2 {
		t.Fatalf("重载后 Days = %d, want 2", got.Days)
	}
}

// TestUsageStore_RingRetention 验证内存环：超过保留窗口的最旧日桶被弹出
// （内存只保留最近 usageRingRetention 个日桶）。
func TestUsageStore_RingRetention(t *testing.T) {
	t.Parallel()
	s := newTestUsageStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i <= usageRingRetention; i++ {
		s.RecordUsageAt("owner-a", "requests", 1, base.AddDate(0, 0, i))
	}
	firstDay := base.Format("2006-01-02")
	if d := s.Daily("owner-a", firstDay); len(d) != 0 {
		t.Fatalf("最旧日桶应被弹出: Daily(%s) = %+v, want 空", firstDay, d)
	}
	lastDay := base.AddDate(0, 0, usageRingRetention).Format("2006-01-02")
	if d := s.Daily("owner-a", lastDay); d["requests"] != 1 {
		t.Fatalf("最新日桶应保留: Daily(%s)[requests] = %d, want 1", lastDay, d["requests"])
	}
}

// TestUsageCSVEscape 验证 CSV 转义：逗号/引号/换行注入用例被包裹/翻倍。
func TestUsageCSVEscape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"普通字段不变", "owner-a", "owner-a"},
		{"空字段不变", "", ""},
		{"含逗号包裹", "a,b", `"a,b"`},
		{"含引号翻倍", `say "hi"`, `"say ""hi"""`},
		{"含换行包裹", "line1\nline2", "\"line1\nline2\""},
		{"逗号+引号+换行组合", "a,\"b\nc\"", "\"a,\"\"b\nc\"\"\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := usageCSVEscape(tc.input); got != tc.want {
				t.Fatalf("usageCSVEscape(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
