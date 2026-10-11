// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"io"
	"log/slog"
	"testing"
)

// TestStorageManager_ReleaseClampsAtZero 账本恒不为负（与 quota.Pool 同不变量）：
// 扫描覆写后在途 Release 过量时，总量与分类计数都必须钳到 0（负值会让上限校验恒通过）。
// 变异：改回 `Add(-size)` → 断言读到负值 → 红。
func TestStorageManager_ReleaseClampsAtZero(t *testing.T) {
	t.Parallel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sm := NewStorageManager(t.TempDir(), 100, nil, log)
	if err := sm.TryReserve(10, CategoryChunked); err != nil {
		t.Fatalf("TryReserve: %v", err)
	}
	sm.Release(50, CategoryChunked) // 过量释放（模拟扫描已经把占用清零后再 Release 在途预留）
	if got := sm.Usage(); got != 0 {
		t.Fatalf("总量应钳到 0, got %d", got)
	}
	if got := sm.UsageByCategory()[CategoryChunked]; got != 0 {
		t.Fatalf("分类计数应钳到 0, got %d", got)
	}
	// 钳 0 后仍可正常预留（上限校验不受影响）。
	if err := sm.TryReserve(20, CategoryChunked); err != nil {
		t.Fatalf("钳 0 后应可正常预留: %v", err)
	}
	if got := sm.Usage(); got != 20 {
		t.Fatalf("预留后应为 20, got %d", got)
	}
}
