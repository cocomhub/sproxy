// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// usage_report_test.go 覆盖计量报告片 2（roadmap 11.10-⑩）：
//  1. Metrics owner 维度计数（RecordUploadForOwner/RecordDownloadForOwner）与
//     /metrics 的 sproxy_usage_*_bytes_total{owner=...} 渲染；
//  2. GET /api/usage/report 权限矩阵：owner 自查询 / 管理员全量 / 越权 403；
//  3. 导出格式：JSON 默认 + CSV（RFC 4180 表头/行）；聚合正确性（上传/下载字节）；
//  4. 错误处理：format 非法 400、from/to 非法/缺失 400、未启用 400、无数据 200 空数组。
//
// 变异点（改后必须红）：① handler 越权检查缺失 → 越权用例红；② from/to 校验缺失 →
// 非法范围用例红；③ format 校验缺失 → 非法格式用例红；④ 未启用检查缺失 → 400 用例红。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// newUsageReportServer 启动启用计量报告的完整路由服务器（admin+user+reader 三角色 Ring）。
func newUsageReportServer(t *testing.T) string {
	t.Helper()
	ring := ringWithReader(t)
	url, _, _ := newAuthSeamServer(t, func(c *Config) {
		c.Usage.Enabled = true
	}, func(opts *RegisterRoutesOpts) {
		opts.CredentialRing = ring
		opts.CredentialStore = nil
	})
	return url
}

// usageReportGet 带 SproxySig 签名 GET /api/usage/report（query 含前导 ? 或空）。
// 缺省范围：from/to 省略 = 全范围（设计：宽松语义，无数据 200 空数组）。
func usageReportGet(t *testing.T, url, query, ak, sk string) (int, []byte) {
	t.Helper()
	return doSignedJSON(t, http.MethodGet, url+"/api/usage/report"+query, ak, sk, nil)
}

// usageReportBody 是报告响应体（from/to/reports）。
type usageReportBody struct {
	From    string         `json:"from"`
	To      string         `json:"to"`
	Reports []usageSummary `json:"reports"`
}

func decodeUsageReport(t *testing.T, body []byte) usageReportBody {
	t.Helper()
	var out usageReportBody
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解析报告 JSON 失败: %v (body=%s)", err, body)
	}
	return out
}

// ---- 权限矩阵 ----

// TestUsageReport_PermissionMatrix 权限矩阵：
//   - user 查自己 → 200 且有数据；
//   - user 查他人 → 403；
//   - user 无 owner（全量）→ 403；
//   - admin 查全量/任意 owner → 200；
//   - reader 查自己 → 200（无数据 → 空数组，报告语义不 404）。
func TestUsageReport_PermissionMatrix(t *testing.T) {
	t.Parallel()
	url := newUsageReportServer(t)

	// user 上传产生用量（落 testAccessKey 桶）。
	if st := uploadSignedAs(t, url, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user 上传 status = %d, want 200", st)
	}

	// user 查自己。
	st, body := usageReportGet(t, url, "?owner="+testAccessKey, testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("user 查自己 status = %d, want 200 (body=%s)", st, body)
	}
	rep := decodeUsageReport(t, body)
	if len(rep.Reports) != 1 || rep.Reports[0].Kinds["upload_bytes"] <= 0 {
		t.Fatalf("user 自查询应含 upload_bytes 数据: %+v", rep)
	}

	// user 查他人 → 403。
	{
		st2, _ := usageReportGet(t, url, "?owner="+testAdminKey, testAccessKey, testAccessSecret)
		if st2 != http.StatusForbidden {
			t.Fatalf("user 查他人 status = %d, want 403", st2)
		}
	}

	// user 无 owner（全量）→ 403。
	{
		st2, _ := usageReportGet(t, url, "", testAccessKey, testAccessSecret)
		if st2 != http.StatusForbidden {
			t.Fatalf("user 查全量 status = %d, want 403", st2)
		}
	}

	// admin 查全量 → 200 且含 user 数据。
	st, body = usageReportGet(t, url, "", testAdminKey, testAdminSecret)
	if st != http.StatusOK {
		t.Fatalf("admin 查全量 status = %d, want 200 (body=%s)", st, body)
	}
	rep = decodeUsageReport(t, body)
	found := false
	for _, r := range rep.Reports {
		if r.Owner == testAccessKey && r.Kinds["upload_bytes"] > 0 {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin 全量报告应含 user 用量: %+v", rep)
	}

	// admin 查任意 owner → 200。
	{
		st2, _ := usageReportGet(t, url, "?owner="+testAccessKey, testAdminKey, testAdminSecret)
		if st2 != http.StatusOK {
			t.Fatalf("admin 查他人 status = %d, want 200", st2)
		}
	}

	// reader 查自己 → 200 空数组（无数据报告语义）。
	st, body = usageReportGet(t, url, "?owner="+readerTestAK, readerTestAK, readerTestSK)
	if st != http.StatusOK {
		t.Fatalf("reader 查自己 status = %d, want 200 (body=%s)", st, body)
	}
	rep = decodeUsageReport(t, body)
	if len(rep.Reports) != 0 {
		t.Fatalf("reader 无数据应返回空数组: %+v", rep)
	}
}

// ---- 错误处理 ----

// TestUsageReport_InvalidParams format/from/to 非法与缺失 → 400。
func TestUsageReport_InvalidParams(t *testing.T) {
	t.Parallel()
	url := newUsageReportServer(t)

	cases := []struct {
		name  string
		query string
	}{
		{"非法 format", "?owner=" + testAccessKey + "&format=xml"},
		{"from 非法", "?owner=" + testAccessKey + "&from=2026-13-01&to=2026-09-30"},
		{"to 非法", "?owner=" + testAccessKey + "&from=2026-09-01&to=2026/09/30"},
		{"from>to", "?owner=" + testAccessKey + "&from=2026-09-30&to=2026-09-01"},
		{"to 非日期", "?owner=" + testAccessKey + "&from=2026-09-01&to=abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, body := usageReportGet(t, url, tc.query, testAccessKey, testAccessSecret)
			if st != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body=%s)", st, body)
			}
		})
	}
}

// TestUsageReport_Disabled 未启用（usage.enabled=false）→ 400。
func TestUsageReport_Disabled(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuthSeamServer(t, nil, func(opts *RegisterRoutesOpts) {
		withTestCreds(opts)
	})
	st, body := usageReportGet(t, url, "?owner="+testAccessKey, testAccessKey, testAccessSecret)
	if st != http.StatusBadRequest {
		t.Fatalf("未启用 status = %d, want 400 (body=%s)", st, body)
	}
}

// ---- 导出格式与聚合 ----

// TestUsageReport_JSONAndCSV JSON 默认格式聚合正确 + CSV 表头/行。
func TestUsageReport_JSONAndCSV(t *testing.T) {
	t.Parallel()
	url := newUsageReportServer(t)

	// user 上传（13 字节 "rbac upload body"）→ upload_bytes 入账；再下载 → download_bytes。
	if st := uploadSignedAs(t, url, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user 上传 status = %d, want 200", st)
	}
	if st := signedGetStatus(t, url, "/download?filename=rbac.txt", testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user 下载 status = %d, want 200", st)
	}

	wantBytes := int64(len("rbac upload body"))

	// JSON 默认。
	st, body := usageReportGet(t, url, "?owner="+testAccessKey, testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("JSON status = %d, want 200", st)
	}
	rep := decodeUsageReport(t, body)
	if len(rep.Reports) != 1 {
		t.Fatalf("reports 应含 1 条: %+v", rep)
	}
	r := rep.Reports[0]
	if r.Owner != testAccessKey || r.Kinds["upload_bytes"] != wantBytes {
		t.Fatalf("JSON upload_bytes = %+v, want %d", r.Kinds, wantBytes)
	}
	if r.Kinds["download_bytes"] != wantBytes {
		t.Fatalf("JSON download_bytes = %+v, want %d", r.Kinds, wantBytes)
	}

	// CSV。
	st, body = usageReportGet(t, url, "?owner="+testAccessKey+"&format=csv", testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("CSV status = %d, want 200", st)
	}
	text := string(body)
	if !strings.HasPrefix(text, "owner,from,to,days,kind,value") {
		t.Fatalf("CSV 缺表头: %q", text)
	}
	if !strings.Contains(text, "upload_bytes,"+fmt.Sprint(wantBytes)+"\r\n") {
		t.Fatalf("CSV 缺 upload_bytes 行: %q", text)
	}
	if !strings.Contains(text, "download_bytes,"+fmt.Sprint(wantBytes)+"\r\n") {
		t.Fatalf("CSV 缺 download_bytes 行: %q", text)
	}
}

// TestUsageReport_EmptyReports 无数据（显式 owner 无记录）→ 200 空数组。
func TestUsageReport_EmptyReports(t *testing.T) {
	t.Parallel()
	url := newUsageReportServer(t)
	st, body := usageReportGet(t, url, "?owner="+readerTestAK, readerTestAK, readerTestSK)
	if st != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", st, body)
	}
	rep := decodeUsageReport(t, body)
	if len(rep.Reports) != 0 {
		t.Fatalf("无数据应返回空数组: %+v", rep)
	}
}

// TestUsageReport_UploadFeedsUsageStore 真实上传 → usageStore 有数据 + /metrics 暴露 owner 序列。
func TestUsageReport_UploadFeedsUsageStore(t *testing.T) {
	t.Parallel()
	url := newUsageReportServer(t)
	if st := uploadSignedAs(t, url, testAccessKey, testAccessSecret); st != http.StatusOK {
		t.Fatalf("user 上传 status = %d, want 200", st)
	}

	st, body := usageReportGet(t, url, "?owner="+testAccessKey, testAccessKey, testAccessSecret)
	if st != http.StatusOK {
		t.Fatalf("status = %d, want 200", st)
	}
	rep := decodeUsageReport(t, body)
	if len(rep.Reports) != 1 || rep.Reports[0].Kinds["upload_bytes"] != int64(len("rbac upload body")) {
		t.Fatalf("上传应入 usageStore: %+v", rep)
	}

	// Prometheus 面（/metrics 匿名可读）同步暴露 owner 序列。
	resp, err := testHTTPClient(t).Get(url + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	mbody, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(mbody), `sproxy_usage_upload_bytes_total{owner="`+testAccessKey+`"}`) {
		t.Fatalf("metrics 缺 owner 序列:\n%s", mbody)
	}
}

// ---- Metrics owner 维度 ----

// TestUsageMetrics_OwnerCounters 直接单测 owner 计数与 /metrics 渲染。
func TestUsageMetrics_OwnerCounters(t *testing.T) {
	t.Parallel()
	ts, h := newTestServerWithMetrics(t)
	h.metrics.RecordUploadForOwner("owner-a", 100)
	h.metrics.RecordUploadForOwner("owner-a", 50)
	h.metrics.RecordDownloadForOwner("owner-b", 30)

	body := metricsBody(t, ts)
	for _, want := range []string{
		`# TYPE sproxy_usage_upload_bytes_total counter`,
		`sproxy_usage_upload_bytes_total{owner="owner-a"} 150`,
		`sproxy_usage_download_bytes_total{owner="owner-b"} 30`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("缺少 %q:\n%s", want, body)
		}
	}
}

// TestUsageMetrics_OwnerNilSafe 无 metrics / 空 owner / 零字节的 owner 记录不 panic 且不计数。
func TestUsageMetrics_OwnerNilSafe(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	h.metrics.RecordUploadForOwner("owner-a", 10)
	h.metrics.RecordDownloadForOwner("owner-b", 20)

	m := NewMetrics()
	m.RecordUploadForOwner("", 10)   // 空 owner 跳过
	m.RecordDownloadForOwner("o", 0) // 零字节跳过
	m.RecordUploadForOwner("o", -1)  // 负字节跳过
	if len(m.usageUpload.samples()) != 0 || len(m.usageDownload.samples()) != 0 {
		t.Fatalf("空 owner/非正字节不应计数: %+v %+v", m.usageUpload.samples(), m.usageDownload.samples())
	}
}
