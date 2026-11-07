// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pipe 提供**纯编排原语**：可中断重试退避、受限并发执行、可注入时钟/睡眠。
//
// 设计依据：`docs/design-http-orchestration.md`（L0 层）。本包**零 HTTP 知识、零业务语义**，
// 因此 HTTP 交互（`pkg/httpc`）、本地 FS 重试、迁移逐项处理等场景都可复用同一份机制。
//
// 为什么需要它（量化，2026-11-08 实测本仓库）：指数退避被各写一遍 ≥5 处
// （`pkg/backup` / `pkg/leader/local.go` / `pkg/tunnel/mesh/node.go` / `pkg/syncmgr/manager.go` /
// `pkg/server/remotefs/accept_retry.go`），裸受限并发 worker 池 ≥10 处 —— 违反收敛原则
// 「同一语义只留一份实现」。
//
// 两条硬约束：
//  1. **任何等待都必须可被 ctx 打断**（默认 Sleeper 用 timer + select，**不用 time.Sleep**）——
//     这正是 baidupcs 上传「卡 ~90s 且不可取消」事故的根因；
//  2. **机制与策略分离**：本包只提供机制，退避大小/次数/可重试判定由调用点声明。
package pipe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Env 是可注入的运行环境（测试可零等待；nil 字段用默认实现）。
type Env struct {
	// Sleep 等待 d，**必须可被 ctx 打断**（nil = 默认 timer+select 实现；不得用 time.Sleep）。
	Sleep func(ctx context.Context, d time.Duration) error
}

func (e Env) sleep(ctx context.Context, d time.Duration) error {
	if e.Sleep != nil {
		return e.Sleep(ctx, d)
	}
	return Sleep(ctx, d)
}

// Sleep 是可取消的等待（默认实现；**刻意不用 time.Sleep**）。
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ErrAttemptsExhausted 表示重试次数耗尽（包裹最后一次错误）。
var ErrAttemptsExhausted = errors.New("pipe: 尝试次数耗尽")

// Policy 是重试策略（**由调用点声明**，本包不预设业务语义）。
type Policy struct {
	Attempts  int                                               // 总尝试次数（<=0 视为 1，即不重试）
	Base      time.Duration                                     // 首次退避（<=0 视为 0 ⇒ 不等待）
	Cap       time.Duration                                     // 退避上限（<=0 视为不封顶）
	Factor    float64                                           // 退避倍率（<=0 视为 2）
	Jitter    bool                                              // 是否加抖动（±25%，避免同步重试风暴）
	Retryable func(err error) bool                              // nil = 全部可重试；返回 false ⇒ 立刻返回该错误
	OnRetry   func(attempt int, delay time.Duration, err error) // 可选：重试回调（观测用）
}

// delay 返回第 attempt 次失败后应等待的时长（attempt 从 1 起）。
func (p Policy) delay(attempt int) time.Duration {
	f := p.Factor
	if f <= 0 {
		f = 2
	}
	if p.Base <= 0 {
		return 0
	}
	d := float64(p.Base)
	for i := 1; i < attempt; i++ {
		d *= f
		if p.Cap > 0 && d >= float64(p.Cap) {
			d = float64(p.Cap)
			break
		}
	}
	if p.Cap > 0 && d > float64(p.Cap) {
		d = float64(p.Cap)
	}
	if p.Jitter {
		d *= 0.75 // 只向下抖动，保证不超过 Cap
	}
	return time.Duration(d)
}

// Retry 按策略执行 op；每次尝试的 attempt 从 1 起。
//
// 行为保证：
//   - ctx 取消 ⇒ 立刻返回 ctx.Err()（**不再发起下一次尝试**）；
//   - 退避期间 ctx 取消 ⇒ 返回 ctx.Err()（可中断）；
//   - 不可重试错误 ⇒ 原样返回（不做无谓重试）；
//   - 次数耗尽 ⇒ 返回包裹 ErrAttemptsExhausted 的错误（errors.Is 可判定）。
func Retry(ctx context.Context, env Env, p Policy, op func(ctx context.Context, attempt int) error) error {
	if op == nil {
		return errors.New("pipe: op 不能为空")
	}
	attempts := p.Attempts
	if attempts <= 0 {
		attempts = 1
	}
	var last error
	for attempt := 1; attempt <= attempts; attempt++ {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		runErr := op(ctx, attempt)
		if runErr == nil {
			return nil
		}
		last = runErr
		if cerr := ctx.Err(); cerr != nil {
			return cerr // 操作本身可能因取消失败：直接返回取消原因
		}
		if !p.allowRetry(runErr) {
			return runErr
		}
		if attempt == attempts {
			break
		}
		if werr := p.waitBefore(ctx, env, attempt, runErr); werr != nil {
			return werr
		}
	}
	return fmt.Errorf("%w（%d 次）: %w", ErrAttemptsExhausted, attempts, last)
}

// allowRetry 判定该错误是否可重试（nil Retryable = 全部可重试）。
func (p Policy) allowRetry(err error) bool {
	return p.Retryable == nil || p.Retryable(err)
}

// waitBefore 执行退避等待（含可选 OnRetry 回调）；等待**可被 ctx 打断**。
func (p Policy) waitBefore(ctx context.Context, env Env, attempt int, err error) error {
	d := p.delay(attempt)
	if p.OnRetry != nil {
		p.OnRetry(attempt, d, err)
	}
	return env.sleep(ctx, d)
}

// RunParts 以**受限并发**执行 items 上的 fn（并发上限 <=0 视为 1）。
//
// 行为保证：
//   - 首个错误 ⇒ 取消其余在途任务（快速失败），并返回该错误；
//   - 已成功的结果**不回滚**（调用方按幂等语义重入时可跳过）；
//   - ctx 取消 ⇒ 尽早返回 ctx.Err()；
//   - 结果顺序无关（调用方若需要顺序应自行收集）。
func RunParts[T any](ctx context.Context, items []T, conc int, fn func(ctx context.Context, item T) error) error {
	if len(items) == 0 {
		return nil
	}
	if conc <= 0 {
		conc = 1
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error

	for i := range items {
		item := items[i]
		mu.Lock()
		stop := firstErr != nil
		mu.Unlock()
		if stop || ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := fn(ctx, item); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
