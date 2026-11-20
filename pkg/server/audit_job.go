// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// auditJobTimeout 是后台审计任务的总时限（覆盖上千次外部 CLI 往返；超时即失败而非无限挂起）。
const auditJobTimeout = 2 * time.Hour

// auditJob[T] 是一次**后台**卷审计任务的状态（孤儿审计 / 元数据审计共用）。
//
// 为什么必须异步（2026-11-20 生产实测）：审计要对每个容器列举、并读取/解密每个 file meta，
// 生产卷约 1800 次**外部 CLI 往返**（BaiduPCS-Go 每次 1–3 s）⇒ 同步 HTTP 请求必然超过客户端超时。
//
// 语义：同一卷+同一类审计**单飞**（运行中重复调用只返回状态）；结果缓存到下次 `refresh=1`。
type auditJob[T any] struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	endedAt   time.Time
	result    T
	err       string
	// done 在本次任务结束时关闭（供测试/调用方等待，无需轮询睡眠）。
	done chan struct{}
}

// auditJobState 是任务状态的只读快照。
type auditJobState[T any] struct {
	running   bool
	startedAt time.Time
	endedAt   time.Time
	result    T
	err       string
}

func (j *auditJob[T]) snapshot() auditJobState[T] {
	j.mu.Lock()
	defer j.mu.Unlock()
	return auditJobState[T]{running: j.running, startedAt: j.startedAt, endedAt: j.endedAt,
		result: j.result, err: j.err}
}

// start 在后台启动审计（已在跑 ⇒ 直接返回，不重复启动）。
func (j *auditJob[T]) start(name string, run func(context.Context) (T, error), logger *slog.Logger) {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return
	}
	j.running = true
	j.done = make(chan struct{})
	j.startedAt = time.Now()
	j.endedAt = time.Time{}
	j.err = ""
	j.mu.Unlock()

	go func() {
		// 后台任务用独立 ctx：客户端断开不得中断审计（否则永远拿不到结果）。
		ctx, cancel := context.WithTimeout(context.Background(), auditJobTimeout)
		defer cancel()
		started := time.Now()
		res, err := run(ctx)
		elapsed := time.Since(started)
		j.mu.Lock()
		j.running = false
		j.endedAt = time.Now()
		if err != nil {
			j.err = err.Error()
		} else {
			j.result = res
		}
		close(j.done)
		j.mu.Unlock()
		if logger == nil {
			return
		}
		if err != nil {
			logger.Warn(name+"失败", "elapsed", elapsed.String(), "err", err)
			return
		}
		logger.Info(name+"完成", "elapsed", elapsed.String())
	}()
}

// waitDone 等待本次任务结束（超时返回 false）。不轮询、不睡眠。
func (j *auditJob[T]) waitDone(timeout time.Duration) bool {
	j.mu.Lock()
	done := j.done
	j.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// 具体审计类型（同一实现的类型实例，避免重复代码）。
type (
	orphanAuditJob = auditJob[syncpkg.OrphanReport]
	metaAuditJob   = auditJob[syncpkg.MetaAuditReport]
)
