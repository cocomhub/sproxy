// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

// TestRetransmitMetrics_Success 验证重传成功路径计入 Retransmits：
// 数据帧首次 Send 失败（非 ErrConnClosed）入队 → 退避后重发成功 → Retransmits=1。
// 白盒：直接调 enqueueRetransmit + scanRetransmitQ（readLoop 经 ReceiveFn 阻塞不干扰）。
func TestRetransmitMetrics_Success(t *testing.T) {
	t.Parallel()
	conn := &mockxfer.MockConn{
		SendFn: func(ctx context.Context, msg []byte) error {
			return nil // 重发成功（enqueue 后 scan 即成功）
		},
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := New(conn, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	// 手动入队（模拟 sendFrame 失败路径）。
	m.enqueueRetransmit([]byte("frame"), 0)
	// 立即扫描：deadline 在未来（retryBaseDelay 后），需等到期或直接验证入队计数。
	// scanRetransmitQ 对未到期条目保留；此处验证入队后队列长度（不依赖时钟）。
	m.retransmitMu.Lock()
	queued := len(m.retransmitQ)
	m.retransmitMu.Unlock()
	if queued != 1 {
		t.Fatalf("入队后队列长度=%d want 1", queued)
	}

	// 直接调 scanRetransmitQ 验证重发成功计数（绕过 deadline：改 entry.deadline 为过去）。
	m.retransmitMu.Lock()
	m.retransmitQ[0].deadline = time.Now().Add(-time.Second)
	m.retransmitMu.Unlock()
	m.scanRetransmitQ()

	if got := m.Metrics().Retransmits.Load(); got != 1 {
		t.Fatalf("重传成功后 Retransmits=%d want 1", got)
	}
}

// TestRetransmitMetrics_Exhausted 验证重传耗尽计入 RetransmitExhausted：
// SendFn 恒失败 → scanRetransmitQ 重试耗尽 → 计数 +1。
func TestRetransmitMetrics_Exhausted(t *testing.T) {
	t.Parallel()
	conn := &mockxfer.MockConn{
		SendFn: func(ctx context.Context, msg []byte) error {
			return mockxfer.ErrSendFailed // 恒失败 → 耗尽
		},
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := New(conn, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	m.enqueueRetransmit([]byte("frame"), 0)
	// 循环 scan：每次失败 retries+1，直到 >= maxRetries 耗尽（maxRetries 有界，防死循环）。
	for range maxRetries + 1 {
		m.retransmitMu.Lock()
		if len(m.retransmitQ) > 0 {
			m.retransmitQ[0].deadline = time.Now().Add(-time.Second)
		}
		m.retransmitMu.Unlock()
		m.scanRetransmitQ()
		if m.Metrics().RetransmitExhausted.Load() > 0 {
			break
		}
	}

	if got := m.Metrics().RetransmitExhausted.Load(); got == 0 {
		t.Fatal("重传耗尽后 RetransmitExhausted 应 > 0")
	}
}

// TestRetransmitMetrics_QueueFull 验证重传队列满关闭计入 RetransmitQueueFull：
// 入队 maxRetransmitQ 个后下一次触发队列满（fail-closed 关闭）。
func TestRetransmitMetrics_QueueFull(t *testing.T) {
	t.Parallel()
	conn := &mockxfer.MockConn{
		SendFn: func(ctx context.Context, msg []byte) error {
			return mockxfer.ErrSendFailed
		},
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := New(conn, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	for range maxRetransmitQ {
		m.enqueueRetransmit([]byte("x"), 0)
	}
	m.enqueueRetransmit([]byte("x"), 0) // 第 maxRetransmitQ+1 个 → 队列满 → 关闭

	if got := m.Metrics().RetransmitQueueFull.Load(); got == 0 {
		t.Fatal("队列满关闭后 RetransmitQueueFull 应 > 0")
	}
	// 关闭是异步（go m.Close()），等 mux done。
	select {
	case <-m.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("队列满后 mux 应关闭")
	}
}

// TestRetransmitMetrics_NoSpurious 验证无重传时计数器为 0（默认零值不虚增）。
func TestRetransmitMetrics_NoSpurious(t *testing.T) {
	t.Parallel()
	conn := &mockxfer.MockConn{
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := New(conn, RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	mm := m.Metrics()
	if mm.Retransmits.Load() != 0 || mm.RetransmitQueueFull.Load() != 0 || mm.RetransmitExhausted.Load() != 0 {
		t.Fatal("无重传时重传指标应为 0")
	}
}
