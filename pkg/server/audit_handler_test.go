// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// auditHandlerResponse 是 GET /api/audit 的响应结构（对齐 audit_handler.go）。
type auditHandlerResponse struct {
	Events []AuditEvent `json:"events"`
	Total  int          `json:"total"`
}

// requestAudit 发起一次带 SproxySig 签名的 GET /api/audit 请求。
func requestAudit(t *testing.T, url, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/api/audit"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/audit: %v", err)
	}
	return resp
}

// decodeAuditResponse 解析审计响应体。
func decodeAuditResponse(t *testing.T, resp *http.Response) auditHandlerResponse {
	t.Helper()
	defer resp.Body.Close()
	var got auditHandlerResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /api/audit body: %v", err)
	}
	return got
}

// TestAuditHandler_ListAfterDelete 验证审计动作（delete）后经 /api/audit 可查到。
func TestAuditHandler_ListAfterDelete(t *testing.T) {
	url, cfgPtr, _ := newAuditTestServer(t, nil)
	body := []byte("audit-me")
	writeUploadFile(t, cfgPtr, "audit-del.txt", body)

	req, _ := http.NewRequest("POST", url+"/delete?filename=audit-del.txt", nil)
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete 应 200, got %d", resp.StatusCode)
	}

	gresp := requestAudit(t, url, "")
	got := decodeAuditResponse(t, gresp)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/audit status = %d, want 200", gresp.StatusCode)
	}
	if got.Total < 1 {
		t.Fatalf("Total = %d, want >= 1（应含 delete 记录）", got.Total)
	}
	found := false
	for _, ev := range got.Events {
		if ev.Action == "delete" && ev.Object == "audit-del.txt" {
			found = true
			if ev.Actor != testAccessKey {
				t.Errorf("delete 审计 actor = %q, want %q", ev.Actor, testAccessKey)
			}
		}
	}
	if !found {
		t.Fatalf("events 中未找到 delete audit-del.txt 记录: %+v", got.Events)
	}
}

// TestAuditHandler_QueryFilters 验证 HTTP 面把 query 翻译成 filter：
// 先做真实审计动作（delete + rename），再按 action/actor/mesh 精确过滤查询。
// 精确相等语义本身由 TestAuditRing_Filter* 覆盖；此处黑盒验证 handler 接线。
func TestAuditHandler_QueryFilters(t *testing.T) {
	url, cfgPtr, _ := newAuditTestServer(t, nil)
	body := []byte("filter-me")
	writeUploadFile(t, cfgPtr, "f-del.txt", body)
	writeUploadFile(t, cfgPtr, "f-old.txt", body)

	delReq, _ := http.NewRequest(http.MethodPost, url+"/delete?filename=f-del.txt", nil)
	delReq.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(delReq, testAccessKey, testAccessSecret)
	delResp, err := http.DefaultClient.Do(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete 应 200, got %d", delResp.StatusCode)
	}

	renReq, _ := http.NewRequest(http.MethodPost, url+"/rename?from=f-old.txt&to=f-new.txt", nil)
	renReq.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(renReq, testAccessKey, testAccessSecret)
	renResp, err := http.DefaultClient.Do(renReq)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	renResp.Body.Close()
	if renResp.StatusCode != http.StatusOK {
		t.Fatalf("rename 应 200, got %d", renResp.StatusCode)
	}

	// 按 action 过滤：仅 delete 事件。
	gresp := requestAudit(t, url, "?limit=5&action=delete")
	got := decodeAuditResponse(t, gresp)
	if got.Total < 1 {
		t.Fatalf("action=delete 过滤 Total = %d, want >= 1", got.Total)
	}
	for _, ev := range got.Events {
		if ev.Action != "delete" {
			t.Errorf("action=delete 过滤含非 delete 事件: %+v", ev)
		}
	}

	// 按 actor 过滤：所有事件 actor 均为 testAccessKey。
	aresp := requestAudit(t, url, "?actor="+testAccessKey)
	agot := decodeAuditResponse(t, aresp)
	if agot.Total < 2 {
		t.Fatalf("actor 过滤 Total = %d, want >= 2", agot.Total)
	}
	for _, ev := range agot.Events {
		if ev.Actor != testAccessKey {
			t.Errorf("actor 过滤含非 %q 事件: %+v", testAccessKey, ev)
		}
	}

	// 按 mesh 过滤：testAccessKey 派生 mesh 与 MeshFrom 一致。
	mesp := requestAudit(t, url, "?mesh="+tunnel.AccessKeyMesh(testAccessKey))
	mgot := decodeAuditResponse(t, mesp)
	if mgot.Total < 2 {
		t.Fatalf("mesh 过滤 Total = %d, want >= 2", mgot.Total)
	}

	// limit 上限验证：limit=100000 clamp 到 500（条数 <= 500）。
	cresp := requestAudit(t, url, "?limit=100000")
	cgot := decodeAuditResponse(t, cresp)
	if len(cgot.Events) > 500 {
		t.Errorf("limit=100000 返回 %d 条, want <= 500（clamp）", len(cgot.Events))
	}
}

// TestAuditHandler_LimitClamp 验证 limit>500 clamp 到 500（返回条数 <= 500）。
func TestAuditHandler_LimitClamp(t *testing.T) {
	url, _, _ := newAuditTestServer(t, nil)
	gresp := requestAudit(t, url, "?limit=100000")
	got := decodeAuditResponse(t, gresp)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", gresp.StatusCode)
	}
	if len(got.Events) > 500 {
		t.Errorf("limit=100000 返回 %d 条, want <= 500（clamp）", len(got.Events))
	}
}

// TestAuditHandler_LimitInvalid 验证 limit 非法（非整数）回落默认（200 + 空/子集）。
func TestAuditHandler_LimitInvalid(t *testing.T) {
	url, _, _ := newAuditTestServer(t, nil)
	gresp := requestAudit(t, url, "?limit=abc")
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("limit=abc 应回落默认并 200, got %d", gresp.StatusCode)
	}
	_ = decodeAuditResponse(t, gresp)
}

// TestAuditHandler_SinceInvalid 验证 since 非法返回 400。
func TestAuditHandler_SinceInvalid(t *testing.T) {
	url, _, _ := newAuditTestServer(t, nil)
	gresp := requestAudit(t, url, "?since=not-a-time")
	defer gresp.Body.Close()
	if gresp.StatusCode != http.StatusBadRequest {
		t.Fatalf("since=not-a-time 应 400, got %d", gresp.StatusCode)
	}
}

// TestAuditHandler_NoAuthUnauthorized 验证无凭据访问 /api/audit 返回 401。
func TestAuditHandler_NoAuthUnauthorized(t *testing.T) {
	url, _, _ := newAuditTestServer(t, nil)
	req, _ := http.NewRequest(http.MethodGet, url+"/api/audit", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("no-auth audit: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401, got %d", resp.StatusCode)
	}
}

// TestAuditHandler_LocalMuxNotRegistered 回归验证 /api/audit 未注册 localMux：
// 非 auth 直连 LocalHandler（隧道内层入口）命中该路径返回 404。
func TestAuditHandler_LocalMuxNotRegistered(t *testing.T) {
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:    mux,
		CfgPtr: &cfgPtr,
		Logger: testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	lh := h.LocalHandler()
	if lh == nil {
		t.Fatal("LocalHandler 返回 nil")
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	lh.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("LocalHandler 直连 GET /api/audit 应为 404（/api/audit 不注册 localMux），got %d (body=%s)", w.Code, w.Body.String())
	}
}

// TestAuditHandler_DisabledRingReturnsEmpty 验证 buffer_size=0（ring 禁用）时
// /api/audit 返回 200 + 空 events + total 0（不 404）。
func TestAuditHandler_DisabledRingReturnsEmpty(t *testing.T) {
	url, _, _ := newAuditTestServer(t, func(cfg *Config) {
		cfg.Audit.BufferSize = 0 // 显式关闭 ring
	})
	gresp := requestAudit(t, url, "")
	got := decodeAuditResponse(t, gresp)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("禁用 ring 应 200, got %d", gresp.StatusCode)
	}
	if got.Total != 0 || len(got.Events) != 0 {
		t.Fatalf("禁用 ring 应 events=[] total=0, got %+v", got)
	}
}

// TestAuditHandler_ConfigBufferSize 验证 SetDefaults 默认 2048（默认启用 ring）。
func TestAuditHandler_ConfigBufferSize(t *testing.T) {
	cfg := Default()
	cfg.SetDefaults()
	if cfg.Audit.BufferSize != 2048 {
		t.Fatalf("默认 Audit.BufferSize = %d, want 2048", cfg.Audit.BufferSize)
	}
}

// TestAuditHandler_ConfigValidateNegative 验证 Audit.BufferSize 为负被 Validate 拒绝。
func TestAuditHandler_ConfigValidateNegative(t *testing.T) {
	cfg := Default()
	cfg.Audit.BufferSize = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("BufferSize=-1 应 Validate 失败, got nil")
	}
}

// TestAuditHandler_SerializeTS 验证事件 TS 经 JSON 序列化为 RFC3339Nano。
func TestAuditHandler_SerializeTS(t *testing.T) {
	ev := AuditEvent{Action: "delete", TS: time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := "2026-09-01T12:00:00Z"
	if m["TS"] != want {
		t.Errorf("TS 序列化 = %v, want %q (RFC3339Nano)", m["TS"], want)
	}
}
