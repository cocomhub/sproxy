// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

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
// /api/audit 返回 200 + 空 events（非 null）+ total 0（不 404）。
// 同时验证 events 序列化为 [] 而非 null：两条关闭路径（ring nil 显式 []AuditEvent{}
// 与 ring 空 Recent 返回 []）都不得弹出 null。
func TestAuditHandler_DisabledRingReturnsEmpty(t *testing.T) {
	url, _, _ := newAuditTestServer(t, func(cfg *Config) {
		cfg.Audit.BufferSize = 0 // 显式关闭 ring
	})
	gresp := requestAudit(t, url, "")
	got := decodeAuditResponse(t, gresp)
	if gresp.StatusCode != http.StatusOK {
		t.Fatalf("禁用 ring 应 200, got %d", gresp.StatusCode)
	}
	if got.Total != 0 {
		t.Fatalf("禁用 ring 应 total=0, got %d", got.Total)
	}
	if got.Events == nil {
		t.Fatal("禁用 ring 的 events 应为非 nil 空数组（序列化为 [] 而非 null）")
	}
	if len(got.Events) != 0 {
		t.Fatalf("禁用 ring 应 events 为空, got %+v", got.Events)
	}

	// 防 JSON null 弹出：原始 body 不得含裸 null 值且 events 键必须序列化成 []。
	// 通过 RawMessage 解析原始 body 核对 events 字段字面量。
	rawBody := readAuditRawBody(t, http.StatusOK, url, "")
	if !json.Valid(rawBody) {
		t.Fatalf("响应体非法 JSON: %s", rawBody)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &m); err != nil {
		t.Fatalf("解析原始响应体: %v", err)
	}
	if string(m["events"]) != "[]" {
		t.Fatalf("events 原始字面量 = %s, want []（不得为 null）", m["events"])
	}
}

// TestAuditHandler_ConfigBufferSize 验证默认启用语义：Default()+SetDefaults 保持
// 2048（默认启用 ring）；&Config{}+SetDefaults 时 BufferSize 保持 0（不被复活）——
// 显式 0 = 关闭必须可达（加载链 Default→Unmarshal→SetDefaults 不得把 0 改回 2048）。
func TestAuditHandler_ConfigBufferSize(t *testing.T) {
	// 默认加载路径：Default() 已含 Audit.BufferSize=2048，SetDefaults 不改变它。
	cfg := Default()
	cfg.SetDefaults()
	if cfg.Audit.BufferSize != 2048 {
		t.Fatalf("默认 Audit.BufferSize = %d, want 2048", cfg.Audit.BufferSize)
	}
	// 显式 0 关闭：&Config{}+SetDefaults 必须保持 0（SetDefaults 禁止复活 0 → 2048）。
	empty := &Config{}
	empty.SetDefaults()
	if empty.Audit.BufferSize != 0 {
		t.Fatalf("&Config{}+SetDefaults 的 BufferSize = %d, want 0（显式 0=关闭 不可被复活）", empty.Audit.BufferSize)
	}
}

// TestAuditHandler_LoadFromProviderBufferSize 验证加载级语义：
//   - 显式 audit.buffer_size=0 → 解析为 0（0=关闭可达，且 Validate 不报错）；
//   - 空 map 加载 → 默认 2048。
func TestAuditHandler_LoadFromProviderBufferSize(t *testing.T) {
	// 显式 0 关闭：LoadFromProvider 全链（Default→Unmarshal→SetDefaults→Validate）后
	// BufferSize 必须保持 0。
	disabled, err := LoadFromProvider(mapProvider{m: map[string]any{
		"audit": map[string]any{"buffer_size": 0},
	}})
	if err != nil {
		t.Fatalf("audit.buffer_size=0 应合法（Validate 不报错）: %v", err)
	}
	if disabled.Audit.BufferSize != 0 {
		t.Fatalf("audit.buffer_size=0 解析后 = %d, want 0（0=关闭 不可被 SetDefaults 复活）", disabled.Audit.BufferSize)
	}
	// 默认加载：空 map → 默认 2048。
	def, err := LoadFromProvider(mapProvider{m: map[string]any{}})
	if err != nil {
		t.Fatalf("LoadFromProvider(空): %v", err)
	}
	if def.Audit.BufferSize != 2048 {
		t.Fatalf("默认加载 BufferSize = %d, want 2048", def.Audit.BufferSize)
	}
}

// readAuditRawBody 复用 requestAudit 取原始 body 字节（不解析结构）。
func readAuditRawBody(t *testing.T, wantStatus int, url, query string) []byte {
	t.Helper()
	resp := requestAudit(t, url, query)
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("status = %d, want %d", resp.StatusCode, wantStatus)
	}
	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatalf("读取 body: %v", err)
	}
	return buf.Bytes()
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
