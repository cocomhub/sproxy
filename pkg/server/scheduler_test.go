// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// scheduler_test.go 覆盖统一任务调度器（roadmap 11.10-H2，设计文档
// docs/designs/2026-09-24-task-scheduler.md §5）：
//  1. Register/Start/Stop 生命周期（计数到 2 → Stop 冻结）；
//  2. panic 恢复（首次 panic 后下个 tick 照常执行）；
//  3. 单飞防重入（长任务阻塞期间并发峰值恒 1）；
//  4. 维护窗口（窗口外 MaintenanceOnly 任务跳过、普通任务照跑；窗口内恢复）；
//  5. FinalRunOnStop（Stop 补跑一次，share「退出前清理」语义）；
//  6. Register 校验（重名/零间隔/空名/缺 Run → error）；
//  7. 维护窗口配置校验与跨午夜解析。
//
// 全部条件等待（testutil.WaitFor / WaitForBool），无固定 sleep（R14 棘轮）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestScheduler_RegisterStartStop 20ms 间隔任务执行 ≥2 次后 Stop，计数冻结。
func TestScheduler_RegisterStartStop(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	var count atomic.Int64
	if err := s.Register(Task{
		Name:     "tick",
		Interval: 20 * time.Millisecond,
		Run:      func(context.Context) { count.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	testutil.WaitFor(t, 30*time.Second, func() bool { return count.Load() >= 2 }, "任务应至少执行 2 次")
	s.Stop()
	after := count.Load()
	// 负向断言「计数冻结」用 WaitForBool 的「超时 = 合法结果」语义：
	// 60ms 内计数不应再增长（若增长则立即红）。
	if testutil.WaitForBool(60*time.Millisecond, func() bool { return count.Load() > after }) {
		t.Fatalf("Stop 后计数应冻结（after=%d got=%d）", after, count.Load())
	}
}

// TestScheduler_PanicRecovery Run 首次 panic 被 recover，下个 tick 仍执行（计数达 2）。
func TestScheduler_PanicRecovery(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	var count atomic.Int64
	if err := s.Register(Task{
		Name:     "panic-task",
		Interval: 20 * time.Millisecond,
		Run: func(context.Context) {
			if count.Add(1) == 1 {
				panic("boom")
			}
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	testutil.WaitFor(t, 30*time.Second, func() bool { return count.Load() >= 2 }, "panic 后下个 tick 仍应执行")
	s.Stop()
}

// TestScheduler_NoReentrant 长任务（阻塞在 gate）阻塞期间多次 tick 被单飞挡下：
// 执行次数保持 1、并发峰值恒 1。
func TestScheduler_NoReentrant(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	var (
		runs          atomic.Int64
		concurrent    atomic.Int64
		maxConcurrent atomic.Int64
		gate          = make(chan struct{})
	)
	if err := s.Register(Task{
		Name:     "slow-task",
		Interval: 20 * time.Millisecond,
		Run: func(context.Context) {
			runs.Add(1)
			cur := concurrent.Add(1)
			for {
				m := maxConcurrent.Load()
				if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
					break
				}
			}
			<-gate // 阻塞制造重入窗口（长于 20ms 间隔）
			concurrent.Add(-1)
		},
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	testutil.WaitFor(t, 30*time.Second, func() bool { return runs.Load() >= 1 && concurrent.Load() == 1 }, "任务应进入并阻塞")
	// 阻塞期间多个 tick 应被单飞挡下：执行次数保持 1、并发峰值 1。
	if testutil.WaitForBool(150*time.Millisecond, func() bool { return runs.Load() > 1 }) {
		t.Fatalf("单飞防重入失效：阻塞期间任务又执行了（runs=%d）", runs.Load())
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("并发峰值=%d want 1（单飞 CAS 失效）", got)
	}
	close(gate)
	s.Stop()
}

// TestScheduler_MaintenanceWindow 窗口外 MaintenanceOnly 任务 0 次、非 MaintenanceOnly 照跑；
// 窗口打开后维护任务恢复执行。
func TestScheduler_MaintenanceWindow(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	var maint atomic.Int64
	var always atomic.Int64
	s.SetMaintenanceWindow(func(time.Time) bool { return false }) // 恒窗口外
	if err := s.Register(Task{
		Name:            "maint",
		Interval:        20 * time.Millisecond,
		MaintenanceOnly: true,
		Run:             func(context.Context) { maint.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(Task{
		Name:     "always",
		Interval: 20 * time.Millisecond,
		Run:      func(context.Context) { always.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	testutil.WaitFor(t, 30*time.Second, func() bool { return always.Load() >= 2 }, "非 MaintenanceOnly 任务窗口外应照跑")
	if got := maint.Load(); got != 0 {
		t.Fatalf("窗口外 MaintenanceOnly 任务不应执行, got %d", got)
	}
	s.SetMaintenanceWindow(func(time.Time) bool { return true }) // 打开窗口
	testutil.WaitFor(t, 30*time.Second, func() bool { return maint.Load() >= 2 }, "窗口内维护任务应执行")
	s.Stop()
}

// TestScheduler_FinalRunOnStop FinalRunOnStop 任务在 Stop 时补跑一次（share 退出前清理语义）。
func TestScheduler_FinalRunOnStop(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	var count atomic.Int64
	if err := s.Register(Task{
		Name:           "final",
		Interval:       time.Hour, // 1h 间隔在本测试内不可能触发 tick
		FinalRunOnStop: true,
		Run:            func(context.Context) { count.Add(1) },
	}); err != nil {
		t.Fatal(err)
	}
	s.Start()
	s.Stop()
	if got := count.Load(); got != 1 {
		t.Fatalf("FinalRunOnStop 任务 Stop 后应补跑一次, got %d", got)
	}
}

// TestScheduler_RegisterValidation 重名/零间隔/负间隔/空名/缺 Run → error。
func TestScheduler_RegisterValidation(t *testing.T) {
	t.Parallel()
	s := NewScheduler(testLogger())
	run := func(context.Context) {}
	if err := s.Register(Task{Name: "", Interval: time.Second, Run: run}); err == nil {
		t.Fatal("空任务名应拒绝")
	}
	if err := s.Register(Task{Name: "zero", Interval: 0, Run: run}); err == nil {
		t.Fatal("零间隔应拒绝")
	}
	if err := s.Register(Task{Name: "neg", Interval: -time.Second, Run: run}); err == nil {
		t.Fatal("负间隔应拒绝")
	}
	if err := s.Register(Task{Name: "nilrun", Interval: time.Second}); err == nil {
		t.Fatal("缺 Run 应拒绝")
	}
	if err := s.Register(Task{Name: "ok", Interval: time.Second, Run: run}); err != nil {
		t.Fatalf("合法注册应成功: %v", err)
	}
	if err := s.Register(Task{Name: "ok", Interval: time.Second, Run: run}); err == nil {
		t.Fatal("重名应拒绝")
	}
}

// TestParseMaintenanceWindow 窗口解析：未启用返回 nil（恒执行）；End<=Start 跨午夜；
// 同日窗口正常判定。
func TestParseMaintenanceWindow(t *testing.T) {
	t.Parallel()
	if got := parseMaintenanceWindow(SchedulerConfig{}); got != nil {
		t.Fatal("未启用窗口应返回 nil（恒执行，零回归）")
	}
	in := parseMaintenanceWindow(SchedulerConfig{MaintenanceWindow: MaintenanceWindowConfig{
		Enabled: true, Start: "22:00", End: "06:00",
	}})
	if in == nil {
		t.Fatal("启用窗口应返回非 nil 判定函数")
	}
	for _, tc := range []struct {
		h, m int
		want bool
	}{
		{21, 59, false}, {22, 0, true}, {23, 59, true},
		{0, 0, true}, {5, 59, true}, {6, 0, false},
	} {
		if got := in(time.Date(2026, 9, 24, tc.h, tc.m, 0, 0, time.Local)); got != tc.want {
			t.Fatalf("跨午夜窗口 22:00-06:00 在 %02d:%02d 判定=%v want %v", tc.h, tc.m, got, tc.want)
		}
	}
	in2 := parseMaintenanceWindow(SchedulerConfig{MaintenanceWindow: MaintenanceWindowConfig{
		Enabled: true, Start: "02:00", End: "04:00",
	}})
	if !in2(time.Date(2026, 9, 24, 3, 0, 0, 0, time.Local)) {
		t.Fatal("同日窗口 02:00-04:00 在 03:00 应判定窗口内")
	}
	if in2(time.Date(2026, 9, 24, 5, 0, 0, 0, time.Local)) {
		t.Fatal("同日窗口 02:00-04:00 在 05:00 应判定窗口外")
	}
}

// TestConfigValidate_MaintenanceWindow 配置校验：启用时 HH:MM 格式 fail-closed；
// 未启用时非法值被忽略（零回归）。
func TestConfigValidate_MaintenanceWindow(t *testing.T) {
	t.Parallel()
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.Scheduler.MaintenanceWindow.Enabled = true
	cfg.Scheduler.MaintenanceWindow.Start = "22:00"
	cfg.Scheduler.MaintenanceWindow.End = "06:00"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法维护窗口应通过校验: %v", err)
	}
	cfg.Scheduler.MaintenanceWindow.Start = "25:00"
	if err := cfg.Validate(); err == nil {
		t.Fatal("start=25:00 非法小时应拒绝")
	}
	cfg.Scheduler.MaintenanceWindow.Start = "22:00"
	cfg.Scheduler.MaintenanceWindow.End = "6:0"
	if err := cfg.Validate(); err == nil {
		t.Fatal("end=6:0 非 HH:MM 应拒绝")
	}
	cfg2 := Default()
	cfg2.StorageRoot = t.TempDir()
	cfg2.Scheduler.MaintenanceWindow.Start = "not-a-time"
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("未启用维护窗口时非法值应被忽略（零回归）: %v", err)
	}
}
