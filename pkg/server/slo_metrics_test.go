// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"math"
	"math/rand"
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

func TestDurationHistogram_ObserveBucketBoundary(t *testing.T) {
	t.Parallel()

	th := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond}
	h := newDurationHistogram(th)
	h.observe(5 * time.Millisecond)                 // 恰在边界（<= 归首桶）
	h.observe(5*time.Millisecond + time.Nanosecond) // 越首桶 → 第二桶
	h.observe(0)                                    // 非正防御性落首桶
	h.observe(-time.Nanosecond)                     // 负时长落首桶

	buckets, _, count := h.snapshot()
	want := []int64{3, 4, 4, 4} // Prometheus 累计：首桶 3（含非正/负），第二桶起累计 4，+Inf 4
	if len(buckets) != len(want) {
		t.Fatalf("buckets 长度 = %d, want %d", len(buckets), len(want))
	}
	for i := range want {
		if buckets[i] != want[i] {
			t.Errorf("buckets[%d] = %d, want %d", i, buckets[i], want[i])
		}
	}
	if count != 4 {
		t.Errorf("count = %d, want 4", count)
	}
}

func TestDurationHistogram_CumulativeInvariant(t *testing.T) {
	t.Parallel()

	th := []time.Duration{
		5 * time.Millisecond, 10 * time.Millisecond, 25 * time.Millisecond,
		50 * time.Millisecond, 100 * time.Millisecond, 250 * time.Millisecond,
		500 * time.Millisecond, time.Second,
	}
	h := newDurationHistogram(th)
	rng := rand.New(rand.NewSource(42))
	const n = 1000
	var sum int64
	for range n {
		d := time.Duration(rng.Int63n(int64(2 * time.Second)))
		h.observe(d)
		sum += int64(d)
	}

	buckets, sumNanos, count := h.snapshot()
	for i := 1; i < len(buckets); i++ {
		if buckets[i] < buckets[i-1] {
			t.Errorf("累计不变量破坏：buckets[%d]=%d < buckets[%d]=%d", i, buckets[i], i-1, buckets[i-1])
		}
	}
	if buckets[len(buckets)-1] != count {
		t.Errorf("+Inf 桶 = %d, want count %d", buckets[len(buckets)-1], count)
	}
	if sumNanos != sum {
		t.Errorf("sum = %d, want %d", sumNanos, sum)
	}
	if count != n {
		t.Errorf("count = %d, want %d", count, n)
	}
}

func TestDurationHistogram_PlusInfCatchesAll(t *testing.T) {
	t.Parallel()

	h := newDurationHistogram([]time.Duration{5 * time.Millisecond, time.Second})
	h.observe(10 * time.Second) // 超最大桶（1s）→ +Inf 桶
	h.observe(time.Hour)        // 超长时长同样落 +Inf，不丢计数

	buckets, _, count := h.snapshot()
	if got := buckets[len(buckets)-1]; got != 2 {
		t.Errorf("+Inf 桶 = %d, want 2", got)
	}
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}
}

func TestApdex_Boundary(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	m.SetApdexThreshold(500 * time.Millisecond)
	m.RecordRequestLatency(500 * time.Millisecond)          // d == T → satisfied
	m.RecordRequestLatency(2 * time.Second)                 // d == 4T → tolerated
	m.RecordRequestLatency(2*time.Second + time.Nanosecond) // d > 4T → frustrated

	if got := m.apdexSatisfied.Load(); got != 1 {
		t.Errorf("satisfied = %d, want 1", got)
	}
	if got := m.apdexTolerated.Load(); got != 1 {
		t.Errorf("tolerated = %d, want 1", got)
	}
	if got := m.apdexFrustrated.Load(); got != 1 {
		t.Errorf("frustrated = %d, want 1", got)
	}
}

func TestApdex_ScoreAndZeroTotal(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	if got := m.apdexScore(); got != 0 {
		t.Errorf("total=0 时 score = %v, want 0（不 NaN）", got)
	}
	if got := m.apdexScore(); math.IsNaN(got) {
		t.Error("total=0 时 score 不得为 NaN")
	}

	m.apdexSatisfied.Add(3)
	m.apdexTolerated.Add(2)
	m.apdexFrustrated.Add(1)
	want := (3.0 + 2.0/2) / 6.0 // (sat + tol/2) / total = 4/6
	if got := m.apdexScore(); math.Abs(got-want) > 1e-9 {
		t.Errorf("score = %v, want %v", got, want)
	}
}

func TestWriteHistogram_TextFormat(t *testing.T) {
	t.Parallel()

	th := []time.Duration{5 * time.Millisecond, 10 * time.Millisecond}
	buckets := []int64{1, 3, 5} // 累计计数；末位 +Inf = 5
	var b strings.Builder
	writeHistogram(&b, "sproxy_request_duration_seconds", "HTTP request duration histogram",
		th, buckets, 42_000_000, 5)

	want := "" +
		"# HELP sproxy_request_duration_seconds HTTP request duration histogram\n" +
		"# TYPE sproxy_request_duration_seconds histogram\n" +
		"sproxy_request_duration_seconds_bucket{le=\"0.005\"} 1\n" +
		"sproxy_request_duration_seconds_bucket{le=\"0.01\"} 3\n" +
		"sproxy_request_duration_seconds_bucket{le=\"+Inf\"} 5\n" +
		"sproxy_request_duration_seconds_sum 0.042\n" +
		"sproxy_request_duration_seconds_count 5\n\n"
	if got := b.String(); got != want {
		t.Errorf("writeHistogram 输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

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
		_, _, cnt := h.metrics.requestDuration.snapshot()
		return cnt == 1
	}, "metricsMiddleware 应记录一次请求时长")

	if got := h.metrics.apdexSatisfied.Load() + h.metrics.apdexTolerated.Load() + h.metrics.apdexFrustrated.Load(); got != 1 {
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
