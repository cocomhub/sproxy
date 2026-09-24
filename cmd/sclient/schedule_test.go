// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// schedule_test.go 验证 cron 表达式解析与下次触发时刻（roadmap P2 定时调度同步残余）：
//  1. 每 6 小时整点："0 */6 * * *" → 02:00 后下次 06:00（含分钟边界）。
//  2. 非法表达式 fail-closed。
//  3. nextAfter 严格大于当前时刻。

import (
	"testing"
	"time"
)

// TestCronExpr_Every6Hours 每 6 小时整点。
func TestCronExpr_Every6Hours(t *testing.T) {
	t.Parallel()
	expr, err := parseCronExpr("0 */6 * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 24, 2, 0, 0, 0, time.UTC)
	next, err := expr.nextAfter(base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Hour() != 6 || next.Minute() != 0 || next.Day() != 24 {
		t.Fatalf("next = %v, want 2026-09-24 06:00", next)
	}
}

// TestCronExpr_Minutely 每分钟（* * * * *）：nextAfter 是下一分钟。
func TestCronExpr_Minutely(t *testing.T) {
	t.Parallel()
	expr, err := parseCronExpr("* * * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 24, 10, 30, 45, 0, time.UTC)
	next, err := expr.nextAfter(base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Hour() != 10 || next.Minute() != 31 {
		t.Fatalf("next = %v, want 10:31", next)
	}
}

// TestCronExpr_Daily 每天 02:30："30 2 * * *"。
func TestCronExpr_Daily(t *testing.T) {
	t.Parallel()
	expr, err := parseCronExpr("30 2 * * *")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	next, err := expr.nextAfter(base)
	if err != nil {
		t.Fatal(err)
	}
	if next.Hour() != 2 || next.Minute() != 30 || next.Day() != 25 {
		t.Fatalf("next = %v, want 2026-09-25 02:30", next)
	}
}

// TestCronExpr_Invalid fail-closed：字段数错 / 数值越界。
func TestCronExpr_Invalid(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"", "0 * * *", "60 * * * *", "* * * 13 *", "1-5-9 * * * *"} {
		if _, err := parseCronExpr(spec); err == nil {
			t.Fatalf("表达式 %q 应解析失败", spec)
		}
	}
}
