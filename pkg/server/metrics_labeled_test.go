// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// metrics_labeled_test.go 钉住 W4 新增的**带标签**指标（跨节点运维面）：
//   - sproxy_mesh_dial_total{carrier,node,service}：成功建链的实际载体（直连/中继）；
//   - sproxy_mesh_dial_fallback_total{node,service}：**打洞失败后回落中继**的次数；
//   - sproxy_remote_write_denied_total{reason,node}：写面**授权**拒绝（不含配额/校验类失败）。
//
// 断言直接作用在 Prometheus 文本输出上（用户可见契约），而不是内部 map —— 后者对了但渲染错
// 等于没指标。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// metricsBody 取一次 /metrics 文本。
func metricsBody(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(ts.URL + "/metrics")
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

func TestMetricsLabeled_MeshDial(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)

	h.metrics.RecordMeshDial("webrtc", "node-a", "volread", false)
	h.metrics.RecordMeshDial("webrtc", "node-a", "volread", false)
	h.metrics.RecordMeshDial("relay", "node-b", "volwrite", true) // 打洞失败后回落

	body := metricsBody(t, ts)
	if !strings.Contains(body, "# TYPE sproxy_mesh_dial_total counter") {
		t.Fatalf("缺少 TYPE 行:\n%s", body)
	}
	want := `sproxy_mesh_dial_total{carrier="webrtc",node="node-a",service="volread"} 2`
	if !strings.Contains(body, want) {
		t.Errorf("缺少带标签计数行 %q:\n%s", want, body)
	}
	wantRelay := `sproxy_mesh_dial_total{carrier="relay",node="node-b",service="volwrite"} 1`
	if !strings.Contains(body, wantRelay) {
		t.Errorf("缺少 %q:\n%s", wantRelay, body)
	}
	// 回落只对回落的那次计数（直连成功不计）。
	fb := `sproxy_mesh_dial_fallback_total{node="node-b",service="volwrite"} 1`
	if !strings.Contains(body, fb) {
		t.Errorf("缺少回落计数 %q:\n%s", fb, body)
	}
	if strings.Contains(body, `sproxy_mesh_dial_fallback_total{node="node-a"`) {
		t.Errorf("直连成功不应计入回落:\n%s", body)
	}
}

func TestMetricsLabeled_RemoteWriteDenied(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)

	h.metrics.RecordRemoteWriteDenied("scope", "node-a")
	h.metrics.RecordRemoteWriteDenied("scope", "node-a")
	h.metrics.RecordRemoteWriteDenied("not_pinned", "node-b")

	body := metricsBody(t, ts)
	for _, want := range []string{
		`# TYPE sproxy_remote_write_denied_total counter`,
		`sproxy_remote_write_denied_total{node="node-a",reason="scope"} 2`,
		`sproxy_remote_write_denied_total{node="node-b",reason="not_pinned"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("缺少 %q:\n%s", want, body)
		}
	}
}

// TestMetricsLabeled_Escaping 钉住标签值转义（Prometheus 文本格式要求对 `\`、`"`、换行转义），
// 否则一个含引号的节点名就能产出畸形文本、让整个抓取失败。
func TestMetricsLabeled_Escaping(t *testing.T) {
	ts, h := newTestServerWithMetrics(t)

	// 输入：node 含一个双引号，service 含一个反斜杠。
	// 期望渲染：双引号变 `\"`，单个反斜杠变 `\\`。
	h.metrics.RecordMeshDial("relay", `node"x`, `svc\y`, false)

	body := metricsBody(t, ts)
	want := `sproxy_mesh_dial_total{carrier="relay",node="node\"x",service="svc\\y"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("标签值未按 Prometheus 文本格式转义，期望 %q:\n%s", want, body)
	}
}

// TestMetricsLabeled_NilSafe 钉住无 metrics 的 Handlers 上调用记录方法不 panic（测试/旧装配路径）。
func TestMetricsLabeled_NilSafe(t *testing.T) {
	h := &Handlers{}
	h.RecordMeshDial("webrtc", "n", "s", false)
	h.RecordRemoteWriteDenied("scope", "n")
}
