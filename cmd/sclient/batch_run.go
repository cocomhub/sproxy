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

// ProgressSink 接收批量任务进度更新（stderr 进度条 / no-op 两种实现）。
type ProgressSink interface {
	Report(Progress)
}

// noopProgressSink 是显式关闭进度条时的空实现。
type noopProgressSink struct{}

func (noopProgressSink) Report(Progress) {}

// runBatchConcurrent 并发执行 ops（每 op 调 exec），返回按输入顺序排列的结果。
//
//   - workers <= 1 → 串行退化（与逐行执行逐字节一致，脚本兼容零回归）；
//   - workers > len(ops) → 钳位到 len(ops)；
//   - 信号量（buffered chan）限并发峰值 <= workers；
//   - 单 op 失败不中断其余；exec panic 由 recover 捕获并转为该 op FAIL（不拖垮整批）；
//   - ctx 取消时：在飞 op 完成后退出，未开始 op 标记 Skipped；
//   - progress 非 nil 时每个完成 op 回调一次 Report。
func runBatchConcurrent(ctx context.Context, ops []string, workers int, exec func(raw string) batchOperationResult, progress ProgressSink) []batchOperationResult {
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

	for i, raw := range ops {
		select {
		case <-ctx.Done():
			// 取消：剩余 op 标记 Skipped（不阻塞，不执行）。
			out[i] = batchOperationResult{Name: raw, Success: false, Message: "Skipped（任务已取消）"}
			continue
		default:
		}
		wg.Add(1)
		go func(idx int, raw string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				out[idx] = batchOperationResult{Name: raw, Success: false, Message: "Skipped（任务已取消）"}
				return
			}
			defer func() { <-sem }()
			// 入队后再次检查取消（避免取消与排队竞争时仍执行）。
			select {
			case <-ctx.Done():
				out[idx] = batchOperationResult{Name: raw, Success: false, Message: "Skipped（任务已取消）"}
				return
			default:
			}
			res := runBatchOperationSafe(exec, raw)
			if !res.Success {
				failed.Add(1)
			}
			done.Add(1)
			out[idx] = res
			progress.Report(Progress{Total: len(ops), Done: int(done.Load()), Failed: int(failed.Load())})
		}(i, raw)
	}
	wg.Wait()
	return out
}

// runBatchOperationSafe 调 exec 并把 panic 捕获为该 op 的 FAIL 结果
// （一个 op panic 不拖垮整批）。
func runBatchOperationSafe(exec func(raw string) batchOperationResult, raw string) (res batchOperationResult) {
	defer func() {
		if r := recover(); r != nil {
			res = batchOperationResult{Name: raw, Success: false, Message: fmt.Sprintf("panic: %v", r)}
		}
	}()
	return exec(raw)
}
