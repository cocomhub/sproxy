// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_user_volume_scenarios_test.go 补设计 §9.1「卷管理」类中的**用户卷**审计场景：
// `user_volume_create` / `user_volume_delete` —— 破坏性操作必须可追溯（谁、对哪个卷、成功与否）。
//
// 断言走 `/api/audit` 的同一读入口 `readAuditEvents`（复用 `attachTestAudit` 的真实审计服务）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_UserVolumeLifecycle 建卷 + 删卷 ⇒ 两条审计，actor/object 正确。
func TestAuditScenario_UserVolumeLifecycle(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, false)
	attachTestAudit(t, h)
	mux := userVolWrap(h, "alice")
	const volName = "audit-userdisk"

	if rec := postUserVolume(t, mux, map[string]any{
		"name": volName, "type": userVolumeTestType, "capacity": 1000,
		"extra": map[string]any{"bduss": "test"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("建卷 = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/api/volumes/user?name="+volName, nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("删卷 = %d, want 200 (body: %s)", delRec.Code, delRec.Body.String())
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{}, 0, false)
	seen := map[string]bool{}
	for _, ev := range entries {
		if ev.Object != volName {
			continue
		}
		switch ev.Action {
		case "user_volume_create", "user_volume_delete":
			seen[ev.Action] = true
			if ev.Actor != "alice" {
				t.Errorf("%s actor = %q, want alice", ev.Action, ev.Actor)
			}
			if ev.Result != "success" {
				t.Errorf("%s result = %q, want success", ev.Action, ev.Result)
			}
		}
	}
	if !seen["user_volume_create"] {
		t.Error("缺少 user_volume_create 审计行")
	}
	if !seen["user_volume_delete"] {
		t.Error("缺少 user_volume_delete 审计行（破坏性操作必须可追溯 ✗）")
	}
}
