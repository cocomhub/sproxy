// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"fmt"
	"sort"
	"time"
)

// 重试结果动作（对齐任务 Results 的 action 值；本地定义避免依赖 pkg/sync）。
const (
	resultActionError        = "error"
	resultActionVerifyFailed = "verify_failed"
	// resultActionRetried 表示已发起重试但子任务结果未回填该文件条目（mock/异常场景兜底）。
	resultActionRetried = "retried"
)

// RetryItem 表示单个失败文件的重试结果（Path + 重试后动作/错误）。
type RetryItem struct {
	Path   string `json:"path"`
	Action string `json:"action"` // 重试后动作：updated/created/verify_failed/error/retried
	Error  string `json:"error,omitempty"`
}

// RetryResult 是 RetryFiles 的明细化响应：哪些重试了 / 哪些跳过（幂等跳过）。
type RetryResult struct {
	Retried []RetryItem `json:"retried"`
	Skipped []string    `json:"skipped,omitempty"` // 指定了但非失败/不存在的文件（幂等跳过）
}

// isFailedAction 报告结果 action 是否属于「可重试失败」：error（传输/写入失败）
// 或 verify_failed（校验核对不一致）。created/updated/skipped 等成功动作不可重试。
func isFailedAction(action string) bool {
	return action == resultActionError || action == resultActionVerifyFailed
}

// RetryFiles 对任务的失败文件发起单文件重试（roadmap 4.3 P1「失败可重试单个文件」）。
//
// 语义：
//   - files 为空 → 重试任务 Results 中全部失败（error/verify_failed）文件；
//     指定 files → 只重试其中属于失败清单的文件，其余（成功/不存在）幂等跳过。
//   - 重试实现：构造**一个**重试子任务（方向/remote/src/dst/冲突/校验策略与源任务一致，
//     Include = 待重试文件 glob 精确匹配，Recursive=false 仅限该文件），经 SubmitAndStart
//     复用引擎 diff+sync 单文件路径——**不重跑整个任务**（原任务状态不变）。
//   - 子任务完成后回写原任务 Results 对应条目（重试后动作），并返回明细。
//   - 无重试语义污染：原任务状态/进度不被重试改写（仅 Results 条目更新）。
//
// 跨 owner 任务返回 ErrNotFound（404 防枚举，与 Get/CancelTask 同语义）。
func (m *Manager) RetryFiles(ctx context.Context, id, owner string, files []string) (*RetryResult, error) {
	task := m.Get(id, owner)
	if task == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	// 收集失败文件（error/verify_failed），按 path 索引。
	failed := make(map[string]SyncFileResult)
	for _, r := range task.Results {
		if isFailedAction(r.Action) {
			failed[r.Path] = r
		}
	}
	if len(failed) == 0 {
		return &RetryResult{}, nil // 无失败文件：无重试、无跳过
	}

	// 决定重试清单：空 files = 全部失败；否则只取失败清单内的（其余跳过）。
	retryPaths := make([]string, 0, len(failed))
	var skipped []string
	if len(files) == 0 {
		for p := range failed {
			retryPaths = append(retryPaths, p)
		}
		sort.Strings(retryPaths)
	} else {
		seen := make(map[string]bool, len(files))
		for _, f := range files {
			if seen[f] {
				continue // 去重
			}
			seen[f] = true
			if _, ok := failed[f]; ok {
				retryPaths = append(retryPaths, f)
			} else {
				skipped = append(skipped, f)
			}
		}
	}
	if len(retryPaths) == 0 {
		return &RetryResult{Skipped: skipped}, nil
	}

	// 构造重试子任务：一次重试 = 一个子任务，Include 精确限定失败文件（引擎只 diff 这些文件）。
	sub, _, err := m.SubmitAndStart(CreateRequest{
		Direction:      task.Direction,
		Remote:         task.Remote,
		Src:            task.Src,
		Dst:            task.Dst,
		Recursive:      false, // 单文件重试：不递归（Include 已精确限定）
		Include:        retryPaths,
		ConflictPolicy: task.ConflictPolicy,
		DeletePolicy:   task.DeletePolicy,
		SyncEmptyDirs:  false,
		FollowSymlinks: task.FollowSymlinks,
		VerifyAfter:    task.VerifyAfter,
		Owner:          owner, // 重试归属与源任务一致（服务端派生；客户端不可伪造）
	})
	if err != nil {
		return nil, fmt.Errorf("创建重试子任务失败: %w", err)
	}
	m.logger.Info("sync task retry created", "task_id", task.ID, "retry_task_id", sub.ID,
		"files", len(retryPaths), "paths", retryPaths)

	// 等待子任务终态（有界超时，避免失败文件永久挂起）。
	final := m.waitTaskTerminal(ctx, sub.ID, 60*time.Second)

	// 回写原任务 Results：子任务结果含该 path → 更新条目；否则保留原条目（兜底 retried）。
	m.backfillRetryResults(task.ID, final)

	retried := make([]RetryItem, 0, len(retryPaths))
	byPath := make(map[string]SyncFileResult, len(final.Results))
	for _, r := range final.Results {
		byPath[r.Path] = r
	}
	for _, p := range retryPaths {
		if r, ok := byPath[p]; ok {
			retried = append(retried, RetryItem{Path: p, Action: r.Action, Error: r.Error})
		} else {
			retried = append(retried, RetryItem{Path: p, Action: resultActionRetried})
		}
	}
	return &RetryResult{Retried: retried, Skipped: skipped}, nil
}

// waitTaskTerminal 轮询任务直到终态（completed/failed/cancelled）或有界超时/ctx 取消。
// 返回最后一次观测的任务快照（找不到任务/超时以 failed 快照表达，供调用方诊断）。
func (m *Manager) waitTaskTerminal(ctx context.Context, id string, timeout time.Duration) *SyncTask {
	deadline := time.Now().Add(timeout)
	for {
		task := m.Get(id, "")
		if task == nil {
			return &SyncTask{ID: id, Status: StatusFailed, Error: "重试任务不存在（可能被删除）"}
		}
		switch task.Status {
		case StatusCompleted, StatusFailed, StatusCancelled:
			return task
		}
		if time.Now().After(deadline) {
			return &SyncTask{ID: id, Status: StatusFailed, Error: "等待重试任务终态超时"}
		}
		select {
		case <-ctx.Done():
			return &SyncTask{ID: id, Status: StatusFailed, Error: ctx.Err().Error()}
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// backfillRetryResults 把重试子任务的结果回写到原任务 Results 对应条目
// （仅更新匹配 path 的条目：重试后 action/error；原任务状态/进度不动）。
func (m *Manager) backfillRetryResults(taskID string, sub *SyncTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	stored, ok := m.tasks[taskID]
	if !ok {
		return // 原任务已被删除：不回写
	}
	byPath := make(map[string]SyncFileResult, len(sub.Results))
	for _, r := range sub.Results {
		byPath[r.Path] = r
	}
	updated := false
	for i := range stored.Results {
		if r, ok := byPath[stored.Results[i].Path]; ok {
			stored.Results[i].Action = r.Action
			stored.Results[i].Error = r.Error
			stored.Results[i].Checksum = r.Checksum
			updated = true
		}
	}
	if updated {
		stored.UpdatedAt = time.Now()
	}
}

// InjectResultsForTest 仅用于测试：覆写任务 Results 并置为 failed 状态（模拟真实执行
// 含失败文件后的回填形态）。生产路径不调用（Results 由执行器回填）。放本文件而非
// _test.go 是为让 pkg/server 的 handler 测试能经公开 API 预置失败任务。
func (m *Manager) InjectResultsForTest(id string, results []SyncFileResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tasks[id]; ok {
		t.Results = append([]SyncFileResult(nil), results...)
		t.Status = StatusFailed
		t.FilesTotal = int64(len(results))
		t.UpdatedAt = time.Now()
	}
}
