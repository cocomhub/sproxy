// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

// rtt_test.go —— 心跳 RTT 采样（roadmap 11.1-⑦ P1）：
// pingLoop 发 Ping 时记录 pingSentAtNano；handlePongFrame 收到 Pong 按
// `now - pingSentAt` 更新 Metrics.LastRTTNanos（无在途 ping 时跳过，避免把
// 对端主动 Pong / 窗口外补发误算成本侧 RTT）。
//
// 用例直接调用 handlePongFrame（包内白盒）而非等待 30s 心跳，确定且零固定等待。

import (
	"testing"
	"time"
)

// TestMuxRTT_UpdatedOnPong：收到 Pong → LastRTTNanos 按 pingSentAt 更新。
// 变异点：handlePongFrame 不更新 RTT / 用错时钟基准 → 本用例红。
func TestMuxRTT_UpdatedOnPong(t *testing.T) {
	t.Parallel()
	m, _ := newTestMux(t)

	sent := time.Now().Add(-10 * time.Millisecond).UnixNano() // 模拟 10ms 前的在途 Ping
	m.pingSentAtNano.Store(sent)

	handlePongFrame(m, 0, nil)

	got := m.Metrics().LastRTTNanos.Load()
	if got <= 0 {
		t.Fatalf("LastRTTNanos = %d, want > 0（Pong 到达应按 pingSentAt 更新 RTT）", got)
	}
	// 量级校验：RTT 应接近 10ms（≤ 实际经过时间，> 0）。
	elapsed := time.Since(time.Unix(0, sent)).Nanoseconds()
	if got > elapsed {
		t.Fatalf("LastRTTNanos(%d) 大于实际经过时间(%d)，RTT 计算有误", got, elapsed)
	}
	// 在途标记消费后复位：下一次无在途 ping 的 Pong 不再覆盖（防旧 ping 污染）。
	if after := m.pingSentAtNano.Load(); after != 0 {
		t.Fatalf("pingSentAtNano 应复位为 0（在途标记已被 Pong 消费）, got %d", after)
	}
}

// TestMuxRTT_SkippedWhenNoPingInFlight：无在途 ping（pingSentAtNano==0）时
// Pong 不得更新 LastRTTNanos——对端主动 Pong 不能冒充本侧 RTT 采样。
// 变异点：去掉「无在途跳过」守卫 → 本用例红。
func TestMuxRTT_SkippedWhenNoPingInFlight(t *testing.T) {
	t.Parallel()
	m, _ := newTestMux(t)

	m.Metrics().LastRTTNanos.Store(999) // 既有采样
	m.pingSentAtNano.Store(0)           // 无在途 ping

	handlePongFrame(m, 0, nil)

	if got := m.Metrics().LastRTTNanos.Load(); got != 999 {
		t.Fatalf("无在途 ping 时 Pong 不应更新 RTT：got %d, want 999（保持旧采样）", got)
	}
}
