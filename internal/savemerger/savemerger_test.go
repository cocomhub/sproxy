// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package savemerger

import (
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// TestMerger_CoalescesWrites：窗口内多次 Mark ⇒ Flush 只写 1 次。
//
// 变异：Mark 直接调用 write ⇒ 写次数等于 Mark 次数 ⇒ 红。
func TestMerger_CoalescesWrites(t *testing.T) {
	t.Parallel()
	var writes atomic.Int64
	m := New(time.Hour, testLog(), func(uint64) error {
		writes.Add(1)
		return nil
	})
	for i := range 50 {
		m.Mark("set", "i", i)
	}
	if got := writes.Load(); got != 0 {
		t.Fatalf("窗口内不应写，got %d", got)
	}
	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("50 次 Mark 应合并为 1 次写，got %d", got)
	}
	if err := m.Flush(); err != nil {
		t.Fatalf("幂等 Flush: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("无脏数据时 Flush 不应再写，got %d", got)
	}
}

// TestMerger_DebounceAutoFlush：窗口到期自动写一次。
func TestMerger_DebounceAutoFlush(t *testing.T) {
	t.Parallel()
	var writes atomic.Int64
	m := New(10*time.Millisecond, testLog(), func(uint64) error {
		writes.Add(1)
		return nil
	})
	m.Mark("set")
	testutil.WaitFor(t, 2*time.Second, func() bool { return writes.Load() == 1 }, "debounce 未自动写")
}

// TestMerger_CloseFlushesFinalWrite：Close 做最终写且幂等。
func TestMerger_CloseFlushesFinalWrite(t *testing.T) {
	t.Parallel()
	var writes atomic.Int64
	m := New(time.Hour, testLog(), func(uint64) error {
		writes.Add(1)
		return nil
	})
	m.Mark("set")
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("Close 应写 1 次，got %d", got)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("重复 Close 不应再写，got %d", got)
	}
	m.Mark("after-close") // 关闭后 Mark 仍更新内存但不持久化
	if err := m.Flush(); err != nil {
		t.Fatalf("关闭后 Flush: %v", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("关闭后不应再写，got %d", got)
	}
}

// TestMerger_WriteErrorRetriesOnce：首次写失败 ⇒ 重试一次；两次都失败不 panic。
func TestMerger_WriteErrorRetriesOnce(t *testing.T) {
	t.Parallel()
	var calls atomic.Int64
	m := New(time.Hour, testLog(), func(uint64) error {
		calls.Add(1)
		return errors.New("disk full")
	})
	m.Mark("set")
	if err := m.Flush(); err == nil {
		t.Fatal("Flush 应返回写错误")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Flush 只尝试一次（重试由定时器路径负责），got %d", got)
	}
}

// TestMerger_SetDebounce：调整窗口后生效（仅测试用途）。
func TestMerger_SetDebounce(t *testing.T) {
	t.Parallel()
	var writes atomic.Int64
	m2 := New(time.Hour, testLog(), func(uint64) error {
		writes.Add(1)
		return nil
	})
	m2.SetDebounce(5 * time.Millisecond)
	m2.Mark("set")
	testutil.WaitFor(t, 2*time.Second, func() bool { return writes.Load() == 1 }, "调整窗口未生效")
}
