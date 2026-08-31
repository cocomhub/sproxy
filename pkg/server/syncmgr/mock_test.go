// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"time"
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
	// started 是非阻塞执行器/阻塞执行器的「Run 被调用」一次性信号（close 一次）：
	// 测试用确定性 channel 同步「任务已真正开始执行（已拿信号量）」，而非死等固定超时
	// 轮询状态（对齐 flaky-network-test-pattern 教训，TestConcurrency_Semaphore）。
	started   chan struct{}
	startOnce sync.Once
}

// newMockExecutor 构造返回固定结果的 mock 执行器。
func newMockExecutor(result *RunResult) *mockExecutor {
	return &mockExecutor{result: result}
}

// newBlockingMockExecutor 构造阻塞直到 ctx 取消或 release 的 mock 执行器；
// 释放后返回一个"完成"结果（1 文件 5 字节）。带 started 信号（Run 被调用时 close）。
func newBlockingMockExecutor() *mockExecutor {
	return &mockExecutor{
		result:  &RunResult{Status: StatusCompleted, FilesTotal: 1, FilesDone: 1, BytesTotal: 5, BytesDone: 5},
		blockCh: make(chan struct{}),
		started: make(chan struct{}),
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

	// 确定性信号：Run 被调用 = 任务已拿到信号量、开始执行（close 一次）。
	if m.started != nil {
		m.startOnce.Do(func() { close(m.started) })
	}

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

// retryMockExecutor 按脚本依次返回 RunResult，并记录每次 Run 的调用时间戳
// （用于验证指数退避时序）。steps 耗尽后重复最后一个结果。不返回 error。
type retryMockExecutor struct {
	mu      sync.Mutex
	steps   []*RunResult
	idx     int
	calls   int
	times   []time.Time
	started chan struct{}
	once    sync.Once
}

func newRetryMockExecutor(steps []*RunResult) *retryMockExecutor {
	return &retryMockExecutor{steps: steps, started: make(chan struct{})}
}

func (m *retryMockExecutor) Run(_ context.Context, _ *SyncTask, _ RemoteConfig) (*RunResult, error) {
	m.mu.Lock()
	m.calls++
	m.times = append(m.times, time.Now())
	call := m.calls
	var res *RunResult
	if m.idx < len(m.steps) {
		res = m.steps[m.idx]
		m.idx++
	} else if len(m.steps) > 0 {
		res = m.steps[len(m.steps)-1]
	}
	m.mu.Unlock()
	if call == 1 {
		m.once.Do(func() { close(m.started) })
	}
	return res, nil
}

// callCount 返回 Run 被调用的次数。
func (m *retryMockExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

// gaps 返回相邻 Run 调用之间的时间间隔（len = calls-1）。
func (m *retryMockExecutor) gaps() []time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	gaps := make([]time.Duration, 0, len(m.times)-1)
	for i := 1; i < len(m.times); i++ {
		gaps = append(gaps, m.times[i].Sub(m.times[i-1]))
	}
	return gaps
}

var _ Executor = (*retryMockExecutor)(nil)

// blockingRetryExecutor：第 1 次 Run 阻塞直到 releaseFirst 后返回可重试错误；
// 第 2 次 Run 阻塞直到 releaseSecond 后返回 completed。用于确定性观察 retrying 状态
// 与取消/删除在退避等待期间的行为（对齐 flaky-network-test-pattern：channel 信号
// 而非死等轮询）。
type blockingRetryExecutor struct {
	mu            sync.Mutex
	calls         int
	started       chan struct{} // 第 1 次 Run 被调用时 close
	secondStarted chan struct{} // 第 2 次 Run 被调用时 close
	firstRelease  chan struct{} // close 使第 1 次 Run 返回可重试错误
	secondRelease chan struct{} // close 使第 2 次 Run 返回 completed
}

func newBlockingRetryExecutor() *blockingRetryExecutor {
	return &blockingRetryExecutor{
		started:       make(chan struct{}),
		secondStarted: make(chan struct{}),
		firstRelease:  make(chan struct{}),
		secondRelease: make(chan struct{}),
	}
}

func (m *blockingRetryExecutor) Run(_ context.Context, _ *SyncTask, _ RemoteConfig) (*RunResult, error) {
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	switch call {
	case 1:
		close(m.started)
		<-m.firstRelease
		return &RunResult{Status: StatusFailed, Retryable: true, Error: "网络错误: connection refused"}, nil
	case 2:
		close(m.secondStarted)
		<-m.secondRelease
		return completedResult(), nil
	}
	return completedResult(), nil
}

func (m *blockingRetryExecutor) releaseFirst()  { close(m.firstRelease) }
func (m *blockingRetryExecutor) releaseSecond() { close(m.secondRelease) }

// callCount 返回 Run 被调用的次数。
func (m *blockingRetryExecutor) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

var _ Executor = (*blockingRetryExecutor)(nil)
