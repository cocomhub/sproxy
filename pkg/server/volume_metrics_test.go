// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// volume_metrics_test.go 钉住「卷健康指标」（roadmap §3 P1）：每卷读写延迟/失败率入
// /metrics（带 volume+op 标签）——面板可见每卷健康：
//   - sproxy_volume_io_total{volume,op}：该卷该操作（upload/download）的请求总数；
//   - sproxy_volume_io_failures_total{volume,op}：失败数（失败率 = 失败/总数）；
//   - sproxy_volume_io_latency_nanos_total{volume,op}：累计延迟（纳秒）。
//
// 断言作用在 Prometheus 文本输出上（用户可见契约）。

import (
	"strings"
	"testing"
	"time"
)

func TestVolumeMetrics_RecordVolumeIO(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)

	h.metrics.RecordVolumeIO("main", "upload", 5*time.Millisecond, true)
	h.metrics.RecordVolumeIO("main", "upload", 3*time.Millisecond, true)
	h.metrics.RecordVolumeIO("main", "upload", 12*time.Millisecond, false) // 失败一次
	h.metrics.RecordVolumeIO("disk2", "download", 8*time.Millisecond, true)

	body := metricsBody(t, ts)
	if !strings.Contains(body, "# TYPE sproxy_volume_io_total counter") {
		t.Fatalf("缺少 TYPE 行:\n%s", body)
	}
	// 总数：main/upload = 3（2 成功 + 1 失败），disk2/download = 1。
	wantTotal := `sproxy_volume_io_total{volume="main",op="upload"} 3`
	if !strings.Contains(body, wantTotal) {
		t.Errorf("缺少 %q:\n%s", wantTotal, body)
	}
	wantDl := `sproxy_volume_io_total{volume="disk2",op="download"} 1`
	if !strings.Contains(body, wantDl) {
		t.Errorf("缺少 %q:\n%s", wantDl, body)
	}
	// 失败数：main/upload 失败 1。
	wantFail := `sproxy_volume_io_failures_total{volume="main",op="upload"} 1`
	if !strings.Contains(body, wantFail) {
		t.Errorf("缺少 %q:\n%s", wantFail, body)
	}
	// 累计延迟：5+3+12 = 20ms = 20_000_000 ns；disk2/download = 8ms。
	wantLat := `sproxy_volume_io_latency_nanos_total{volume="main",op="upload"} 20000000`
	if !strings.Contains(body, wantLat) {
		t.Errorf("缺少 %q:\n%s", wantLat, body)
	}
}
