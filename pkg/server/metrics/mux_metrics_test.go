// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

func TestWriteHubNodeLinkGauge_NoMux(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteHubNodeLinkGauge(&b, hub.NodeInfo{}, "node-a")
	out := b.String()

	assertContainsAll(t, out,
		"# TYPE sproxy_hub_node_retransmits gauge\n",
		`sproxy_hub_node_retransmits{node="node-a"} 0`,
		`sproxy_hub_node_errors{node="node-a"} 0`,
		`sproxy_hub_node_connected_seconds{node="node-a"} 0`,
		`sproxy_hub_node_rtt_ms{node="node-a"} 0`,
	)
}

func TestWriteHubNodeLinkGauge_MuxCounters(t *testing.T) {
	t.Parallel()

	mx := &mux.Mux{}
	mm := mx.Metrics()
	mm.Retransmits.Store(3)
	mm.Errors.Store(2)
	mm.LastRTTNanos.Store(int64(1500 * time.Microsecond)) // 1.5ms → 截断为 1ms

	var b strings.Builder
	WriteHubNodeLinkGauge(&b, hub.NodeInfo{
		Mux:       mx,
		Connected: time.Now().Add(-2 * time.Hour),
	}, "node-a")
	out := b.String()

	if got := metricValue(t, out, `sproxy_hub_node_retransmits{node="node-a"}`); got != 3 {
		t.Errorf("retransmits = %d, want 3", got)
	}
	if got := metricValue(t, out, `sproxy_hub_node_errors{node="node-a"}`); got != 2 {
		t.Errorf("errors = %d, want 2", got)
	}
	if got := metricValue(t, out, `sproxy_hub_node_rtt_ms{node="node-a"}`); got != 1 {
		t.Errorf("rtt_ms = %d, want 1（纳秒→毫秒截断）", got)
	}
	// 连接时长依赖运行时刻：断言落在 2 小时的 5 秒窗内，避免时钟抖动导致 flake。
	if got := metricValue(t, out, `sproxy_hub_node_connected_seconds{node="node-a"}`); got < 7200 || got > 7205 {
		t.Errorf("connected_seconds = %d, want 7200..7205", got)
	}
}

func TestWriteHubNodeLinkGauge_NoRTTSample(t *testing.T) {
	t.Parallel()

	mx := &mux.Mux{}
	mx.Metrics().LastRTTNanos.Store(0) // 尚无 Ping→Pong 采样

	var b strings.Builder
	WriteHubNodeLinkGauge(&b, hub.NodeInfo{Mux: mx}, "node-a")
	if got := metricValue(t, b.String(), `sproxy_hub_node_rtt_ms{node="node-a"}`); got != -1 {
		t.Errorf("rtt_ms = %d, want -1（无采样）", got)
	}
}

func TestAccumulateNodeMuxMetrics_SumsAndMax(t *testing.T) {
	t.Parallel()

	nodeA := &mux.Mux{}
	a := nodeA.Metrics()
	a.Streams.Opened.Store(2)
	a.Streams.BytesRead.Store(10)
	a.Streams.BytesWritten.Store(20)
	a.Streams.Active.Store(1)
	a.Streams.MaxActive.Store(3)
	a.Streams.Errors.Store(1)
	a.FramesSent.Store(5)
	a.FramesReceived.Store(6)
	a.PingsSent.Store(1)
	a.PongsReceived.Store(2)
	a.PongsSent.Store(3)
	a.PongsCoalesced.Store(4)
	a.PongsDropped.Store(1)
	a.DatagramHandlerDrops.Store(1)
	a.StreamOverflowSpills.Store(2)
	a.StreamWindowViolations.Store(1)
	a.Retransmits.Store(2)
	a.RetransmitQueueFull.Store(1)
	a.RetransmitExhausted.Store(1)
	a.ReadLoopPush.Waits.Store(2)
	a.ReadLoopPush.Nanos.Store(10)
	a.ReadLoopPush.MaxNanos.Store(7)
	a.DataChMaxFrames.Store(4)
	a.MaxBufferedBytes.Store(100)
	a.Errors.Store(2)

	nodeB := &mux.Mux{}
	bm := nodeB.Metrics()
	bm.Streams.Opened.Store(3)
	bm.Streams.BytesRead.Store(1)
	bm.Streams.BytesWritten.Store(2)
	bm.Streams.Active.Store(2)
	bm.Streams.MaxActive.Store(5) // 峰值取最大：不是求和（1+2 也不等于 5）
	bm.Streams.Errors.Store(4)
	bm.Errors.Store(1)
	bm.ReadLoopPush.Waits.Store(1)
	bm.ReadLoopPush.Nanos.Store(5)
	bm.ReadLoopPush.MaxNanos.Store(3)
	bm.DataChMaxFrames.Store(2)
	bm.MaxBufferedBytes.Store(50)

	total := &mux.Metrics{}
	AccumulateNodeMuxMetrics(total, hub.NodeInfo{Mux: nodeA})
	AccumulateNodeMuxMetrics(total, hub.NodeInfo{Mux: nodeB})

	checks := []struct {
		name string
		got  int64
		want int64
	}{
		{"Streams.Opened", total.Streams.Opened.Load(), 5},
		{"Streams.BytesRead", total.Streams.BytesRead.Load(), 11},
		{"Streams.BytesWritten", total.Streams.BytesWritten.Load(), 22},
		{"Streams.Active", total.Streams.Active.Load(), 3},
		{"Streams.MaxActive", total.Streams.MaxActive.Load(), 5},
		{"Streams.Errors", total.Streams.Errors.Load(), 5},
		{"FramesSent", total.FramesSent.Load(), 5},
		{"FramesReceived", total.FramesReceived.Load(), 6},
		{"PingsSent", total.PingsSent.Load(), 1},
		{"PongsReceived", total.PongsReceived.Load(), 2},
		{"PongsSent", total.PongsSent.Load(), 3},
		{"PongsCoalesced", total.PongsCoalesced.Load(), 4},
		{"PongsDropped", total.PongsDropped.Load(), 1},
		{"DatagramHandlerDrops", total.DatagramHandlerDrops.Load(), 1},
		{"StreamOverflowSpills", total.StreamOverflowSpills.Load(), 2},
		{"StreamWindowViolations", total.StreamWindowViolations.Load(), 1},
		{"Retransmits", total.Retransmits.Load(), 2},
		{"RetransmitQueueFull", total.RetransmitQueueFull.Load(), 1},
		{"RetransmitExhausted", total.RetransmitExhausted.Load(), 1},
		{"ReadLoopPush.Waits", total.ReadLoopPush.Waits.Load(), 3},
		{"ReadLoopPush.Nanos", total.ReadLoopPush.Nanos.Load(), 15},
		{"ReadLoopPush.MaxNanos", total.ReadLoopPush.MaxNanos.Load(), 7},
		{"DataChMaxFrames", total.DataChMaxFrames.Load(), 4},
		{"MaxBufferedBytes", total.MaxBufferedBytes.Load(), 100},
		{"Errors", total.Errors.Load(), 3},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestWriteReadLoopBlock_ExactFormat(t *testing.T) {
	t.Parallel()

	stat := &mux.BlockStat{}
	stat.Waits.Store(3)
	stat.Nanos.Store(1200)
	stat.MaxNanos.Store(800)

	var b strings.Builder
	WriteReadLoopBlock(&b, "push", "push data", stat)

	const want = "" +
		"# HELP sproxy_mux_readloop_push_waits Times readLoop entered push data\n" +
		"# TYPE sproxy_mux_readloop_push_waits counter\n" +
		"sproxy_mux_readloop_push_waits 3\n" +
		"\n" +
		"# HELP sproxy_mux_readloop_push_nanos_total Total nanoseconds readLoop spent in push data\n" +
		"# TYPE sproxy_mux_readloop_push_nanos_total counter\n" +
		"sproxy_mux_readloop_push_nanos_total 1200\n" +
		"\n" +
		"# HELP sproxy_mux_readloop_push_nanos_max Max nanoseconds readLoop spent in a single push data\n" +
		"# TYPE sproxy_mux_readloop_push_nanos_max gauge\n" +
		"sproxy_mux_readloop_push_nanos_max 800\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteGCIDMetrics_BuiltinDownloaderHasNoStats(t *testing.T) {
	t.Parallel()

	// 默认注册表的内置 HTTP 下载器未实现 GCIDStats ⇒ 不输出任何序列。
	var b strings.Builder
	WriteGCIDMetrics(&b)
	if got := b.String(); got != "" {
		t.Errorf("内置下载器不应输出 GCID 指标，got:\n%s", got)
	}
}

func TestWriteHybridMetrics_BuiltinDownloaderHasNoCounters(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteHybridMetrics(&b)
	if got := b.String(); got != "" {
		t.Errorf("内置下载器不应输出 hybrid 指标，got:\n%s", got)
	}
}
