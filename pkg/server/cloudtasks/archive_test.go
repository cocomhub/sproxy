// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// 本文件锁云归档入口（P1-c S3a）的**包内契约**：归档名归一的边界、owner 透传到领域侧
// （跨 owner 任务按不存在处理）与批量全跳过的响应体。完整链路（tar 落盘、配额预留/结算、
// 外部卷转存）仍由 pkg/server 的既有 HTTP 测试驱动。

// absentTenants 模拟租户解析不可用（TenantFor 恒 nil）：归档源收集应把它当作 skip 而非报错。
type absentTenants struct{}

func (absentTenants) TenantFor(string) *storage.Tenant { return nil }

// recordingArchiveTasks 记录 SnapshotTask 的 owner 入参（归档必须按请求者过滤，跨 owner
// 任务视为不存在）。
type recordingArchiveTasks struct {
	stubTasks
	snapshotOwner string
}

func (r *recordingArchiveTasks) SnapshotTask(id, owner string) (*cloud.CloudTask, bool) {
	r.snapshotOwner = owner
	return r.stubTasks.SnapshotTask(id, owner)
}

// TestNormalizeArchiveName 锁归档名归一边界：Base 去路径、补 .tar.gz、255 字节上限；
// 错误文案与迁移前逐字一致（既有测试断言 message）。
func TestNormalizeArchiveName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		want    string
		wantMsg string
	}{
		{name: "keeps bare name and appends suffix", input: "backup", want: "backup.tar.gz"},
		{name: "keeps existing suffix", input: "backup.tar.gz", want: "backup.tar.gz"},
		{name: "strips directory components", input: "../../etc/passwd", want: "passwd.tar.gz"},
		{name: "empty name rejected", input: "", wantMsg: "invalid archive name"},
		{name: "dot rejected", input: ".", wantMsg: "invalid archive name"},
		{name: "dotdot rejected", input: "..", wantMsg: "invalid archive name"},
		{name: "too long rejected", input: strings.Repeat("a", 256), wantMsg: "archive name too long"},
		{name: "255 bytes accepted", input: strings.Repeat("a", 248) + ".tar.gz", want: strings.Repeat("a", 248) + ".tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, msg := NormalizeArchiveName(tc.input)
			if msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
			if got != tc.want {
				t.Errorf("name = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestArchiveTask_OwnerFilterAndNotFound 锁单任务归档的 owner 透传与 404 响应体：
// 任务快照必须按**请求者** owner 过滤（跨 owner 即不存在），且响应字节与迁移前一致。
func TestArchiveTask_OwnerFilterAndNotFound(t *testing.T) {
	t.Parallel()

	tasks := &recordingArchiveTasks{tasks: map[string]*cloud.CloudTask{}}
	svc := New(Deps{Tasks: tasks})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/t1/archive", strings.NewReader("{}"))
	req.SetPathValue("id", "t1")

	svc.ArchiveTask(rec, req, "alice")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
	if tasks.snapshotOwner != "alice" {
		t.Fatalf("SnapshotTask owner = %q, want %q（归档必须按请求者过滤）", tasks.snapshotOwner, "alice")
	}
	var got CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Success || got.Message != "task not found" {
		t.Fatalf("response = %+v, want success=false message=%q", got, "task not found")
	}
}

// TestArchiveBatch_AllTasksSkippedReturns400 锁批量归档「全部跳过」的响应体：400 +
// no valid tasks to archive + skipped 列表（skipped 计数与列表逐项保留）。
func TestArchiveBatch_AllTasksSkippedReturns400(t *testing.T) {
	t.Parallel()

	// 已完成任务但租户不可用（TenantFor 返回 nil）⇒ 逐任务 skip。
	tasks := &stubTasks{tasks: map[string]*cloud.CloudTask{
		"t1": {ID: "t1", Owner: "ghost", Filename: "EXAMPLE-001.bin", Status: "completed"},
	}}
	svc := New(Deps{Tasks: tasks, Tenants: absentTenants{}})
	rec := httptest.NewRecorder()
	body := `{"task_ids":["t1"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/archive", strings.NewReader(body))

	svc.ArchiveBatch(rec, req, "alice")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var got CloudArchiveResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Success || got.Message != "no valid tasks to archive" {
		t.Fatalf("response = %+v, want success=false message=%q", got, "no valid tasks to archive")
	}
	if got.SkippedCount != 1 || len(got.SkippedTasks) != 1 || got.SkippedTasks[0] != "t1" {
		t.Fatalf("skipped = %d %v, want 1 [t1]", got.SkippedCount, got.SkippedTasks)
	}
}
