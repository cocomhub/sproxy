// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// collectProgress 是测试用进度收集器（线程安全）。
type collectProgress struct {
	mu    sync.Mutex
	total int
	done  int
	fail  int
}

func (p *collectProgress) Report(pr Progress) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = pr.Total
	p.done = pr.Done
	p.fail = pr.Failed
}

func TestRunBatchConcurrent_OrderPreserved(t *testing.T) {
	t.Parallel()
	ops := make([]string, 30)
	for i := range ops {
		ops[i] = itoa(i)
	}
	got := runBatchConcurrent(context.Background(), ops, 4, func(raw string) batchOperationResult {
		return batchOperationResult{Name: raw, Success: true, Message: "ok"}
	}, nil)
	for i, r := range got {
		if r.Name != ops[i] {
			t.Fatalf("第 %d 个结果 = %q, want %q（保序失败）", i, r.Name, ops[i])
		}
	}
}

func TestRunBatchConcurrent_ConcurrencyPeak(t *testing.T) {
	t.Parallel()
	ops := make([]string, 30)
	for i := range ops {
		ops[i] = itoa(i)
	}
	var cur atomic.Int64
	var maxSeen atomic.Int64
	got := runBatchConcurrent(context.Background(), ops, 4, func(raw string) batchOperationResult {
		n := cur.Add(1)
		for {
			old := maxSeen.Load()
			if n <= old || maxSeen.CompareAndSwap(old, n) {
				break
			}
		}
		runtime.Gosched()
		cur.Add(-1)
		return batchOperationResult{Name: raw, Success: true}
	}, nil)
	if len(got) != len(ops) {
		t.Fatalf("结果数 = %d, want %d", len(got), len(ops))
	}
	if maxSeen.Load() > 4 {
		t.Fatalf("并发峰值 %d > 4（信号量失效）", maxSeen.Load())
	}
}

func TestRunBatchConcurrent_FailureIsolated(t *testing.T) {
	t.Parallel()
	ops := []string{"a", "b", "c", "d"}
	var fails atomic.Int64
	got := runBatchConcurrent(context.Background(), ops, 2, func(raw string) batchOperationResult {
		if raw == "b" || raw == "d" {
			fails.Add(1)
			return batchOperationResult{Name: raw, Success: false, Message: "boom"}
		}
		return batchOperationResult{Name: raw, Success: true, Message: "ok"}
	}, nil)
	if fails.Load() != 2 {
		t.Fatalf("失败数 = %d, want 2", fails.Load())
	}
	for i, r := range got {
		wantOK := ops[i] == "a" || ops[i] == "c"
		if r.Success != wantOK {
			t.Errorf("op=%q Success=%v, want %v", ops[i], r.Success, wantOK)
		}
	}
}

func TestRunBatchConcurrent_ProgressMatches(t *testing.T) {
	t.Parallel()
	ops := make([]string, 10)
	for i := range ops {
		ops[i] = itoa(i)
	}
	prog := &collectProgress{}
	got := runBatchConcurrent(context.Background(), ops, 4, func(raw string) batchOperationResult {
		if raw == "3" {
			return batchOperationResult{Name: raw, Success: false, Message: "x"}
		}
		return batchOperationResult{Name: raw, Success: true}
	}, prog)
	if prog.total != 10 || prog.done != 10 || prog.fail != 1 {
		t.Fatalf("progress = %+v, want total=10 done=10 fail=1", prog)
	}
	failCount := 0
	for _, r := range got {
		if !r.Success {
			failCount++
		}
	}
	if failCount != 1 {
		t.Fatalf("结果失败数 = %d, want 1（与 progress 一致）", failCount)
	}
}

func TestRunBatchConcurrent_SerialDegradation(t *testing.T) {
	t.Parallel()
	ops := []string{"a", "b", "c"}
	got := runBatchConcurrent(context.Background(), ops, 1, func(raw string) batchOperationResult {
		return batchOperationResult{Name: raw, Success: true, Message: "ok"}
	}, nil)
	for i, r := range got {
		if r.Name != ops[i] || !r.Success {
			t.Fatalf("串行退化结果 = %+v, want %q ok", r, ops[i])
		}
	}
}

func TestRunBatchConcurrent_PanicRecovered(t *testing.T) {
	t.Parallel()
	ops := []string{"a", "b", "c"}
	got := runBatchConcurrent(context.Background(), ops, 2, func(raw string) batchOperationResult {
		if raw == "b" {
			panic("boom")
		}
		return batchOperationResult{Name: raw, Success: true, Message: "ok"}
	}, nil)
	if len(got) != 3 {
		t.Fatalf("结果数 = %d, want 3（panic op 不应拖垮整批）", len(got))
	}
	if got[0].Success != true || got[2].Success != true {
		t.Errorf("非 panic op 应成功: %+v", got)
	}
	if got[1].Success != false || !strings.Contains(got[1].Message, "boom") {
		t.Errorf("panic op 应为 FAIL 且含 panic 信息: %+v", got[1])
	}
}

func TestRunBatchConcurrent_CancelMarksSkipped(t *testing.T) {
	t.Parallel()
	ops := make([]string, 20)
	for i := range ops {
		ops[i] = itoa(i)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var entered atomic.Int64
	got := runBatchConcurrent(ctx, ops, 1, func(raw string) batchOperationResult {
		n := entered.Add(1)
		if n == 2 {
			cancel()
		}
		return batchOperationResult{Name: raw, Success: true}
	}, nil)
	var skipped, ran int
	for _, r := range got {
		if r.Success {
			ran++
		} else {
			skipped++
		}
	}
	if ran == 0 || skipped == 0 {
		t.Fatalf("cancel 后 ran=%d skipped=%d, want 两者皆 >0（未执行 op 应标记 Skipped）", ran, skipped)
	}
}

func TestRunBatchConcurrent_EmptyOps(t *testing.T) {
	t.Parallel()
	got := runBatchConcurrent(context.Background(), nil, 4, func(raw string) batchOperationResult {
		return batchOperationResult{Name: raw, Success: true}
	}, nil)
	if len(got) != 0 {
		t.Fatalf("空 ops 结果数 = %d, want 0", len(got))
	}
}

func TestRunBatchConcurrent_WorkersClampedToOps(t *testing.T) {
	t.Parallel()
	ops := []string{"a", "b"}
	var peak atomic.Int64
	runBatchConcurrent(context.Background(), ops, 10, func(raw string) batchOperationResult {
		n := peak.Add(1)
		if n > 2 {
			t.Errorf("workers 未钳位到 len(ops)=2（峰值 %d）", n)
		}
		peak.Add(-1)
		return batchOperationResult{Name: raw, Success: true}
	}, nil)
}

// itoa 测试辅助（避免引 strconv 到热路径）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := "0123456789"
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = digits[n%10]
		n /= 10
	}
	return string(b[i:])
}
