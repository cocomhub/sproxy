// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"time"
)

// enqueue 把 writeMsg 投递到 writeCh（非阻塞，满则走默认丢弃语义），并更新发送缓冲
// 水位统计（SendBufferedCurrent/Max）。所有 writeCh <- 的入口都应走本方法（统一观测）。
func (m *Mux) enqueue(msg writeMsg) bool {
	select {
	case m.writeCh <- msg:
		cur := m.metrics.SendBufferedCurrent.Add(1)
		for {
			max := m.metrics.SendBufferedMax.Load()
			if cur <= max || m.metrics.SendBufferedMax.CompareAndSwap(max, cur) {
				break
			}
		}
		return true
	default:
		return false
	}
}

// dequeue 在 writeLoop 消费 writeCh 后递减水位（与 enqueue 对称）。
func (m *Mux) dequeue() {
	m.metrics.SendBufferedCurrent.Add(-1)
}

// currentBufferCap 返回 writeCh 当前容量（帧数）。
func (m *Mux) currentBufferCap() int {
	return int(m.bufferCap.Load())
}

// adjustBufferCap 按水位调整缓冲上限（watermark 配置生效时）：
//   - 水位 > High → 降 Step（下限 MinCap）
//   - 水位 ≤ Low → 恢复默认 defaultWriteChCap
//
// 防抖：调整间最小间隔 5s（lastAdjustNano 记录），避免高频抖动。调整记日志与
// BufferAdjustments 指标（可观测）。
func (m *Mux) adjustBufferCap(watermark BufferWatermarkConfig, watermarkOn bool) {
	if !watermarkOn || watermark.High <= 0 {
		return
	}
	now := time.Now().UnixNano()
	last := m.lastAdjustNano.Load()
	if last != 0 && now-last < int64(5*time.Second) {
		return // 防抖：间隔内不重复调整
	}
	cur := m.metrics.SendBufferedCurrent.Load()
	capNow := m.bufferCap.Load()
	if cur > int64(watermark.High) && capNow > int64(watermark.MinCap) {
		next := max(capNow-int64(watermark.Step), int64(watermark.MinCap))
		m.bufferCap.Store(next)
		m.metrics.BufferAdjustments.Add(1)
		m.lastAdjustNano.Store(now)
		m.logger.Info("mux: 发送缓冲上限下调（高压）", "cap", next, "watermark", cur, "high", watermark.High)
		return
	}
	if cur <= int64(watermark.Low) && capNow < int64(defaultWriteChCap) {
		m.bufferCap.Store(int64(defaultWriteChCap))
		m.metrics.BufferAdjustments.Add(1)
		m.lastAdjustNano.Store(now)
		m.logger.Info("mux: 发送缓冲上限恢复默认（回落）", "cap", defaultWriteChCap, "watermark", cur, "low", watermark.Low)
	}
}
