// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"io"
	"log/slog"
	"sync"
)

// discardLogger 返回丢弃日志的测试 logger，避免测试输出噪音。
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// mockQuota 实现 QuotaStore（内存计数，max=0 不限制）。
type mockQuota struct {
	mu   sync.Mutex
	max  int64
	used int64
}

func newMockQuota(max int64) *mockQuota { return &mockQuota{max: max} }

func (q *mockQuota) TryReserve(size int64, _ int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.max > 0 && q.used+size > q.max {
		return ErrStorageFull
	}
	q.used += size
	return nil
}

func (q *mockQuota) Release(size int64, _ int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.used -= size
	if q.used < 0 {
		q.used = 0
	}
}

func (q *mockQuota) Usage() int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.used
}

func (q *mockQuota) MaxBytes() int64 { return q.max }

// mockExecutor 模拟同步执行：返回可配置结果、错误或阻塞（直到 ctx 取消或释放）。
type mockExecutor struct {
	mu       sync.Mutex
	result   *RunResult
	err      error
	blockCh  chan struct{} // 非 nil 时 Run 阻塞直到关闭或 ctx 取消
	calls    int
	lastTask *SyncTask
}

// newMockExecutor 构造返回固定结果的 mock 执行器。
func newMockExecutor(result *RunResult) *mockExecutor {
	return &mockExecutor{result: result}
}

// newBlockingMockExecutor 构造阻塞直到 ctx 取消或 release 的 mock 执行器；
// 释放后返回一个"完成"结果（1 文件 5 字节）。
func newBlockingMockExecutor() *mockExecutor {
	return &mockExecutor{
		result:  &RunResult{Status: StatusCompleted, FilesTotal: 1, FilesDone: 1, BytesTotal: 5, BytesDone: 5},
		blockCh: make(chan struct{}),
	}
}

func (m *mockExecutor) Run(ctx context.Context, task *SyncTask, _ RemoteConfig) (*RunResult, error) {
	m.mu.Lock()
	m.calls++
	m.lastTask = task
	res := m.result
	err := m.err
	block := m.blockCh
	m.mu.Unlock()

	if block != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-block:
		}
	}
	if err != nil {
		return nil, err
	}
	return res, nil
}

// release 释放阻塞的 Run（供测试在取消/完成后解除阻塞）。
func (m *mockExecutor) release() {
	m.mu.Lock()
	ch := m.blockCh
	m.blockCh = nil
	m.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// callCount 返回 Run 被调用的次数。
func (m *mockExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

var _ Executor = (*mockExecutor)(nil)
