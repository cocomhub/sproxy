// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"gopkg.in/yaml.v3"
)

// slo_metrics_test.go 是 SLO 指标（roadmap 11.10-H3）的测试：手写无锁桶直方图
// （durationHistogram）+ Apdex 三档 + /metrics 渲染 + metricsMiddleware 时长捕获。
// 全部用例 t.Parallel()（R18）；纯标准库断言；仅 127.0.0.1（httptest 默认回环）。

func TestMetricsMiddleware_RecordsLatency(t *testing.T) {
	t.Parallel()

	ts, h := newTestServerWithMetrics(t)
	resp, err := metricsHTTPClient().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	resp.Body.Close()

	// 条件轮询：middleware 在 handler 返回后同步记账，-race 下调度可能延迟（禁固定 sleep）。
	testutil.WaitFor(t, 2*time.Second, func() bool {
		cnt := h.metrics.RequestDurationCount()
		return cnt == 1
	}, "metricsMiddleware 应记录一次请求时长")

	if got := func() int64 { a, b, c := h.metrics.ApdexCounts(); return a + b + c }(); got != 1 {
		t.Errorf("apdex 三档合计 = %d, want 恰一档 +1", got)
	}
}

func TestMetricsHandler_AppendsSLOSection(t *testing.T) {
	t.Parallel()

	h := &Handlers{metrics: NewMetrics(), logger: testutil.DiscardLogger()}
	w := httptest.NewRecorder()
	h.MetricsHandler(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()

	// 无样本也输出 HELP/TYPE（可发现性约定）；SLO 段追加在尾部，既有行不变（零回归）。
	checks := []string{
		"# TYPE sproxy_request_duration_seconds histogram",
		"sproxy_request_duration_seconds_bucket{le=\"+Inf\"} 0",
		"sproxy_request_duration_seconds_count 0",
		"# TYPE sproxy_apdex gauge",
		"sproxy_apdex 0",
		"sproxy_apdex_satisfied_total 0",
		"sproxy_apdex_tolerated_total 0",
		"sproxy_apdex_frustrated_total 0",
		"sproxy_requests_total 0",
	}
	for _, c := range checks {
		if !strings.Contains(body, c) {
			t.Errorf("/metrics 缺少 %q", c)
		}
	}
	// SLO 段必须在既有 requests 行之后（追加尾部，不改既有行）。
	if strings.Index(body, "sproxy_apdex_satisfied_total") < strings.Index(body, "sproxy_requests_total") {
		t.Error("SLO 段应追加在既有指标之后")
	}
}

func TestConfig_ApdexThresholdDefault(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if cfg.ApdexThreshold != 500*time.Millisecond {
		t.Errorf("Default().ApdexThreshold = %v, want 500ms", cfg.ApdexThreshold)
	}

	// SetDefaults 兜底：零值 → 500ms。
	cfg2 := &Config{}
	cfg2.SetDefaults()
	if cfg2.ApdexThreshold != 500*time.Millisecond {
		t.Errorf("SetDefaults 后 ApdexThreshold = %v, want 500ms", cfg2.ApdexThreshold)
	}

	// YAML 显式配置生效。
	cfg3 := &Config{}
	if err := yaml.Unmarshal([]byte("apdex_threshold: 1s\n"), cfg3); err != nil {
		t.Fatalf("yaml 解析失败: %v", err)
	}
	cfg3.SetDefaults()
	if cfg3.ApdexThreshold != time.Second {
		t.Errorf("yaml apdex_threshold: 1s → %v, want 1s", cfg3.ApdexThreshold)
	}
}
