// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// wait_test.go 锁定容量层等待语义（设计 docs/designs/2026-10-19-waitq.md §8.2 层 B）：
// 与配额层同口径 —— 零字节直通 / 不足排队且释放唤醒 / 超时 / 不可能容纳立即失败。

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestWaitReserve_ZeroBytesPassthrough 零字节直通（与配额层同口径；不排队、不报错）。
func TestWaitReserve_ZeroBytesPassthrough(t *testing.T) {
	t.Parallel()
	sm := NewStorageManager(t.TempDir(), 1024, nil, testLogger())
	if err := sm.WaitReserve(context.Background(), 0, CategoryChunked, time.Second); err != nil {
		t.Fatalf("零字节应直通成功, got %v", err)
	}
	if got := sm.Usage(); got != 0 {
		t.Fatalf("零字节不应改变账本, usage=%d", got)
	}
}

// TestWaitReserve_TooLargeFailsFast 不可能容纳（> 上限）⇒ 立即失败（不白等超时）。
func TestWaitReserve_TooLargeFailsFast(t *testing.T) {
	t.Parallel()
	sm := NewStorageManager(t.TempDir(), 100, nil, testLogger())
	start := time.Now()
	err := sm.WaitReserve(context.Background(), 101, CategoryChunked, 30*time.Second)
	if err == nil {
		t.Fatal("超过上限应失败")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("应立即失败（不排队）, took %v", d)
	}
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("错误应包装 ErrStorageFull（调用方 507 映射不变）, got %v", err)
	}
}

// TestWaitReserve_WakesOnRelease 不足 ⇒ 排队；Release ⇒ 唤醒并成功（丢唤醒即红）。
func TestWaitReserve_WakesOnRelease(t *testing.T) {
	t.Parallel()
	sm := NewStorageManager(t.TempDir(), 100, nil, testLogger())
	if err := sm.TryReserve(100, CategoryChunked); err != nil {
		t.Fatalf("预占: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- sm.WaitReserve(context.Background(), 60, CategoryChunked, 5*time.Second)
	}()
	// 释放全部：等待侧的「查 + 入队」与释放侧的「改账 + 唤醒」同锁协作 ⇒ 不会丢唤醒
	// （无需 sleep 轮询：即使释放先发生，等待者的锁内快速尝试也会立即成功）。
	sm.Release(100, CategoryChunked)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("释放后应成功, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("释放后未被唤醒（丢唤醒）")
	}
}

// TestWaitReserve_Timeout 超时 ⇒ ErrWaitTimeout 包装 ErrStorageFull。
func TestWaitReserve_Timeout(t *testing.T) {
	t.Parallel()
	sm := NewStorageManager(t.TempDir(), 100, nil, testLogger())
	if err := sm.TryReserve(100, CategoryChunked); err != nil {
		t.Fatalf("预占: %v", err)
	}
	err := sm.WaitReserve(context.Background(), 10, CategoryChunked, 30*time.Millisecond)
	if !errors.Is(err, ErrWaitTimeout) || !errors.Is(err, ErrStorageFull) {
		t.Fatalf("应 ErrWaitTimeout 包装 ErrStorageFull, got %v", err)
	}
}
