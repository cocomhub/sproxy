// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/server/issues"
	"github.com/cocomhub/sproxy/pkg/server/router"
)

// newIssuesAPIForTest 构造带两条事项的 API（一条需人工、一条可自动重试）。
func newIssuesAPIForTest(t *testing.T) (*IssuesAPI, *IssueCenter) {
	t.Helper()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	c.ReportStorageIssue("baidu", string(issues.KindDirMetaAmbiguous), "OypE/abc",
		"容器内有 2 个 dir-meta；建议：人工仲裁后经 GC 清理旧 blob", false)
	c.ReportStorageIssue("baidu", string(issues.KindContainerNotIndexed), "OypE/def",
		"dir_id 不在索引；建议：重建索引", true)
	return NewIssuesAPI(c), c
}

func doReq(h http.HandlerFunc, method, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// handlerFor 从 Routes() 里取指定 method+path 的 handler（保证测试走**注册表**而不是绕过）。
func handlerFor(t *testing.T, a *IssuesAPI, method, path string) http.HandlerFunc {
	t.Helper()
	for _, r := range a.Routes() {
		if r.Method == method && r.Path == path {
			return r.H
		}
	}
	t.Fatalf("路由不存在：%s %s", method, path)
	return nil
}

// TestIssuesAPI_Routes_Shape 锁定路由注册契约（R38：分类必须显式声明，写面必须标 Mutating）。
func TestIssuesAPI_Routes_Shape(t *testing.T) {
	t.Parallel()
	a, _ := newIssuesAPIForTest(t)
	want := map[string]struct {
		mutating bool
	}{
		"GET /issues":          {false},
		"GET /issues/detail":   {false},
		"POST /issues/resolve": {true},
		"POST /issues/scan":    {true},
	}
	routes := a.Routes()
	if len(routes) != len(want) {
		t.Fatalf("路由数 = %d，期望 %d", len(routes), len(want))
	}
	for _, r := range routes {
		key := r.Method + " " + r.Path
		exp, ok := want[key]
		if !ok {
			t.Fatalf("未预期的路由 %s", key)
		}
		if r.Mutating != exp.mutating {
			t.Errorf("%s Mutating = %v，期望 %v", key, r.Mutating, exp.mutating)
		}
		if r.Server != router.ClassAuth || r.Local != router.ClassTunnel {
			t.Errorf("%s 分类 = (server=%v local=%v)，期望 (auth, tunnel)", key, r.Server, r.Local)
		}
	}
}

// TestIssuesAPI_ListAndFilters 锁定列表与筛选（UI 依赖：按状态/卷/未解决筛）。
func TestIssuesAPI_ListAndFilters(t *testing.T) {
	t.Parallel()
	a, _ := newIssuesAPIForTest(t)
	h := handlerFor(t, a, "GET", "/issues")

	rec := doReq(h, "GET", "/api/issues")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	var v issuesListView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if v.Total != 2 {
		t.Fatalf("total = %d，期望 2", v.Total)
	}
	if v.Stats[string(issues.StatusNeedsHuman)] != 1 || v.Stats[string(issues.StatusOpen)] != 1 {
		t.Fatalf("stats 不符：%+v", v.Stats)
	}

	// 按状态筛
	rec = doReq(h, "GET", "/api/issues?status=needs_human")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Items) != 1 || v.Items[0].Kind != issues.KindDirMetaAmbiguous {
		t.Fatalf("按状态筛选不符：%+v", v.Items)
	}
	// 按未解决筛
	rec = doReq(h, "GET", "/api/issues?unresolved=1&volume=baidu")
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if len(v.Items) != 2 {
		t.Fatalf("按卷+未解决筛选应 2 条，got %d", len(v.Items))
	}
}

// TestIssuesAPI_Detail 锁定详情端点（人工判断依据：Detail/LastError 必须返回）。
func TestIssuesAPI_Detail(t *testing.T) {
	t.Parallel()
	a, c := newIssuesAPIForTest(t)
	h := handlerFor(t, a, "GET", "/issues/detail")
	id := c.Manager().List(issues.ListFilter{Kind: issues.KindDirMetaAmbiguous})[0].ID

	rec := doReq(h, "GET", "/api/issues/detail?id="+id)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	var it issues.Item
	if err := json.Unmarshal(rec.Body.Bytes(), &it); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !strings.Contains(it.Detail, "人工仲裁") {
		t.Fatalf("详情应含**详细失败原因/处置建议**，got %q", it.Detail)
	}
	// 缺 id / 不存在的 id
	if rec := doReq(h, "GET", "/api/issues/detail"); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 id 应 400，got %d", rec.Code)
	}
	if rec := doReq(h, "GET", "/api/issues/detail?id=nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("不存在应 404，got %d", rec.Code)
	}
}

// TestIssuesAPI_Resolve 锁定人工闭环：必须带 note（审计），成功后从未解决列表消失。
func TestIssuesAPI_Resolve(t *testing.T) {
	t.Parallel()
	a, c := newIssuesAPIForTest(t)
	h := handlerFor(t, a, "POST", "/issues/resolve")
	id := c.Manager().List(issues.ListFilter{Kind: issues.KindDirMetaAmbiguous})[0].ID

	// 缺 note ⇒ 400（强制人工写明处置方式）
	if rec := doReq(h, "POST", "/api/issues/resolve?id="+id); rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 note 应 400，got %d", rec.Code)
	}
	rec := doReq(h, "POST", "/api/issues/resolve?id="+id+"&"+url.Values{
		"note": {"已确认保留 Version 较大者并经 GC 删除旧 blob"},
	}.Encode())
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，body=%s", rec.Code, rec.Body.String())
	}
	if n := len(c.Manager().List(issues.ListFilter{UnresolvedOnly: true})); n != 1 {
		t.Fatalf("解决后未解决应剩 1 条，got %d", n)
	}
	it, _ := c.Manager().Get(id)
	if it.Resolution == "" {
		t.Fatal("应记录人工处置说明（审计）")
	}
}

// TestIssuesAPI_Scan 锁定手动扫描端点：未注入 scanner ⇒ 501（可观测，不假装成功）；
// 注入后返回统计。
func TestIssuesAPI_Scan(t *testing.T) {
	t.Parallel()
	a, _ := newIssuesAPIForTest(t)
	h := handlerFor(t, a, "POST", "/issues/scan")

	if rec := doReq(h, "POST", "/api/issues/scan"); rec.Code != http.StatusNotImplemented {
		t.Fatalf("未注入 scanner 应 501，got %d", rec.Code)
	}
	called := 0
	a.SetScanner(func(context.Context) ScanStats {
		called++
		return ScanStats{Volumes: 2, Nodes: 7, Reported: 1}
	})
	rec := doReq(h, "POST", "/api/issues/scan")
	if rec.Code != http.StatusOK || called != 1 {
		t.Fatalf("状态码/调用次数不符：%d/%d", rec.Code, called)
	}
	var sv scanView
	if err := json.Unmarshal(rec.Body.Bytes(), &sv); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if !sv.OK || sv.Volumes != 2 || sv.Reported != 1 {
		t.Fatalf("扫描统计不符：%+v", sv)
	}
}

// TestIssuesAPI_Unwired 锁定"未装配"可观测（503 而不是 panic/空 200）。
func TestIssuesAPI_Unwired(t *testing.T) {
	t.Parallel()
	a := NewIssuesAPI(nil)
	for _, r := range a.Routes() {
		rec := doReq(r.H, r.Method, "/api/issues")
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s 未装配应 503，got %d", r.Method, r.Path, rec.Code)
		}
	}
}
