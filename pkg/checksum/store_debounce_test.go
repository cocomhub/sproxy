// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// store_debounce_test.go 钉住「写合并」语义（写放大治理，2026-10-30）：
// `Set`/`Delete`/`Rename` 等只更新内存并累加代际，由 debounce 定时器合并为**一次**全量落盘；
// 此前每条写都 `save()` 全量重写 JSON ⇒ 批量 N 文件 = O(N²) I/O。

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// countingWriter 返回一个计数落盘实现（不实际写盘）。
func countingWriter() (*atomic.Int64, func([]byte) error) {
	var n atomic.Int64
	return &n, func([]byte) error {
		n.Add(1)
		return nil
	}
}

// TestStore_ManyWritesCoalesceToOneSave：窗口内 50 次 Set ⇒ Flush 时只落盘 1 次。
//
// 变异：persist 直接 cs.save() ⇒ 每次 Set 都落盘 ⇒ saves=50 ⇒ 红。
func TestStore_ManyWritesCoalesceToOneSave(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	saves, write := countingWriter()
	cs.writeSnap = write

	for i := range 50 {
		cs.Set("user/f"+itoa(i), "sum")
	}
	if got := saves.Load(); got != 0 {
		t.Fatalf("debounce 窗口内不应落盘，got %d 次", got)
	}
	if err := cs.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := saves.Load(); got != 1 {
		t.Fatalf("50 次 Set 应合并为 1 次落盘，got %d", got)
	}
	if got := cs.Len(); got != 50 {
		t.Fatalf("内存台账应为 50 条，got %d", got)
	}
}

// TestStore_DebounceFlushesAutomatically：短 debounce 到期后自动落盘（无需 Flush）。
func TestStore_DebounceFlushesAutomatically(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	cs.merger.setDebounce(10 * time.Millisecond)
	saves, write := countingWriter()
	cs.writeSnap = write

	cs.Set("user/a.txt", "sum")
	testutil.WaitFor(t, 2*time.Second, func() bool { return saves.Load() == 1 }, "debounce 未自动落盘")
}

// TestStore_CloseFlushesFinalWrite：Close 做最终落盘且幂等；关闭后不再排定时器。
func TestStore_CloseFlushesFinalWrite(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	saves, write := countingWriter()
	cs.writeSnap = write

	cs.Set("user/a.txt", "sum")
	if err := cs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := saves.Load(); got != 1 {
		t.Fatalf("Close 应最终落盘 1 次，got %d", got)
	}
	if err := cs.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if got := saves.Load(); got != 1 {
		t.Fatalf("重复 Close 不应再落盘，got %d", got)
	}
}

// itoa 避免额外 import（测试内只用十进制小整数）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
