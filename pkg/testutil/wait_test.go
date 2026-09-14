// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package testutil

import (
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTB 是 waitTB 的替身：只记录 Fatalf，不真的终止测试——
// 使「超时」这条分支可以被测到（testing.TB 含私有方法，无法在本包外实现）。
type fakeTB struct {
	fatalMsgs []string
}

func (f *fakeTB) Helper() {}

func (f *fakeTB) Fatalf(format string, args ...any) {
	f.fatalMsgs = append(f.fatalMsgs, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func TestWaitFor_ReturnsImmediatelyWhenAlreadyTrue(t *testing.T) {
	t.Parallel()
	start := time.Now()
	WaitFor(t, time.Second, func() bool { return true })
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("条件已满足时不应等待，实际耗时 %s", elapsed)
	}
}

func TestWaitFor_PollsUntilConditionFlips(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	WaitFor(t, 2*time.Second, func() bool { return calls.Add(1) >= 5 })
	if got := calls.Load(); got < 5 {
		t.Fatalf("cond 调用次数 = %d, want >= 5", got)
	}
}

func TestWaitFor_FailsWithCallerMessageOnTimeout(t *testing.T) {
	t.Parallel()
	fake := &fakeTB{}
	WaitFor(fake, 20*time.Millisecond, func() bool { return false }, "任务应进入 downloading")
	if len(fake.fatalMsgs) != 1 {
		t.Fatalf("期望恰好 1 条 Fatalf，实际 %d：%v", len(fake.fatalMsgs), fake.fatalMsgs)
	}
	got := fake.fatalMsgs[0]
	if !strings.Contains(got, "WaitFor 超时") || !strings.Contains(got, "20ms") {
		t.Errorf("超时信息应含 timeout：%q", got)
	}
	if !strings.Contains(got, "任务应进入 downloading") {
		t.Errorf("超时信息应含调用方说明（flake 报告需自解释）：%q", got)
	}
}

func TestWaitFor_TimeoutWithoutMessage(t *testing.T) {
	t.Parallel()
	fake := &fakeTB{}
	WaitFor(fake, 10*time.Millisecond, func() bool { return false })
	if len(fake.fatalMsgs) != 1 || !strings.Contains(fake.fatalMsgs[0], "WaitFor 超时") {
		t.Fatalf("无说明时也须失败并含超时字样：%v", fake.fatalMsgs)
	}
}
