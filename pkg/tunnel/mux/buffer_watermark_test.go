// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"context"
	"testing"
	"time"
)

// TestBufferWatermark_DefaultOff 未配置 watermark = 行为不变（零回归）。
func TestBufferWatermark_DefaultOff(t *testing.T) {
	t.Parallel()
	m := NewWithOpts(&discardConn{}, RoleDialer)
	if m.watermark.High != 0 {
		t.Fatalf("默认 watermark.High = %d, want 0（关闭）", m.watermark.High)
	}
	if got := m.currentBufferCap(); got != defaultWriteChCap {
		t.Fatalf("默认 bufferCap = %d, want %d", got, defaultWriteChCap)
	}
}

// TestBufferWatermark_AdjustDownUp 水位持续 > High → 降 Step（下限 MinCap）；
// 回落 ≤ Low → 恢复默认。
func TestBufferWatermark_AdjustDownUp(t *testing.T) {
	t.Parallel()
	m := NewWithOpts(&discardConn{}, RoleDialer, WithBufferWatermark(BufferWatermarkConfig{
		High: 8, Low: 2, Step: 2, MinCap: 4,
	}))
	// 高压：水位 > High → 逐步降 Step（模拟从 12 开始，验证降级链 + MinCap 下限）。
	m.bufferCap.Store(12)
	m.metrics.SendBufferedCurrent.Store(10)
	// 模拟防抖已过（lastAdjust 设 10s 前），连续多次调整验证降级链。
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != 10 {
		t.Fatalf("高压第一次调整 cap = %d, want 10（12-2）", got)
	}
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != 8 {
		t.Fatalf("高压第二次调整 cap = %d, want 8", got)
	}
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != 6 {
		t.Fatalf("高压第三次调整 cap = %d, want 6", got)
	}
	// 继续高压到 MinCap 4 下限。
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != 4 {
		t.Fatalf("高压第四次调整 cap = %d, want 4（MinCap 下限）", got)
	}
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != 4 {
		t.Fatalf("下限后不应再降, cap = %d, want 4", got)
	}
	// 回落：水位 ≤ Low → 恢复默认。
	m.metrics.SendBufferedCurrent.Store(1)
	m.lastAdjustNano.Store(time.Now().UnixNano() - int64(10*time.Second))
	m.adjustBufferCap(m.watermark, true)
	if got := m.currentBufferCap(); got != defaultWriteChCap {
		t.Fatalf("回落后 cap = %d, want 默认 %d", got, defaultWriteChCap)
	}
}

// TestBufferWatermark_EnqueueDequeue 水位统计：enqueue 增 / dequeue 减。
func TestBufferWatermark_EnqueueDequeue(t *testing.T) {
	t.Parallel()
	m := NewWithOpts(&discardConn{}, RoleDialer)
	if m.enqueue(writeMsg{}) {
		if got := m.metrics.SendBufferedCurrent.Load(); got != 1 {
			t.Fatalf("enqueue 后水位 = %d, want 1", got)
		}
		m.dequeue()
		if got := m.metrics.SendBufferedCurrent.Load(); got != 0 {
			t.Fatalf("dequeue 后水位 = %d, want 0", got)
		}
	}
}

// TestBufferWatermark_DisabledNoAdjust watermark 关闭时 adjust 不生效。
func TestBufferWatermark_DisabledNoAdjust(t *testing.T) {
	t.Parallel()
	m := NewWithOpts(&discardConn{}, RoleDialer) // 未配置 watermark
	m.metrics.SendBufferedCurrent.Store(100)
	m.adjustBufferCap(m.watermark, false)
	if got := m.currentBufferCap(); got != defaultWriteChCap {
		t.Fatalf("watermark 关时 cap 不应变, got %d", got)
	}
}

// TestBufferWatermark_MetricsExposed SendBuffered 指标可查（metrics 接线）。
func TestBufferWatermark_MetricsExposed(t *testing.T) {
	t.Parallel()
	m := NewWithOpts(&discardConn{}, RoleDialer)
	m.metrics.SendBufferedCurrent.Store(5)
	m.metrics.SendBufferedMax.Store(9)
	if got := m.metrics.SendBufferedCurrent.Load(); got != 5 {
		t.Fatalf("SendBufferedCurrent = %d, want 5", got)
	}
	if got := m.metrics.SendBufferedMax.Load(); got != 9 {
		t.Fatalf("SendBufferedMax = %d, want 9", got)
	}
}

// discardConn 是测试用 xfer.Conn（无操作）。
type discardConn struct{}

func (c *discardConn) Send(ctx context.Context, msg []byte) error  { return nil }
func (c *discardConn) Receive(ctx context.Context) ([]byte, error) { return nil, nil }
func (c *discardConn) Close() error                                { return nil }
