// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// TestArchiveGroup_RequireAllRejectsIncomplete 锁「全有或全无」语义：
// 组内存在未完成子任务时，require_all=true 必须 409 + pending 列表，且**不产生归档文件**。
func TestArchiveGroup_RequireAllRejectsIncomplete(t *testing.T) {
	t.Parallel()
	svc, _, tenantRoot, quotaStub := newGroupArchiveEnv(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/archive",
		strings.NewReader(`{"archive_name":"g1-archive.tar.gz","require_all":true}`))
	req.SetPathValue("id", "g1")

	svc.ArchiveGroup(rec, req, "alice")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	var got CloudArchiveResult
	decodeBody(t, rec, &got)
	if got.Success {
		t.Fatalf("require_all 未满足不得返回 success: %+v", got)
	}
	if got.Message != "group not fully completed" {
		t.Fatalf("message = %q，期望 group not fully completed", got.Message)
	}
	if len(got.SkippedTasks) != 1 || got.SkippedTasks[0] != "t2" {
		t.Fatalf("pending_tasks = %v，期望 [t2]", got.SkippedTasks)
	}
	// 关键：不得留下半成品（归档桶无文件、无配额预留）。
	if _, err := tenantRoot.Stat("archive/g1-archive.tar.gz"); err == nil {
		t.Fatal("require_all 拒绝时不得产生归档文件")
	}
	if len(quotaStub.calls) != 0 {
		t.Fatalf("require_all 拒绝时不得预留配额，实际调用 = %v", quotaStub.calls)
	}
}

// TestArchiveGroup_RequireAllAllowsComplete 锁正向：全部 completed 时 require_all 正常归档。
func TestArchiveGroup_RequireAllAllowsComplete(t *testing.T) {
	t.Parallel()
	svc, groups, tenantRoot, _ := newGroupArchiveEnv(t)
	// 把 t2 补成 completed 并写入对应源文件（夹具默认是 downloading 且无源文件）。
	_ = groups
	svc.deps.Tasks.(*stubTasks).tasks["t2"].Status = "completed"
	if err := tenantRoot.MkdirAll("cloud/t2", 0o755); err != nil {
		t.Fatalf("mkdir t2: %v", err)
	}
	f, cerr := tenantRoot.OpenFile("cloud/t2/b.bin", os.O_CREATE|os.O_WRONLY, 0o644)
	if cerr != nil {
		t.Fatalf("create t2 source: %v", cerr)
	}
	if _, werr := f.Write([]byte("bbbbb")); werr != nil {
		t.Fatalf("write t2 source: %v", werr)
	}
	if clErr := f.Close(); clErr != nil {
		t.Fatalf("close t2 source: %v", clErr)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/groups/g1/archive",
		strings.NewReader(`{"archive_name":"all.tar.gz","require_all":true}`))
	req.SetPathValue("id", "g1")

	svc.ArchiveGroup(rec, req, "alice")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var got CloudArchiveResult
	decodeBody(t, rec, &got)
	if !got.Success || got.TaskCount != 2 || got.SkippedCount != 0 {
		t.Fatalf("响应 = %+v，期望 2 个文件且无跳过", got)
	}
	raw := readRootFile(t, tenantRoot, "archive/all.tar.gz")
	entries := tarEntries(t, raw)
	if entries["t1/a.bin"] != "aaaaa" || entries["t2/b.bin"] != "bbbbb" {
		t.Fatalf("归档条目 = %v，期望含 t1/a.bin 与 t2/b.bin", entries)
	}
}
