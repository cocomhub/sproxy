// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

// metrics_retransmit_test.go 验证 mux 重传质量指标在 /metrics 暴露
// （roadmap §6 P1：传输质量指标——重传次数/队列满/耗尽可观测）。

// TestMetrics_MuxRetransmitExposed 注入带重传计数的 mux 到 routeTable，
// 验证 /metrics 文本包含三个重传指标（Prometheus 格式）。
func TestMetrics_MuxRetransmitExposed(t *testing.T) {
	t.Parallel()

	conn := &mockxfer.MockConn{
		ReceiveFn: func(ctx context.Context) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	m := mux.New(conn, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	m.Metrics().Retransmits.Add(3)
	m.Metrics().RetransmitQueueFull.Add(1)
	m.Metrics().RetransmitExhausted.Add(2)

	rt := hub.NewMeshRouteTable()
	rt.Add("", hub.NodeInfo{ID: hub.NodeID("node-a"), Addr: "127.0.0.1:1", Mux: m}, nil)

	h := &Handlers{routeTable: rt, logger: testutil.DiscardLogger(), metrics: NewMetrics()}
	w := httptest.NewRecorder()
	h.MetricsHandler(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body, _ := io.ReadAll(w.Result().Body)
	text := string(body)
	for _, want := range []string{
		"sproxy_mux_retransmits_total 3",
		"sproxy_mux_retransmit_queue_full_total 1",
		"sproxy_mux_retransmit_exhausted_total 2",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("缺少指标 %q（/metrics 输出）：\n%s", want, text)
		}
	}
}

// TestMetrics_MuxRetransmitNilRouteTable 空 routeTable 时聚合 nil 不 panic（安全路径）。
func TestMetrics_MuxRetransmitNilRouteTable(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testutil.DiscardLogger()}
	w := httptest.NewRecorder()
	h.MetricsHandler(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d want 200", w.Code)
	}
}
