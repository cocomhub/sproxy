// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// upload_progress_test.go 锁定二期 T1（2026-11-10）的进度回调上下文契约：
//   - 注入后可取出并上报；
//   - **未注入 ⇒ 静默 no-op**（历史路径零回归，调用方无需判空）；
//   - **nil 回调 = 清除**（避免"注入了空回调"被误认为可上报）；
//   - 并发上报安全（卷层会并发调用，如 secretdata 块级并发）。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
)

// TestUploadProgress_RoundTrip 注入后可取出并原样上报。
func TestUploadProgress_RoundTrip(t *testing.T) {
	t.Parallel()
	var got []struct {
		stage       UploadStage
		done, total int64
	}
	ctx := WithUploadProgress(context.Background(), func(s UploadStage, d, tt int64) {
		got = append(got, struct {
			stage       UploadStage
			done, total int64
		}{s, d, tt})
	})
	fn, ok := UploadProgressFrom(ctx)
	if !ok || fn == nil {
		t.Fatal("注入后应能取出回调")
	}
	fn(StageEncrypt, 3, 10)
	ReportUploadProgress(ctx, StageRemote, 7, 20)
	if len(got) != 2 {
		t.Fatalf("应上报 2 次，got %d", len(got))
	}
	if got[0].stage != StageEncrypt || got[0].done != 3 || got[0].total != 10 {
		t.Fatalf("第一次上报不符: %+v", got[0])
	}
	if got[1].stage != StageRemote || got[1].done != 7 || got[1].total != 20 {
		t.Fatalf("第二次上报不符: %+v", got[1])
	}
}

// TestUploadProgress_NoopWhenAbsent 未注入 ⇒ 取出失败且上报是 no-op（不得 panic）。
func TestUploadProgress_NoopWhenAbsent(t *testing.T) {
	t.Parallel()
	if _, ok := UploadProgressFrom(context.Background()); ok {
		t.Fatal("未注入时不应取出回调")
	}
	ReportUploadProgress(context.Background(), StageEncrypt, 1, 2) // 不得 panic
	// 注：nil ctx 的防御在实现内（`if ctx == nil`）；此处**不传 nil**（staticcheck SA1012 禁止
	// 显式传 nil context），由实现层的 nil 分支自行保证 no-op 语义。
}

// TestUploadProgress_NilClears nil 回调 = 清除（语义显式，避免静默失效）。
func TestUploadProgress_NilClears(t *testing.T) {
	t.Parallel()
	ctx := WithUploadProgress(context.Background(), func(UploadStage, int64, int64) {})
	if _, ok := UploadProgressFrom(ctx); !ok {
		t.Fatal("先注入应可取出")
	}
	ctx2 := WithUploadProgress(ctx, nil)
	if _, ok := UploadProgressFrom(ctx2); ok {
		t.Fatal("注入 nil 应等价于清除")
	}
}

// TestUploadProgress_ConcurrentSafe 并发上报（卷层块级并发场景）不得竞态/丢计数。
func TestUploadProgress_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	var n atomic.Int64
	ctx := WithUploadProgress(context.Background(), func(UploadStage, int64, int64) { n.Add(1) })
	const goroutines, per = 8, 200
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for i := range per {
				ReportUploadProgress(ctx, StageEncrypt, int64(i+1), per)
			}
		})
	}
	wg.Wait()
	if got := n.Load(); got != int64(goroutines*per) {
		t.Fatalf("应收到 %d 次上报，got %d", goroutines*per, got)
	}
}
