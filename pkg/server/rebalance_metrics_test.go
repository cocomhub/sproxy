// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// rebalance_metrics_test.go 钉住「卷再平衡迁移进度」指标（roadmap 3.3 P1 残余，#440 后补）：
//   - rebalanceProgress 状态结构：begin/add/end/Percent 语义（互斥保护、按 from/to 维度、
//     total<=0 时 percent=0 保守、完成清除）；
//   - /metrics 输出 sproxy_rebalance_progress{from_volume,to_volume} gauge（0-100 百分比，
//     进行中任务才存在；完成后清零 = 序列消失）；
//   - rebalance handler 端到端：发起 rebalance → 完成后 /metrics 无进度序列（清零），
//     且 VolumeIO 不受影响。
//
// 断言作用在 Prometheus 文本输出上（用户可见契约）。

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// fetchMetricsText 拉取指定 URL 的 /metrics 文本（twoVolumeServer 返回 string URL，
// 不能直接用 metricsBody——后者要 *httptest.Server）。
func fetchMetricsText(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	buf := new(strings.Builder)
	if _, err := io.Copy(buf, resp.Body); err != nil {
		t.Fatalf("读响应: %v", err)
	}
	return buf.String()
}

// TestRebalanceProgress_BeginAddEnd 状态结构语义：begin 登记 → add 累加 → Percent 正确 → end 清除。
func TestRebalanceProgress_BeginAddEnd(t *testing.T) {
	t.Parallel()
	p := newRebalanceProgress()
	p.begin("main", "disk2", 100) // 总字节 100
	p.add("main", "disk2", 40)
	p.add("main", "disk2", 20) // 累计 60

	s := p.samples()
	if len(s) != 1 {
		t.Fatalf("进度样本应 1 条, got %d: %+v", len(s), s)
	}
	if !strings.Contains(s[0].labels, `from_volume="main"`) || !strings.Contains(s[0].labels, `to_volume="disk2"`) {
		t.Fatalf("标签应含 from/to: %s", s[0].labels)
	}
	if s[0].value != 60 {
		t.Fatalf("percent=%d want 60（60/100）", s[0].value)
	}

	p.end("main", "disk2")
	if got := p.samples(); len(got) != 0 {
		t.Fatalf("end 后应无进度样本, got %+v", got)
	}
}

// TestRebalanceProgress_TotalUnknown 总字节 0（无限配额/未知）→ percent 保守 0。
func TestRebalanceProgress_TotalUnknown(t *testing.T) {
	t.Parallel()
	p := newRebalanceProgress()
	p.begin("main", "disk2", 0)
	p.add("main", "disk2", 50)
	s := p.samples()
	if len(s) != 1 {
		t.Fatalf("进度样本应 1 条, got %d", len(s))
	}
	if s[0].value != 0 {
		t.Fatalf("total=0 时 percent 应 0（不可算保守）, got %d", s[0].value)
	}
}

// TestRebalanceProgress_MultiPair 多任务按 (from,to) 独立分组。
func TestRebalanceProgress_MultiPair(t *testing.T) {
	t.Parallel()
	p := newRebalanceProgress()
	p.begin("main", "disk2", 100)
	p.begin("a", "b", 10)
	p.add("main", "disk2", 50)
	p.add("a", "b", 10)
	s := p.samples()
	if len(s) != 2 {
		t.Fatalf("两个任务应 2 条样本, got %d: %+v", len(s), s)
	}
	m := map[string]int64{}
	for _, x := range s {
		if strings.Contains(x.labels, `from_volume="main"`) {
			m["main"] = x.value
		}
		if strings.Contains(x.labels, `from_volume="a"`) {
			m["a"] = x.value
		}
	}
	if m["main"] != 50 || m["a"] != 100 {
		t.Fatalf("percent 分组错误: %+v", m)
	}
}

// TestRebalanceMetrics_HandlerClearsAfterDone 端到端：rebalance 完成后 /metrics 无进度序列
// （清零可观测）；VolumeIO 指标正常输出不受影响。
func TestRebalanceMetrics_HandlerClearsAfterDone(t *testing.T) {
	t.Parallel()
	url, _, dirs := twoVolumeServer(t)

	// 上传一个文件到 main，然后 rebalance → 完成后进度应清零。
	status, _, respBody := volumeUpload(t, url, "prog.txt", []byte("hello"), "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, respBody)
	}
	_ = dirs
	status, bodyResp := rebalanceVolume(t, url, "main", "disk2", "")
	if status != http.StatusOK {
		t.Fatalf("rebalance status=%d want 200, body=%s", status, bodyResp)
	}
	_ = decodeRebalance(t, bodyResp)

	// /metrics：rebalance 完成后不应有 sproxy_rebalance_progress 序列（gauge 无样本不输出）。
	// twoVolumeServer 返回 string URL（非 *httptest.Server）——用独立 GET 拉取。
	body := fetchMetricsText(t, url)
	if strings.Contains(body, "sproxy_rebalance_progress") {
		t.Fatalf("rebalance 完成后 /metrics 不应含进度序列:\n%s", body)
	}
	// VolumeIO 正常输出（rebalance handler 经 upload 路径记录）。
	if !strings.Contains(body, "sproxy_volume_io_total") {
		t.Fatalf("VolumeIO 应正常输出:\n%s", body)
	}
}

// TestRebalanceMetrics_ManualGauge 手动构造进度 → /metrics 输出 sproxy_rebalance_progress gauge
// 文本（含 HELP/TYPE + 样本行 + 百分比）。
func TestRebalanceMetrics_ManualGauge(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)
	h.rebalanceProg = newRebalanceProgress()
	h.rebalanceProg.begin("main", "disk2", 200)
	h.rebalanceProg.add("main", "disk2", 80)

	body := metricsBody(t, ts)
	want := `sproxy_rebalance_progress{from_volume="main",to_volume="disk2"} 40`
	if !strings.Contains(body, want) {
		t.Fatalf("缺少 %q:\n%s", want, body)
	}
	if !strings.Contains(body, "# TYPE sproxy_rebalance_progress gauge") {
		t.Fatalf("缺少 TYPE 行:\n%s", body)
	}
}
