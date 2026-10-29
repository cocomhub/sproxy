// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// state_adapter_debounce_test.go 钉住集群后端（StateStore）的写合并（2026-10-30）：
// 每次 Put 是一次 RTT + 全量序列化，批量写必须合并为一个窗口内的一次 Put。

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// TestStateBackedStore_ManyWritesCoalesceToOnePut：窗口内 50 次 Set ⇒ Flush 只 Put 1 次。
//
// 变异：写方法直接 `s.save()`（每写一次 Put）⇒ 窗口内 put>0 且总数 50 ⇒ 红。
func TestStateBackedStore_ManyWritesCoalesceToOnePut(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(t.TempDir(), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())
	var puts atomic.Int64
	s.put = func(context.Context, string, []byte) error {
		puts.Add(1)
		return nil
	}

	for i := range 50 {
		s.Set("user/f"+itoa(i), "sum")
	}
	if got := puts.Load(); got != 0 {
		t.Fatalf("合并窗口内不应 Put，got %d 次", got)
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("50 次 Set 应合并为 1 次 Put，got %d", got)
	}
	if got := len(s.GetAll()); got != 50 {
		t.Fatalf("内存台账应为 50 条，got %d", got)
	}
}

// TestStateBackedStore_CloseFlushesFinalWrite：Close 做最终 Put 且幂等。
func TestStateBackedStore_CloseFlushesFinalWrite(t *testing.T) {
	t.Parallel()
	st := state.NewLocalStateStore(t.TempDir(), stateTestLogger())
	s := NewStateBackedChecksumStore(st, "checksum/alice/all", nil, "", stateTestLogger())
	var puts atomic.Int64
	s.put = func(context.Context, string, []byte) error {
		puts.Add(1)
		return nil
	}

	s.Set("user/a.txt", "sum")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("Close 应最终 Put 1 次，got %d", got)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if got := puts.Load(); got != 1 {
		t.Fatalf("重复 Close 不应再 Put，got %d", got)
	}
}
