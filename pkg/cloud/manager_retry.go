// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"fmt"
	"strings"
)

// RetrySelector 是「按条件重试任务」的选择器（2026-11-21）。
//
// 设计目标：**不依赖任务存活期**也能重试——此前只有 `POST /tasks/{id}/resume`，
// 任务一旦过期清理就无法重试（实测：失败的修复任务 1h 后消失，用户完全无从下手）。
type RetrySelector struct {
	// TaskID 指定单任务。
	TaskID string
	// URL 按源 URL 匹配（支持前缀/包含匹配，便于粘贴同一分享下的多条）。
	URL string
	// TransferPath 按转存目标路径精确匹配（修复场景最常用：按卷内 rel 重试）。
	TransferPath string
	// GroupID 按任务组匹配（整组重试）。
	GroupID string
	// Owner 限定归属（空 = 不限制，仅内部/管理调用方使用）。
	Owner string
	// Limit 最多重试多少个（<=0 = 不限）。
	Limit int
	// IncludeCompletedDamaged 是否把「completed 但完整性 damaged」也纳入（重下自愈）。
	IncludeCompletedDamaged bool
}

// retryable 判断任务是否可被重试（终态里有救的那几种）。
func (m *CloudDownloadManager) retryable(t *CloudTask, sel RetrySelector) bool {
	if sel.Owner != "" && t.Owner != sel.Owner {
		return false
	}
	switch t.Status {
	case "failed", "cancelled", "pending":
	case "completed":
		if !sel.IncludeCompletedDamaged || t.IntegrityStatus != "damaged" {
			return false
		}
	default:
		return false
	}
	if sel.URL != "" && !strings.Contains(t.URL, sel.URL) {
		return false
	}
	if sel.TransferPath != "" {
		if t.Transfer == nil || t.Transfer.Path != sel.TransferPath {
			return false
		}
	}
	if sel.GroupID != "" && t.GroupID != sel.GroupID {
		return false
	}
	return true
}

// SelectRetryable 返回匹配的任务 ID 列表（稳定顺序：创建时间升序），不修改任何状态。
//
// 用途：`POST /api/cloud/tasks/retry` 与事项执行器共用的**唯一选择实现**。
func (m *CloudDownloadManager) SelectRetryable(sel RetrySelector) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, 8)
	if sel.TaskID != "" {
		if t, ok := m.tasks[sel.TaskID]; ok && m.retryable(t, sel) {
			out = append(out, t.ID)
		}
		return out
	}
	for _, t := range m.tasks {
		if m.retryable(t, sel) {
			out = append(out, t.ID)
			if sel.Limit > 0 && len(out) >= sel.Limit {
				break
			}
		}
	}
	return out
}

// RetryTasks 按选择器重试（复用 ResumeTask 内核，不新造下载路径）。
//
// 返回：已提交重试的任务 ID，以及每个任务的错误（成功为 nil）。
// 幂等：同任务在 running 时 ResumeTask 自身会拒绝/等待（既有语义），调用方可安全重复调用。
func (m *CloudDownloadManager) RetryTasks(sel RetrySelector, force bool) ([]string, map[string]error) {
	ids := m.SelectRetryable(sel)
	errs := make(map[string]error, len(ids))
	ok := make([]string, 0, len(ids))
	for _, id := range ids {
		if err := m.ResumeTask(id, force, sel.Owner); err != nil {
			errs[id] = fmt.Errorf("retry %s: %w", id, err)
			continue
		}
		ok = append(ok, id)
	}
	return ok, errs
}
