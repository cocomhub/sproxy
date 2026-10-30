// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package savemerger 提供「全量落盘合并器」：把同一后端的多次写合并为固定窗口内的
// 一次落盘。
//
// 存在理由：台账类存储（checksum / dedup）的 `save()` 都是**全量快照写**（10 万条 ≈ 6MB
// 或一次 KV Put / RTT）；批量 N 文件逐条落盘 = O(N²) I/O。合并后写放大降为 O(N/窗口)。
//
// 崩溃窗口 ≤ debounce；台账类派生状态可由对象 meta 侧车重建，故窗口内丢失可自愈。
// 需要强持久化的调用方调 `Flush()`；生命周期结束调 `Close()`。
package savemerger

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultDebounce 是落盘合并窗口（固定窗口：窗口内不再排新定时器，避免持续写永不落盘）。
const DefaultDebounce = time.Second

const (
	logPersistFailed      = "台账落盘失败"
	logPersistRetryFailed = "台账落盘重试失败"
)

// Merger 把同一后端的多次写合并为一次落盘。并发安全。
type Merger struct {
	debounce time.Duration
	log      *slog.Logger
	// write 由持有者实现（内部做快照 + 落盘）。gen 是调用时刻的脏代际，成功后由本类型
	// 推进 savedGen（写期间若有新写 ⇒ gen 落后 ⇒ 仍保持 dirty，下轮再落盘）。
	write func(gen uint64) error

	dirty atomic.Uint64
	saved atomic.Uint64

	mu       sync.Mutex
	timer    *time.Timer
	closed   bool
	lastOp   string
	lastArgs []any
}

// New 构造合并器；debounce <= 0 用 DefaultDebounce。
func New(debounce time.Duration, log *slog.Logger, write func(gen uint64) error) *Merger {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	return &Merger{debounce: debounce, log: log, write: write}
}

// Mark 记录一次内存写（不落盘），并确保窗口定时器已挂。
func (m *Merger) Mark(op string, args ...any) {
	m.dirty.Add(1)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.timer != nil {
		return
	}
	m.lastOp, m.lastArgs = op, args
	m.timer = time.AfterFunc(m.debounce, m.flushPending)
}

func (m *Merger) flushPending() {
	m.mu.Lock()
	m.timer = nil
	closed := m.closed
	op, args := m.lastOp, m.lastArgs
	m.mu.Unlock()
	if closed {
		return
	}
	if err := m.flushOnce(); err != nil {
		m.log.Error(logPersistFailed, append([]any{"op", op}, args...)...)
		if retryErr := m.flushOnce(); retryErr != nil {
			m.log.Error(logPersistRetryFailed, append([]any{"op", op}, args...)...)
		}
	}
}

// flushOnce 在有脏数据时落盘一次；无脏数据为空操作。
func (m *Merger) flushOnce() error {
	gen := m.dirty.Load()
	if gen <= m.saved.Load() {
		return nil
	}
	if err := m.write(gen); err != nil {
		return err
	}
	m.saved.Store(gen)
	return nil
}

// Flush 立即落盘（有待持久化改动时）。
func (m *Merger) Flush() error {
	m.mu.Lock()
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.mu.Unlock()
	return m.flushOnce()
}

// Close 停止定时器并做最终落盘（幂等）；关闭后的 Mark 仍更新内存但不再持久化。
func (m *Merger) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	if m.timer != nil {
		m.timer.Stop()
		m.timer = nil
	}
	m.mu.Unlock()
	return m.flushOnce()
}

// SetDebounce 调整合并窗口（仅测试用于缩短等待）。
func (m *Merger) SetDebounce(d time.Duration) {
	m.mu.Lock()
	m.debounce = d
	m.mu.Unlock()
}
