// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件钉住 readLoop 阻塞观测（含 Pong/数据报 handler 侧计数）真的出现在 /metrics 上，
// 并钉住**跨 mux 聚合语义**：次数/耗时求和、单次峰值取最大、dataCh 水位取最大（不是求和）。
//
// 动机：这些指标是「readLoop 头阻塞」治理取舍的证据基线（审计 F2），若只在 mux 内部有值
// 而不出现在运维面，等于没有可观测性；聚合口径写错（例如把「峰值」当计数器求和）会把
// 单条连接的瞬时尖峰放大成看起来的系统性问题。

// newMuxForMetrics 建立一个挂在路由表上的 mux，返回它与注册用的 routeTable。
func newMuxForMetrics(t *testing.T, rt *hub.MeshRouteTable, id string) *mux.Mux {
	t.Helper()
	pipeA, _ := xfertest.Pipe()
	m := mux.New(pipeA, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.AddNode("", hub.NodeID(id), m)
	return m
}

// TestMetricsHandler_ReadLoopObservability：三条 readLoop 阻塞路径、Pong 计数与 dataCh 水位
// 必须出现在 /metrics 文本里，且跨 mux 按各自语义聚合。
func TestMetricsHandler_ReadLoopObservability(t *testing.T) {
	t.Parallel()
	rt := hub.NewMeshRouteTable()
	m1 := newMuxForMetrics(t, rt, "n1")
	m2 := newMuxForMetrics(t, rt, "n2")

	// m1：push 阻塞 1 次（含耗时）、Pong 出线 2 次、被合并 1 次、handler 丢弃 3 次、水位 5 帧。
	// （直接写原子字段：enter/leave 是 mux 包内记账细节，跨包断言聚合语义只需给定观测值）
	m1.Metrics().ReadLoopPush.Waits.Store(1)
	m1.Metrics().ReadLoopPush.Nanos.Store(5_000_000)
	m1.Metrics().ReadLoopPush.MaxNanos.Store(5_000_000)
	m1.Metrics().PongsSent.Add(2)
	m1.Metrics().PongsCoalesced.Add(1)
	m1.Metrics().PongsDropped.Add(4)
	m1.Metrics().DatagramHandlerDrops.Add(3)
	m1.Metrics().DataChMaxFrames.Store(5)
	m1.Metrics().StreamOverflowSpills.Add(6)
	m1.Metrics().StreamWindowViolations.Add(1)
	m1.Metrics().MaxBufferedBytes.Store(70_000)

	// m2：push 阻塞 2 次（无耗时）、Pong 出线 1 次、水位 9 帧（应取最大 ⇒ 输出 9，而非 5+9）。
	m2.Metrics().ReadLoopPush.Waits.Store(2)
	m2.Metrics().PongsSent.Add(1)
	m2.Metrics().DataChMaxFrames.Store(9)
	m2.Metrics().StreamOverflowSpills.Add(4)
	m2.Metrics().StreamWindowViolations.Add(2)
	m2.Metrics().MaxBufferedBytes.Store(131_070)

	h := &Handlers{metrics: NewMetrics(), routeTable: rt, logger: testutil.DiscardLogger()}
	w := httptest.NewRecorder()
	h.MetricsHandler(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	want := []string{
		"\nsproxy_mux_pongs_sent 3\n",
		"\nsproxy_mux_pongs_coalesced 1\n",
		"\nsproxy_mux_pongs_dropped 4\n",
		"\nsproxy_mux_datagram_handler_drops 3\n",
		"\nsproxy_mux_readloop_push_waits 3\n",
		"\nsproxy_mux_readloop_push_nanos_total 5000000\n",
		"\nsproxy_mux_readloop_push_nanos_max 5000000\n",
		"\nsproxy_mux_readloop_datagram_waits 0\n",
		"\nsproxy_mux_readloop_pong_waits 0\n",
		"\nsproxy_mux_stream_datach_max_frames 9\n",
		"\nsproxy_mux_stream_overflow_spills 10\n",
		"\nsproxy_mux_stream_window_violations 3\n",
		"\nsproxy_mux_stream_buffered_max_bytes 131070\n",
	}
	for _, line := range want {
		if !strings.Contains(body, line) {
			t.Errorf("/metrics 缺少 %q", strings.TrimSpace(line))
		}
	}
	// 峰值类不得求和：水位应为 max(5,9)=9（上面的断言已覆盖 9），且不得出现 14。
	if strings.Contains(body, "\nsproxy_mux_stream_datach_max_frames 14\n") {
		t.Error("dataCh 水位是峰值类指标，跨 mux 必须取最大而非求和")
	}
	// 「已收未消费字节峰值」同理：应为 max(70000,131070)=131070，不得出现 201070。
	if strings.Contains(body, "\nsproxy_mux_stream_buffered_max_bytes 201070\n") {
		t.Error("已收未消费字节峰值是峰值类指标，跨 mux 必须取最大而非求和")
	}
}

// TestMetricsHandler_NoMuxMetricsSection：无 mux 实例时不得渲染该段（保持既有行为）。
func TestMetricsHandler_NoMuxMetricsSection(t *testing.T) {
	t.Parallel()
	h := &Handlers{metrics: NewMetrics(), logger: testutil.DiscardLogger()}
	w := httptest.NewRecorder()
	h.MetricsHandler(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(w.Body.String(), "sproxy_mux_") {
		t.Error("无 mux 实例时不应渲染 mux 指标段")
	}
}
