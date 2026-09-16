// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mux

import (
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestStreamStats_ActiveTracksOpenClose 钉住 Active/MaxActive 随流 Open/Close 正确增减
// （审计 F6 修法②的可观测性：acceptor 侧流从不主动 Close，对端失联时 Active 滞留 ⇒ 运维面可见）。
//
// 经 Mux 公开 API（Open/Close/Abort）驱动，断言 Metrics().Streams.Active 与 StreamStats().ActiveStreams
// 同步、MaxActive 记录峰值且单调不降。
func TestStreamStats_ActiveTracksOpenClose(t *testing.T) {
	t.Parallel()

	pipeA, _ := xfertest.Pipe()
	m := New(pipeA, RoleDialer)
	defer m.Close()

	// 初始：无流。
	if got := m.Metrics().Streams.Active.Load(); got != 0 {
		t.Fatalf("初始 Active = %d, want 0", got)
	}

	// 开 3 条流：Active 递增、MaxActive 记录峰值。
	var streams []Stream
	for i := range 3 {
		s, err := m.Open(t.Context())
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		streams = append(streams, s)
		if got := m.Metrics().Streams.Active.Load(); got != int64(i+1) {
			t.Fatalf("Open 后 Active = %d, want %d", got, i+1)
		}
	}
	if got := m.Metrics().Streams.MaxActive.Load(); got != 3 {
		t.Fatalf("MaxActive = %d, want 3", got)
	}
	if got := m.StreamStats().ActiveStreams; got != 3 {
		t.Fatalf("StreamStats().ActiveStreams = %d, want 3", got)
	}

	// 关 1 条：Active 递减、MaxActive 保持 3。
	if err := streams[0].Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Close 经 writeCh 异步，等 Active 回到 2（对端 Close 帧处理后才注销）。
	waitActive(t, m, 2)
	if got := m.Metrics().Streams.MaxActive.Load(); got != 3 {
		t.Fatalf("MaxActive 应保持峰值 3, got %d", got)
	}
	if got := m.StreamStats().ActiveStreams; got != 2 {
		t.Fatalf("StreamStats().ActiveStreams = %d, want 2", got)
	}

	// Abort 1 条：同步注销（removeStreamIf），Active 立即递减。
	if err := streams[1].Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := m.Metrics().Streams.Active.Load(); got != 1 {
		t.Fatalf("Abort 后 Active = %d, want 1", got)
	}

	// 再开 1 条：Active 回到 2，MaxActive 仍 3（不降）。
	if _, err := m.Open(t.Context()); err != nil {
		t.Fatalf("Open 4th: %v", err)
	}
	if got := m.Metrics().Streams.Active.Load(); got != 2 {
		t.Fatalf("再开后 Active = %d, want 2", got)
	}
	if got := m.Metrics().Streams.MaxActive.Load(); got != 3 {
		t.Fatalf("MaxActive 应保持 3, got %d", got)
	}
}

// TestStreamStats_LongestIdleGrowsWithoutActivity 钉住 LongestIdle 随空闲持续增长
// （acceptor 侧流不主动 Close ⇒ 对端失联时最久空闲时长持续增大，是「疑似泄漏流」哨兵）。
//
// 用测试可控时序：开一条流后不活动，轮询断言 LongestIdle 单调非降且最终 ≥ 某阈值
// （不依赖固定 sleep——用 WaitFor 轮询 + 直接构造「已空闲 N ms」的 lastActivity）。
func TestStreamStats_LongestIdleGrowsWithoutActivity(t *testing.T) {
	t.Parallel()

	pipeA, _ := xfertest.Pipe()
	m := New(pipeA, RoleDialer)
	defer m.Close()

	// 直接构造一条流的 lastActivity 为过去 500ms（模拟「已空闲 500ms 的流」）。
	s, err := m.Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	st := s.(*stream)
	st.lastActivity.Store(time.Now().Add(-500 * time.Millisecond).UnixNano())

	// 采样 LongestIdle 应 ≥ 500ms。
	if got := m.StreamStats().LongestIdle; got < 500*time.Millisecond {
		t.Fatalf("LongestIdle = %v, want >= 500ms", got)
	}
	if got := m.LongestIdle(); got < 500*time.Millisecond {
		t.Fatalf("Mux.LongestIdle() = %v, want >= 500ms", got)
	}

	// 再推进 lastActivity 到 1s 前：LongestIdle 应增大（单调非降）。
	st.lastActivity.Store(time.Now().Add(-1 * time.Second).UnixNano())
	if got := m.StreamStats().LongestIdle; got < 900*time.Millisecond {
		t.Fatalf("LongestIdle 未增大 = %v, want >= 900ms", got)
	}

	// 关掉后：无活跃流 ⇒ LongestIdle 为 0。
	if err := s.Abort(); err != nil {
		t.Fatalf("Abort: %v", err)
	}
	if got := m.StreamStats().LongestIdle; got != 0 {
		t.Fatalf("无活跃流时 LongestIdle = %v, want 0", got)
	}
}

// waitActive 轮询等待 Active 到期望值（Close 异步注销；用 testutil.WaitForBool，不用固定 sleep）。
func waitActive(t *testing.T, m *Mux, want int64) {
	t.Helper()
	ok := testutil.WaitForBool(5*time.Second, func() bool {
		return m.Metrics().Streams.Active.Load() == want
	})
	if !ok {
		t.Fatalf("Active 未在 5s 内到达 %d, 当前 %d", want, m.Metrics().Streams.Active.Load())
	}
}
