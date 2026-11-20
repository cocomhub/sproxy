// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cloud"
)

// TestRetryTasks_Handler 锁定 retry 端点语义（2026-11-21）：
// 按 transfer_path 选择重试、回填 retried、并记账业务审计动作 `cloud_task_retry`（R35-b 场景证据）。
func TestRetryTasks_Handler(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{retryIDs: []string{"t1"}}
	audit := &stubAudit{}
	svc := &Service{deps: Deps{Tasks: tasks, Audit: audit}}
	body := `{"transfer_path":"jav/x/1.mp4","force":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/retry", strings.NewReader(body))
	rec := httptest.NewRecorder()

	svc.RetryTasks(rec, req, "owner-1")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if tasks.retrySel.TransferPath != "jav/x/1.mp4" || tasks.retrySel.Owner != "owner-1" {
		t.Fatalf("选择器未按请求填充: %+v", tasks.retrySel)
	}
	if !tasks.retryForce {
		t.Fatal("force 未透传")
	}
	if !strings.Contains(rec.Body.String(), `"retried":["t1"]`) {
		t.Fatalf("响应缺少 retried: %s", rec.Body.String())
	}
	if len(audit.calls) != 1 || audit.calls[0].action != "cloud_task_retry" {
		t.Fatalf("必须记录 cloud_task_retry 审计，got %+v", audit.calls)
	}
}

// TestRetryTasks_RejectsEmptySelector 锁定「无条件重试全部任务」必须被拒绝（防误操作）。
func TestRetryTasks_RejectsEmptySelector(t *testing.T) {
	t.Parallel()
	tasks := &stubTasks{}
	svc := &Service{deps: Deps{Tasks: tasks, Audit: &stubAudit{}}}
	req := httptest.NewRequest(http.MethodPost, "/api/cloud/tasks/retry", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	svc.RetryTasks(rec, req, "o")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("空选择器应 400，got %d", rec.Code)
	}
}

// 编译期：选择器类型来自 pkg/cloud（避免调用侧自造同形结构）。
var _ = cloud.RetrySelector{}
