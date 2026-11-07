// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// metricValue 从渲染文本中取指定序列（`<series> <value>` 行）的整数值。
// 用于对「值依赖运行时」的 gauge（如连接时长）做区间断言。
func metricValue(t *testing.T, out, series string) int64 {
	t.Helper()
	prefix := series + " "
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimPrefix(line, prefix), 10, 64)
		if err != nil {
			t.Fatalf("序列 %s 的值无法解析: %v（行=%q）", series, err, line)
		}
		return v
	}
	t.Fatalf("输出缺少序列 %s:\n%s", series, out)
	return 0
}

// assertContainsAll 断言输出包含全部片段（片段缺失即报告，便于定位渲染漂移）。
func assertContainsAll(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("输出缺少片段 %q:\n%s", w, out)
		}
	}
}

func TestWriteLabeledCounter_EmptySamplesStillEmitsHelpAndType(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteLabeledCounter(&b, "sproxy_demo_total", "Demo counter", nil)

	const want = "" +
		"# HELP sproxy_demo_total Demo counter\n" +
		"# TYPE sproxy_demo_total counter\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteLabeledCounter_SortsSamples(t *testing.T) {
	t.Parallel()

	samples := []LabeledSample{
		{Labels: `status="5xx"`, Value: 3},
		{Labels: `status="2xx"`, Value: 9},
		{Labels: `status="4xx"`, Value: 1},
	}
	var b strings.Builder
	WriteLabeledCounter(&b, "sproxy_demo_total", "Demo counter", samples)

	const want = "" +
		"# HELP sproxy_demo_total Demo counter\n" +
		"# TYPE sproxy_demo_total counter\n" +
		`sproxy_demo_total{status="2xx"} 9` + "\n" +
		`sproxy_demo_total{status="4xx"} 1` + "\n" +
		`sproxy_demo_total{status="5xx"} 3` + "\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配（应按标签排序）:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteGaugeSamples_NoSamplesWritesNothing(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteGaugeSamples(&b, "sproxy_rebalance_percent", "Rebalance percent", nil)
	if got := b.String(); got != "" {
		t.Errorf("无样本时 gauge 不应输出 HELP/TYPE，got:\n%s", got)
	}
}

func TestWriteGaugeSamples_SortsAndFormats(t *testing.T) {
	t.Parallel()

	samples := []LabeledSample{
		{Labels: `from_volume="vol-b",to_volume="vol-c"`, Value: 0},
		{Labels: `from_volume="vol-a",to_volume="vol-c"`, Value: 42},
	}
	var b strings.Builder
	WriteGaugeSamples(&b, "sproxy_rebalance_percent", "Rebalance percent", samples)

	const want = "" +
		"# HELP sproxy_rebalance_percent Rebalance percent\n" +
		"# TYPE sproxy_rebalance_percent gauge\n" +
		`sproxy_rebalance_percent{from_volume="vol-a",to_volume="vol-c"} 42` + "\n" +
		`sproxy_rebalance_percent{from_volume="vol-b",to_volume="vol-c"} 0` + "\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteMetric_ExactFormat(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteMetric(&b, "sproxy_demo_waits", "counter", "Demo waits", 5)

	const want = "" +
		"# HELP sproxy_demo_waits Demo waits\n" +
		"# TYPE sproxy_demo_waits counter\n" +
		"sproxy_demo_waits 5\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteLabeledGauge_SortsLabels(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteLabeledGauge(&b, "sproxy_demo_gauge", "Demo gauge",
		map[string]string{"zone": "z1", "node": "node-a"}, 7)

	const want = "" +
		"# HELP sproxy_demo_gauge Demo gauge\n" +
		"# TYPE sproxy_demo_gauge gauge\n" +
		`sproxy_demo_gauge{node="node-a",zone="z1"} 7` + "\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配（标签应按名字排序）:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestWriteLabeledGauge_NoLabels(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteLabeledGauge(&b, "sproxy_demo_gauge", "Demo gauge", nil, 0)

	if !strings.Contains(b.String(), "sproxy_demo_gauge{} 0\n") {
		t.Errorf("无标签时应输出空标签集 `{}`:\n%s", b.String())
	}
}

func TestEscapeLabel(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		`plain`:        `plain`,
		`a\b`:          `a\\b`,
		`a"b`:          `a\"b`,
		"line1\nline2": `line1\nline2`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatSeconds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0"},
		{5 * time.Second, "5"},
		{1500 * time.Millisecond, "1.5"},
		{2500 * time.Millisecond, "2.5"},
		{42 * time.Millisecond, "0.042"},
		{time.Millisecond, "0.001"},
		{1500 * time.Microsecond, "0.0015"},
	}
	for _, c := range cases {
		if got := formatSeconds(c.in); got != c.want {
			t.Errorf("formatSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderSLOMetrics_HistogramAndApdex(t *testing.T) {
	t.Parallel()

	m := NewMetrics()
	// T = 500ms：10ms/100ms satisfied，1s/1.5s tolerated，3s/5s frustrated。
	for _, d := range []time.Duration{
		10 * time.Millisecond, 100 * time.Millisecond,
		time.Second, 1500 * time.Millisecond,
		3 * time.Second, 5 * time.Second,
	} {
		m.RecordRequestLatency(d)
	}

	var b strings.Builder
	m.RenderSLOMetrics(&b)
	out := b.String()

	assertContainsAll(t, out,
		"# HELP sproxy_request_duration_seconds HTTP request duration histogram\n",
		"# TYPE sproxy_request_duration_seconds histogram\n",
		// 累计桶：10ms→le 0.01 起；100ms→le 0.1 起；1s→le 1 起；1.5s→le 2.5 起；3s/5s→le 5 起。
		`sproxy_request_duration_seconds_bucket{le="0.005"} 0`,
		`sproxy_request_duration_seconds_bucket{le="0.01"} 1`,
		`sproxy_request_duration_seconds_bucket{le="0.1"} 2`,
		`sproxy_request_duration_seconds_bucket{le="1"} 3`,
		`sproxy_request_duration_seconds_bucket{le="2.5"} 4`,
		`sproxy_request_duration_seconds_bucket{le="5"} 6`,
		`sproxy_request_duration_seconds_bucket{le="+Inf"} 6`,
		"sproxy_request_duration_seconds_sum 10.61",
		"sproxy_request_duration_seconds_count 6",
		"# TYPE sproxy_apdex gauge\n",
		"sproxy_apdex 0.5\n",
		"sproxy_apdex_satisfied_total 2\n",
		"sproxy_apdex_tolerated_total 2\n",
		"sproxy_apdex_frustrated_total 2\n",
	)
}

func TestRenderSLOMetrics_SkipsWhenHistogramMissing(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	var nilMetrics *Metrics
	nilMetrics.RenderSLOMetrics(&b)
	if got := b.String(); got != "" {
		t.Errorf("nil 接收者不应输出，got:\n%s", got)
	}

	// 零值 Metrics（未装配直方图）同样不输出。
	zero := &Metrics{}
	zero.RenderSLOMetrics(&b)
	if got := b.String(); got != "" {
		t.Errorf("零值 Metrics 不应输出，got:\n%s", got)
	}
}

func TestWriteHistogram_NoThresholdsOnlyPlusInf(t *testing.T) {
	t.Parallel()

	var b strings.Builder
	WriteHistogram(&b, "sproxy_demo_seconds", "Demo", nil, []int64{4}, 5_000_000, 4)

	const want = "" +
		"# HELP sproxy_demo_seconds Demo\n" +
		"# TYPE sproxy_demo_seconds histogram\n" +
		`sproxy_demo_seconds_bucket{le="+Inf"} 4` + "\n" +
		"sproxy_demo_seconds_sum 0.005\n" +
		"sproxy_demo_seconds_count 4\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("输出不匹配:\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}
