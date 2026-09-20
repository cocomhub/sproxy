// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	u "net/url"
	"sync/atomic"
	"testing"
	"time"
)

// requestAuditExport 发起一次带 SproxySig 签名的 GET /api/audit/export 请求。
func requestAuditExport(t *testing.T, url, query string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url+"/api/audit/export"+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	signRequest(req, testAccessKey, testAccessSecret)
	// 每测试自建独立 client（禁共享 DefaultTransport——并行用例的 server.Close()
	// 会打断共享池在途连接）。
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET /api/audit/export: %v", err)
	}
	return resp
}

// decodeAuditExport 解析导出响应体（JSON 数组）。
func decodeAuditExport(t *testing.T, resp *http.Response) []AuditEvent {
	t.Helper()
	defer resp.Body.Close()
	var got []AuditEvent
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode /api/audit/export body: %v", err)
	}
	return got
}

// TestAuditExport_Basic 验证导出返回 JSON 数组，元素含 action/actor/ts 字段，按 ts 升序。
func TestAuditExport_Basic(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newAuditTestServer(t, nil)
	// 直接写 ring：经 h.RecordAudit 无法控制 TS，测试用带显式 TS 的事件走真实
	// handler 无法注入——因此用 writeUploadFile + delete 产生真实 delete 审计事件，
	// 再断言导出含该事件（TS 为记录时刻，升序由 ring 顺序保证）。
	body := []byte("export-me")
	writeUploadFile(t, cfgPtr, "exp-del.txt", body)
	delReq, _ := http.NewRequest(http.MethodPost, url+"/delete?filename=exp-del.txt", nil)
	delReq.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(delReq, testAccessKey, testAccessSecret)
	delResp, err := testHTTPClient(t).Do(delReq)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("delete 应 200, got %d", delResp.StatusCode)
	}

	resp := requestAuditExport(t, url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, want 200", resp.StatusCode)
	}
	got := decodeAuditExport(t, resp)
	if len(got) < 1 {
		t.Fatalf("export 应为非空数组, got %d 条", len(got))
	}
	found := false
	for _, ev := range got {
		if ev.Action == "delete" && ev.Object == "exp-del.txt" {
			found = true
			if ev.Actor != testAccessKey {
				t.Errorf("export delete 事件 actor = %q, want %q", ev.Actor, testAccessKey)
			}
			if ev.TS.IsZero() {
				t.Errorf("export delete 事件 TS 为零值")
			}
		}
	}
	if !found {
		t.Fatalf("export 未找到 delete exp-del.txt 事件: %+v", got)
	}
	// 升序校验：TS 非降。
	for i := 1; i < len(got); i++ {
		if got[i].TS.Before(got[i-1].TS) {
			t.Errorf("export 事件未按 TS 升序: [%d]=%v > [%d]=%v", i-1, got[i-1].TS, i, got[i].TS)
		}
	}
}

// TestAuditExport_Filters 验证 action/actor/after_ts 过滤。
func TestAuditExport_Filters(t *testing.T) {
	t.Parallel()
	url, cfgPtr, _ := newAuditTestServer(t, nil)
	body := []byte("filter-export")
	for _, name := range []string{"f1.txt", "f2.txt"} {
		writeUploadFile(t, cfgPtr, name, body)
		req, _ := http.NewRequest(http.MethodPost, url+"/delete?filename="+name, nil)
		req.Header.Set("X-File-Checksum", sha256hex(body))
		signRequest(req, testAccessKey, testAccessSecret)
		resp, err := testHTTPClient(t).Do(req)
		if err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("delete %s 应 200, got %d", name, resp.StatusCode)
		}
	}

	// action 过滤：只含 delete。
	aresp := requestAuditExport(t, url, "?action=delete")
	agot := decodeAuditExport(t, aresp)
	if len(agot) < 2 {
		t.Fatalf("action=delete 过滤应 >= 2 条, got %d", len(agot))
	}
	for _, ev := range agot {
		if ev.Action != "delete" {
			t.Errorf("action=delete 过滤含非 delete 事件: %+v", ev)
		}
	}

	// actor 过滤：全为 testAccessKey。
	uresp := requestAuditExport(t, url, "?actor="+testAccessKey)
	ugot := decodeAuditExport(t, uresp)
	if len(ugot) < 2 {
		t.Fatalf("actor 过滤应 >= 2 条, got %d", len(ugot))
	}
	for _, ev := range ugot {
		if ev.Actor != testAccessKey {
			t.Errorf("actor 过滤含非 %q 事件: %+v", testAccessKey, ev)
		}
	}

	// after_ts 过滤：未来时间 → 0 条。RFC3339 含 '+'（时区偏移），须 URL 编码。
	q := u.Values{}
	q.Set("after_ts", time.Now().Add(time.Hour).Format(time.RFC3339))
	fresp := requestAuditExport(t, url, "?"+q.Encode())
	fgot := decodeAuditExport(t, fresp)
	if len(fgot) != 0 {
		t.Fatalf("after_ts=未来应 0 条, got %d", len(fgot))
	}
}

// TestAuditExport_AfterTSInvalid 验证 after_ts 非法返回 400。
func TestAuditExport_AfterTSInvalid(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuditTestServer(t, nil)
	resp := requestAuditExport(t, url, "?after_ts=not-a-time")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("after_ts=not-a-time 应 400, got %d", resp.StatusCode)
	}
}

// TestAuditExport_Empty 验证空 ring 导出空数组 200。
func TestAuditExport_Empty(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuditTestServer(t, nil)
	resp := requestAuditExport(t, url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, want 200", resp.StatusCode)
	}
	got := decodeAuditExport(t, resp)
	if len(got) != 0 {
		t.Fatalf("空 ring 应导出空数组, got %d 条", len(got))
	}
}

// TestAuditExport_Disabled 验证 audit 未启用（ring nil）导出空数组 200（与 /api/audit 一致）。
func TestAuditExport_Disabled(t *testing.T) {
	t.Parallel()
	// audit.buffer_size=0 关闭 ring。
	url, _, _ := newAuditTestServer(t, func(cfg *Config) {
		cfg.Audit.BufferSize = 0
	})
	resp := requestAuditExport(t, url, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export status = %d, want 200", resp.StatusCode)
	}
	got := decodeAuditExport(t, resp)
	if len(got) != 0 {
		t.Fatalf("audit 关闭应导出空数组, got %d 条", len(got))
	}
}

// TestAuditExport_NoAuthUnauthorized 验证主 mux 无凭据访问 /api/audit/export 返回 401
// （与 /api/audit 同款 authMiddleware 保护）。
func TestAuditExport_NoAuthUnauthorized(t *testing.T) {
	t.Parallel()
	url, _, _ := newAuditTestServer(t, nil)
	req, _ := http.NewRequest(http.MethodGet, url+"/api/audit/export", nil)
	client := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(client.CloseIdleConnections)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("no-auth export: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401, got %d", resp.StatusCode)
	}
}

// TestAuditExport_LocalMuxReachable 验证 /api/audit/export 在隧道内层（localMux）可达：
// 与 /api/audit 同款双注册（隧道内层裸注册）。
func TestAuditExport_LocalMuxReachable(t *testing.T) {
	t.Parallel()
	// 复用现有 /api/audit 的 localMux 可达测试形态（见 audit_handler_test.go 的
	// TestAuditHandler_LocalMuxReachable）：经 localMux 直接调用 export handler。
	// 注意 StorageRoot 必须显式 t.TempDir()——Default() 的 "./storage" 相对路径会
	// 在包目录（go test CWD=pkg/server/）创建卷根，污染同包 F1 门禁测试。
	cfg := Default()
	cfg.StorageRoot = t.TempDir()
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	mux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:     mux,
		CfgPtr:  &cfgPtr,
		Version: "test",
		BuildAt: "test",
		Logger:  testLogger(),
	})
	t.Cleanup(func() { _ = h.Close() })

	// 直接调 handler（localMux 注册路径 = handler 直连，无额外鉴权）。
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/audit/export", nil)
	h.auditExportHandler(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("localMux 面 export 应 200, got %d", rr.Code)
	}
}
