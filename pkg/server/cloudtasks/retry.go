// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloudtasks

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cloud"
)

// RetryTasks 处理 POST /api/cloud/tasks/retry（2026-11-21）。
//
// 为什么需要：此前只有 `POST /tasks/{id}/resume`，且失败任务默认 1h 即清理 ⇒ 用户
// 「看不到任务、也无法重试」。本入口支持按 **task_id / url / transfer_path / group_id**
// 选择待重试任务（含"已消失"历史任务只要仍在保留期内），复用 ResumeTask 内核。
//
// 语义：
//   - 选择器至少给一个；全空 ⇒ 400（避免误重试全部任务）。
//   - 返回 `retried[]`（已提交）与 `errors{}`（逐任务失败原因），**部分成功不视为整体失败**。
//   - 幂等：同任务运行中重复调用由 ResumeTask 自身拒绝（错误回填到 errors）。
func (s *Service) RetryTasks(w http.ResponseWriter, r *http.Request, owner string) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	var req struct {
		TaskID                  string `json:"task_id"`
		URL                     string `json:"url"`
		TransferPath            string `json:"transfer_path"`
		GroupID                 string `json:"group_id"`
		Force                   bool   `json:"force"`
		Limit                   int    `json:"limit"`
		IncludeCompletedDamaged bool   `json:"include_completed_damaged"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSON(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	if err := drainBody(r); err != nil {
		s.writeJSON(w, map[string]string{"error": msgBadRequest}, http.StatusBadRequest)
		return
	}
	if req.TaskID == "" && req.URL == "" && req.TransferPath == "" && req.GroupID == "" {
		s.writeJSON(w, map[string]string{
			"error": "至少提供 task_id / url / transfer_path / group_id 之一（拒绝无条件重试全部任务）",
		}, http.StatusBadRequest)
		return
	}

	sel := cloud.RetrySelector{
		TaskID:                  req.TaskID,
		URL:                     req.URL,
		TransferPath:            req.TransferPath,
		GroupID:                 req.GroupID,
		Owner:                   owner,
		Limit:                   req.Limit,
		IncludeCompletedDamaged: req.IncludeCompletedDamaged,
	}
	retried, errs := s.deps.Tasks.RetryTasks(sel, req.Force)
	if len(errs) > 0 {
		s.deps.Audit.Record(r.Context(), "cloud_task_retry", "task", strings.Join(retried, ","),
			auditResultError, fmt.Sprintf("%d 个任务重试失败", len(errs)))
	} else {
		s.deps.Audit.Record(r.Context(), "cloud_task_retry", "task", strings.Join(retried, ","),
			auditResultSuccess, "")
	}
	jsonErrs := make(map[string]string, len(errs))
	for id, err := range errs {
		jsonErrs[id] = err.Error()
	}
	status := http.StatusOK
	if len(retried) == 0 && len(jsonErrs) > 0 {
		status = http.StatusConflict // 选中了但全部失败（例如仍在运行）
	}
	s.writeJSON(w, map[string]any{
		"success": len(retried) > 0,
		"retried": retried,
		"errors":  jsonErrs,
	}, status)
}
