// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_presign_scenarios_test.go 补设计 §9.1 的 **presign 类**审计场景（P1 缺口）：
// 预签名签发（`backend_presign`）与直传登记（`backend_presign_complete`）**成功与被拒都要留痕**。
//
// 为什么这是高危面：presign 用**服务端凭据**换直传 URL，客户端随后绕过一切 FS 装饰器直接写
// 对象存储 ⇒ 403（ACL/占用目录/封装封印）与 400（参数篡改）恰恰是**攻击信号**，此前
// 这些失败路径一条审计都没有 ✗（本批修复：`presignStatusWriter` + 状态码观察）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// auditPresignBackend 是最小 Presigner 替身（场景前缀命名，避免与既有替身重名）。
type auditPresignBackend struct{}

func (auditPresignBackend) FS() syncpkg.FS { return nil }
func (auditPresignBackend) Close() error   { return nil }
func (auditPresignBackend) PresignedURL(_ context.Context, path, method string, _ int64) (string, error) {
	return "https://mock.audit/" + method + "/" + path, nil
}

// TestAuditScenario_Presign 签发成功 / 未注册类型被拒 / 登记被拒 —— 三条都必须有审计行。
func TestAuditScenario_Presign(t *testing.T) {
	t.Parallel()
	const typ = "backends-audit-presign-test"
	registry.RegisterBackend(typ, func(context.Context, volume.Volume) (registry.ExternalBackend, error) {
		return auditPresignBackend{}, nil
	})
	defer registry.UnregisterBackendForTest(typ)

	h := &Handlers{}
	attachTestAudit(t, h)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/backends/{type}/presign", h.backendPresignHandler)
	mux.HandleFunc("POST /api/backends/{type}/presign/complete", h.backendPresignCompleteHandler)

	post := func(target string) int {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, target, nil))
		return rec.Code
	}

	// ① 成功签发（GET 直传不受 PUT 占用/封印守卫约束）。
	if st := post("/api/backends/" + typ + "/presign?path=a.txt&method=GET"); st != http.StatusOK {
		t.Fatalf("presign 成功路径 status = %d, want 200", st)
	}
	// ② 未注册类型 ⇒ 404（高危面被拒，必须留痕）。
	if st := post("/api/backends/no-such-type-audit/presign?path=a.txt&method=GET"); st != http.StatusNotFound {
		t.Fatalf("未注册类型 status = %d, want 404", st)
	}
	// ③ 登记被拒（同款未注册类型）⇒ 404 + 留痕。
	if st := post("/api/backends/no-such-type-audit/presign/complete?path=a.txt"); st != http.StatusNotFound {
		t.Fatalf("complete 未注册类型 status = %d, want 404", st)
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	if len(entries) == 0 {
		t.Fatal("presign 操作应产生审计行（埋点缺失或读面不通 ✗）")
	}

	var successSign, failedSign, failedComplete int
	for _, ev := range entries {
		switch {
		case ev.Action == "backend_presign" && ev.Result == "success" && ev.Object == typ:
			successSign++
		case ev.Action == "backend_presign" && ev.Result == AuditResultError:
			failedSign++
		case ev.Action == "backend_presign_complete" && ev.Result == AuditResultError:
			failedComplete++
		}
	}
	if successSign != 1 {
		t.Errorf("签发成功应有 1 条 success 审计, got %d", successSign)
	}
	if failedSign != 1 {
		t.Errorf("被拒的签发应有 1 条 failed 审计, got %d（修复前为 0 ⇒ 必红）", failedSign)
	}
	if failedComplete != 1 {
		t.Errorf("被拒的登记应有 1 条 failed 审计, got %d", failedComplete)
	}
}
