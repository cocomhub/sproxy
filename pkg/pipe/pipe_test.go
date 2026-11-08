// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pipe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEnv 记录退避序列，零等待。
type fakeEnv struct {
	mu     sync.Mutex
	delays []time.Duration
	sleeps int
	cancel context.CancelFunc
	failAt int // 第几次 sleep 时取消（0 = 不取消）
}

func newFakeEnv() *fakeEnv { return &fakeEnv{} }

func (f *fakeEnv) env() Env {
	return Env{
		Sleep: func(ctx context.Context, d time.Duration) error {
			f.mu.Lock()
			f.delays = append(f.delays, d)
			f.sleeps++
			n := f.sleeps
			cancel := f.cancel
			failAt := f.failAt
			f.mu.Unlock()
			if cancel != nil && failAt > 0 && n >= failAt {
				cancel()
				return ctx.Err()
			}
			return ctx.Err()
		},
	}
}

func (f *fakeEnv) got() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.delays...)
}

// TestRetry_BackoffSequence 退避序列：Base×Factorⁿ，封顶 Cap。
func TestRetry_BackoffSequence(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	var attempts int
	err := Retry(context.Background(), fe.env(), Policy{
		Attempts: 5, Base: time.Second, Cap: 8 * time.Second,
	}, func(context.Context, int) error { attempts++; return errors.New("boom") })
	if err == nil {
		t.Fatal("持续失败应报错")
	}
	if !errors.Is(err, ErrAttemptsExhausted) {
		t.Fatalf("应包裹 ErrAttemptsExhausted，got %v", err)
	}
	if attempts != 5 {
		t.Fatalf("应尝试 5 次，got %d", attempts)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second}
	got := fe.got()
	if len(got) != len(want) {
		t.Fatalf("退避次数应为 %d，got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 次退避应为 %v，got %v（完整 %v）", i+1, want[i], got[i], got)
		}
	}
}

// TestRetry_CapApplied 超过 Cap 后退避不再增长（且不小于 Cap）。
func TestRetry_CapApplied(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	_ = Retry(context.Background(), fe.env(), Policy{
		Attempts: 6, Base: time.Second, Cap: 3 * time.Second,
	}, func(context.Context, int) error { return errors.New("x") })
	for i, d := range fe.got() {
		if d > 3*time.Second {
			t.Fatalf("第 %d 次退避 %v 超过 Cap 3s", i+1, d)
		}
	}
	if fe.got()[len(fe.got())-1] != 3*time.Second {
		t.Fatalf("末次退避应为 Cap=3s，got %v", fe.got())
	}
}

// TestRetry_NonRetryableReturnsImmediately 不可重试错误 ⇒ 立刻返回（不消耗重试）。
func TestRetry_NonRetryableReturnsImmediately(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	fatal := errors.New("fatal-4xx")
	var attempts int
	err := Retry(context.Background(), fe.env(), Policy{
		Attempts: 5, Base: time.Second,
		Retryable: func(e error) bool { return !errors.Is(e, fatal) },
	}, func(context.Context, int) error { attempts++; return fatal })
	if !errors.Is(err, fatal) {
		t.Fatalf("应原样返回致命错误，got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("致命错误不得重试，got %d 次", attempts)
	}
	if len(fe.got()) != 0 {
		t.Fatalf("致命错误不得退避，got %v", fe.got())
	}
}

// TestRetry_CancelDuringBackoffStopsImmediately 退避期间取消 ⇒ 立即返回，不再尝试。
func TestRetry_CancelDuringBackoffStopsImmediately(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fe := newFakeEnv()
	fe.cancel, fe.failAt = cancel, 1
	var attempts int
	err := Retry(ctx, fe.env(), Policy{Attempts: 9, Base: time.Second},
		func(context.Context, int) error { attempts++; return errors.New("transient") })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，got %v", err)
	}
	if attempts > 2 {
		t.Fatalf("取消后不得继续尝试（got %d 次）", attempts)
	}
}

// TestRetry_SucceedsAfterRetries 瞬时失败后成功 ⇒ nil 且 attempts 正确。
func TestRetry_SucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	var attempts int
	err := Retry(context.Background(), fe.env(), Policy{Attempts: 5, Base: time.Millisecond},
		func(context.Context, int) error {
			attempts++
			if attempts < 3 {
				return errors.New("transient")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("重试后应成功，got %v", err)
	}
	if attempts != 3 {
		t.Fatalf("应为 3 次尝试，got %d", attempts)
	}
}

// TestRetry_AttemptNumbering attempt 从 1 起递增（可观测）。
func TestRetry_AttemptNumbering(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	var seen []int
	_ = Retry(context.Background(), fe.env(), Policy{Attempts: 3, Base: time.Millisecond},
		func(_ context.Context, attempt int) error { seen = append(seen, attempt); return errors.New("x") })
	if fmt.Sprint(seen) != "[1 2 3]" {
		t.Fatalf("attempt 应为 [1 2 3]，got %v", seen)
	}
}

// TestRetry_CancelBeforeFirstAttempt 已取消的 ctx ⇒ 一次都不执行。
func TestRetry_CancelBeforeFirstAttempt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fe := newFakeEnv()
	var attempts int
	err := Retry(ctx, fe.env(), Policy{Attempts: 3},
		func(context.Context, int) error { attempts++; return nil })
	if !errors.Is(err, context.Canceled) || attempts != 0 {
		t.Fatalf("已取消应直接返回且不执行，got err=%v attempts=%d", err, attempts)
	}
}

// TestRunParts_FirstErrorCancelsSiblings 首个错误 ⇒ 取消其余（快速失败）并返回该错误。
func TestRunParts_FirstErrorCancelsSiblings(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	items := make([]int, 32)
	for i := range items {
		items[i] = i // 值唯一：否则错误条件永不成立 ⇒ 兄弟任务永久阻塞（测试自锁）
	}
	var started atomic.Int64
	err := RunParts(context.Background(), items, 4, func(ctx context.Context, it int) error {
		started.Add(1)
		if it == 1 {
			return boom
		}
		<-ctx.Done() // 兄弟任务阻塞直到被取消
		return ctx.Err()
	})
	if !errors.Is(err, boom) {
		t.Fatalf("应返回首个错误，got %v", err)
	}
	if started.Load() >= int64(len(items)) {
		t.Fatalf("首个错误后不应继续派发全部任务，started=%d", started.Load())
	}
}

// TestRunParts_ConcurrencyBounded 并发度不超上限（用 barrier 让并发真正重叠，确定性）。
func TestRunParts_ConcurrencyBounded(t *testing.T) {
	t.Parallel()
	const conc = 3
	var cur, peak atomic.Int64
	var once sync.Once
	release := make(chan struct{})
	items := make([]int, 12)
	err := RunParts(context.Background(), items, conc, func(context.Context, int) error {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		if n >= conc {
			once.Do(func() { close(release) }) // 达到并发上限 ⇒ 放行
		}
		// 等待放行；带超时保护（超时即继续，让断言暴露问题，不挂死测试）
		select {
		case <-release:
		case <-time.After(3 * time.Second):
		}
		cur.Add(-1)
		return nil
	})
	if err != nil {
		t.Fatalf("无错误时应成功，got %v", err)
	}
	if peak.Load() > conc {
		t.Fatalf("并发峰值 %d 超过上限 %d", peak.Load(), conc)
	}
	if peak.Load() < conc {
		t.Fatalf("并发未达上限（peak=%d want=%d）—— 编排未真正并发", peak.Load(), conc)
	}
}

// TestRunParts_AllExecuted 全部执行（无错误时不得漏项）。
func TestRunParts_AllExecuted(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	items := make([]int, 50)
	if err := RunParts(context.Background(), items, 5, func(context.Context, int) error {
		n.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("应成功，got %v", err)
	}
	if n.Load() != 50 {
		t.Fatalf("应执行 50 项，got %d", n.Load())
	}
}

// TestRunParts_EmptyAndZeroConc 空列表 ⇒ nil；并发 0 ⇒ 视为 1（仍全部执行）。
func TestRunParts_EmptyAndZeroConc(t *testing.T) {
	t.Parallel()
	if err := RunParts(context.Background(), nil, 4, func(context.Context, int) error { return nil }); err != nil {
		t.Fatalf("空列表应返回 nil，got %v", err)
	}
	var n atomic.Int64
	if err := RunParts(context.Background(), []int{1, 2, 3}, 0, func(context.Context, int) error {
		n.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("并发 0 应视为 1，got %v", err)
	}
	if n.Load() != 3 {
		t.Fatalf("应执行 3 项，got %d", n.Load())
	}
}

// TestRunParts_CancelPropagates ctx 取消 ⇒ 返回取消错误。
func TestRunParts_CancelPropagates(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	items := make([]int, 8)
	err := RunParts(ctx, items, 2, func(ctx context.Context, it int) error {
		if it == 0 {
			cancel()
			return ctx.Err()
		}
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("应返回 context.Canceled，got %v", err)
	}
}

// TestPolicy_CustomBackoffOverridesCurve 自定义退避函数应完全接管曲线（机制在 pipe、策略在调用点）。
func TestPolicy_CustomBackoffOverridesCurve(t *testing.T) {
	t.Parallel()
	fe := newFakeEnv()
	calls := 0
	pol := Policy{
		Attempts: 4,
		Base:     time.Hour, // 应被 Backoff 覆盖（若生效，退避会是小时级）
		Backoff: func(attempt int) time.Duration {
			calls++
			return time.Duration(attempt) * 10 * time.Millisecond // 自定义曲线：10ms,20ms,30ms
		},
	}
	_ = Retry(context.Background(), fe.env(), pol, func(context.Context, int) error {
		return errors.New("x")
	})
	got := fe.got()
	want := []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	if len(got) != len(want) {
		t.Fatalf("退避次数应为 %d，got %v", len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 次退避应为自定义曲线 %v，got %v", i+1, want[i], got[i])
		}
	}
	if calls != 3 {
		t.Fatalf("自定义 Backoff 应被调用 3 次（attempt 1..3），got %d", calls)
	}
}

// TestPolicy_DelayExported 导出 Delay 与内部计算一致（供调用点复用退避曲线）。
func TestPolicy_DelayExported(t *testing.T) {
	t.Parallel()
	pol := Policy{Attempts: 3, Base: time.Second, Cap: 4 * time.Second, Factor: 2}
	if got := pol.Delay(1); got != time.Second {
		t.Fatalf("Delay(1)=%v want 1s", got)
	}
	if got := pol.Delay(3); got != 4*time.Second {
		t.Fatalf("Delay(3) 应封顶 4s，got %v", got)
	}
	custom := Policy{Backoff: func(int) time.Duration { return 7 * time.Millisecond }}
	if got := custom.Delay(1); got != 7*time.Millisecond {
		t.Fatalf("自定义 Backoff 应优先，got %v", got)
	}
}
