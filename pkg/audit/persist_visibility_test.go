// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// persist_visibility_test.go 锁定「**写入失败绝不静默**」：
//
// 背景（2026-10-20 M3 排查）：`persist` 在 best-effort 模式下会**吞掉** `Journal.Append` 的错误
// （返回 nil，只累计 metrics + 打日志）——这对业务是对的（审计绝不阻断业务），但**对运维是黑洞**：
// 若调用方没注入 `Options.Logger`，落盘失败将**无任何可见痕迹**，而上层（如恢复协调器）会把
// `MarkDone` 的 nil 误读为「提交记录已写入」✗。
//
// 因此 `New` 在未注入 Logger 时**兜底为 `slog.Default()`**（写失败至少进标准日志），并保证
// metrics 计数一定递增（两条独立可观测通道）。

import (
	"context"
	"errors"
	"testing"
)

// visFailJournal 是所有写入都失败的 Journal 替身（前缀 vis 防与包内既有替身重名）。
type visFailJournal struct {
	NullJournal
	appends int
}

func (j *visFailJournal) Append(Entry) error {
	j.appends++
	return errors.New("磁盘不可写（测试替身）")
}

// visFailMetrics 只统计写入失败数（嵌入 NullMetrics ⇒ 其余方法零值）。
type visFailMetrics struct {
	NullMetrics
	failed int
}

func (m *visFailMetrics) IncrWriteFailed() { m.failed++ }

// TestService_WriteFailureIsObserved 未注入 Logger 时：落盘失败仍被**计数**且不阻断业务。
func TestService_WriteFailureIsObserved(t *testing.T) {
	t.Parallel()
	j := &visFailJournal{}
	m := &visFailMetrics{}
	caps := FullCapabilities()
	s, err := New(Options{Journal: j, Caps: &caps, Metrics: m})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	sc, oerr := s.Open(ctx, "task.cloud.obs1")
	if oerr != nil {
		t.Fatalf("Open: %v", oerr)
	}
	// ① 业务写入 best-effort：Save 返回 nil（绝不因审计失败阻断业务）。
	if serr := sc.Save(ctx, "k", 1); serr != nil {
		t.Fatalf("best-effort 下 Save 不应返回错误: %v", serr)
	}
	// ② **提交记录例外**：`MarkDone` 是「任务已完成」的唯一凭据 —— 没落盘却返回 nil 会让
	//    协调器误报「已恢复并提交」，下次重启**重复执行副作用**（评审 R13 P2）⇒ 必须报错。
	if merr := sc.MarkDone(ctx); merr == nil {
		t.Fatal("MarkDone 在提交记录落盘失败时必须返回错误（否则会重复执行任务）")
	}
	// ③ 失败后内存 done 必须**回滚**：再次 MarkDone 仍会真正尝试写入（而不是直接 return nil）。
	before := j.appends
	_ = sc.MarkDone(ctx)
	if j.appends == before {
		t.Fatal("MarkDone 失败后应回滚 done 并允许重试")
	}
	// ④ 但失败必须**可观测**（两条通道之一：metrics 计数）。
	if j.appends == 0 {
		t.Fatal("应尝试过 Append（替身未被调用说明写入路径变了）")
	}
	if m.failed != j.appends {
		t.Fatalf("写入失败计数应与实际失败次数一致: failed=%d appends=%d", m.failed, j.appends)
	}
	// ③ New 必须兜底 logger（否则失败在无注入时完全不可见）。
	if s.eff.logger == nil {
		t.Fatal("未注入 Logger 时 New 必须兜底 slog.Default()（写入失败不得静默）")
	}
}
