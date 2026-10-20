// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_sync_scenarios_test.go 补设计 §9.1「sync HTTP（任务/冲突）」中的**任务创建**审计：
// `sync_task_create` —— 同步任务会持续改动数据（推/拉），创建动作必须可追溯（谁、对哪个远端、
// 哪个任务 ID）。冲突解决（`sync_conflict_resolve`）需要先由真实同步执行产生冲突，另行补齐
// （见 docs/testing/audit-scenarios.md §3）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

// TestAuditScenario_SyncTaskCreate 创建同步任务 ⇒ 一条 sync_task_create（object=任务 ID）。
func TestAuditScenario_SyncTaskCreate(t *testing.T) {
	t.Parallel()
	h, _ := newUserVolumeAPIHandlers(t, true) // 该 harness 装配了 syncMgr
	attachTestAudit(t, h)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sync/tasks", func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(withActor(r.Context(), "alice"))
		h.syncCreateTask(w, r)
	})

	body := strings.NewReader(`{"direction":"push","remote":"userdisk1"}`)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/sync/tasks", body))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("创建同步任务 = %d, want 201/200 (body: %s)", rec.Code, rec.Body.String())
	}

	entries := h.readAuditEvents(context.Background(), audit.EntryFilter{Action: "sync_task_create"}, 0, false)
	if len(entries) == 0 {
		t.Fatal("缺少 sync_task_create 审计行（同步任务会持续改数据 ✗）")
	}
	ev := entries[len(entries)-1]
	if ev.Object == "" {
		t.Error("sync_task_create object（任务 ID）不能为空")
	}
	if ev.Actor != "alice" {
		t.Errorf("actor = %q, want alice", ev.Actor)
	}
	if ev.Result != "success" {
		t.Errorf("result = %q, want success", ev.Result)
	}
}
