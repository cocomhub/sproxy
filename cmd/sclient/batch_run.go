// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// Progress 是一次批量任务的整体进度快照。
type Progress struct {
	Total  int
	Done   int
	Failed int
}

// ProgressReporter 接收批量任务进度更新（stderr 进度条 / no-op 两种实现）。
type ProgressReporter interface {
	Report(Progress)
}

// noopProgressSink 是显式关闭进度条时的空实现。
type noopProgressSink struct{}

func (noopProgressSink) Report(Progress) { /* noop：显式关闭进度条 */ }

// batchWorkerCtx 是 runBatchWorker 的共享执行上下文（S107 收敛）：信号量/执行器/输出/
// 原子计数/进度上报在一次批量运行中全程不变，各 goroutine 只按 idx/raw 差异化。
type batchWorkerCtx struct {
	sem      chan struct{}
	exec     func(raw string) batchOperationResult
	out      []batchOperationResult
	done     *atomic.Int64
	failed   *atomic.Int64
	progress ProgressReporter
	total    int
}

// runBatchConcurrent 并发执行 ops（每 op 调 exec），返回按输入顺序排列的结果。
//
//   - workers <= 1 → 串行退化（与逐行执行逐字节一致，脚本兼容零回归）；
//   - workers > len(ops) → 钳位到 len(ops)；
//   - 信号量（buffered chan）限并发峰值 <= workers；
//   - 单 op 失败不中断其余；exec panic 由 recover 捕获并转为该 op FAIL（不拖垮整批）；
//   - ctx 取消时：在飞 op 完成后退出，未开始 op 标记 Skipped；
//   - progress 非 nil 时每个完成 op 回调一次 Report。
func runBatchConcurrent(ctx context.Context, ops []string, workers int, exec func(raw string) batchOperationResult, progress ProgressReporter) []batchOperationResult {
	if progress == nil {
		progress = noopProgressSink{}
	}
	if workers <= 1 {
		workers = 1
	}
	if workers > len(ops) {
		workers = len(ops)
	}
	if workers < 1 {
		workers = 1
	}
	out := make([]batchOperationResult, len(ops))
	if len(ops) == 0 {
		return out
	}

	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	var done, failed atomic.Int64
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// 共享 worker 上下文（S107 收敛）：信号量/执行器/输出/原子计数/进度在一次批量
	// 运行中全程不变，各 goroutine 只按 idx/raw 差异化。
	wc := &batchWorkerCtx{
		sem: sem, exec: exec, out: out, done: &done, failed: &failed,
		progress: progress, total: len(ops),
	}

	for i, raw := range ops {
		select {
		case <-ctx.Done():
			// 取消：剩余 op 标记 Skipped（不阻塞，不执行）。
			out[i] = batchOperationResult{Name: raw, Success: false, Message: msgBatchSkipped}
			continue
		default:
		}
		wg.Add(1)
		// 入队 + 二次确认 + 执行 + 进度上报收敛到 runBatchWorker（与主循环分离，降 CC）。
		go func(idx int, raw string) {
			defer wg.Done()
			runBatchWorker(ctx, wc, idx, raw)
		}(i, raw)
	}
	wg.Wait()
	// 终值收敛：并发完成时各 goroutine 的 Report 携带的是『当时的』原子快照，
	// 最后一次持锁写入未必是 done=len(ops) —— 直接断言 collectProgress.done==len(ops)
	// 会偶发失败（TestRunBatchConcurrent_ProgressMatches flake）。
	// 因此 Wait 后补发一次终值，保证 progress 必收敛到完成态（幂等）。
	progress.Report(Progress{Total: len(ops), Done: int(done.Load()), Failed: int(failed.Load())})
	return out
}

// runBatchWorker 执行单个 batch op（在已启动的 goroutine 中）：
// 信号量排队 →（取消时标记 Skipped）→ 入队后再次确认取消（避免取消与排队
// 竞争时仍执行）→ runBatchOperationSafe 执行并更新原子进度计数/上报。
// exec panic 由 runBatchOperationSafe 捕获为该 op FAIL（不拖垮整批）。
func runBatchWorker(ctx context.Context, wc *batchWorkerCtx, idx int, raw string) {
	select {
	case wc.sem <- struct{}{}:
	case <-ctx.Done():
		wc.out[idx] = batchOperationResult{Name: raw, Success: false, Message: msgBatchSkipped}
		return
	}
	defer func() { <-wc.sem }()
	// 入队后再次检查取消（避免取消与排队竞争时仍执行）。
	select {
	case <-ctx.Done():
		wc.out[idx] = batchOperationResult{Name: raw, Success: false, Message: msgBatchSkipped}
		return
	default:
	}
	res := runBatchOperationSafe(wc.exec, raw)
	if !res.Success {
		wc.failed.Add(1)
	}
	wc.done.Add(1)
	wc.out[idx] = res
	wc.progress.Report(Progress{Total: wc.total, Done: int(wc.done.Load()), Failed: int(wc.failed.Load())})
}

// runBatchOperationSafe 调执行并把 panic 捕获为该 op 的 FAIL 结果
// （一个 op panic 不拖垮整批）。
func runBatchOperationSafe(exec func(raw string) batchOperationResult, raw string) (res batchOperationResult) {
	defer func() {
		if r := recover(); r != nil {
			res = batchOperationResult{Name: raw, Success: false, Message: fmt.Sprintf("panic: %v", r)}
		}
	}()
	return exec(raw)
}
