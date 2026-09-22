// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"fmt"
	"time"
)

// createFanoutTask 实现多节点扇出：对每个 remote 创建一个独立子任务
// （复用单任务 CreateTask 语义：去重/预留/持久化），并建一个父任务聚合视图。
//
// 语义：
//   - 每个子任务独立状态/进度/结果（各自走 CreateTask + SubmitAndStart 路径）；
//   - 单目标失败不阻塞其它（子任务独立执行，无共享失败传播）；
//   - 父任务 Remote 为空 + FanoutRemote 空，子任务 FanoutParentID 回指父任务；
//   - 失败节点独立重试：RetryRemote 按 remote 只重试失败子任务。
//
// 返回父任务（聚合视图载体）。
func (m *Manager) createFanoutTask(req CreateRequest) (*SyncTask, bool, error) {
	// 校验每个 remote（复用单值校验逻辑，逐 remote 执行）。
	for _, r := range req.Remotes {
		sub := req
		sub.Remote = r
		sub.Remotes = nil
		if err := m.validateCreateRequest(&sub); err != nil {
			return nil, false, fmt.Errorf("remote %q: %w", r, err)
		}
	}

	// 父任务（聚合视图：Remote 空，Fanout 语义由子任务承载）。
	parent := &SyncTask{
		ID:           newSyncTaskID(),
		Owner:        req.Owner,
		Direction:    req.Direction,
		Remote:       "", // 父任务无单一 remote（聚合视图）
		Src:          req.Src,
		Dst:          req.Dst,
		Recursive:    req.Recursive,
		Include:      append([]string(nil), req.Include...),
		Exclude:      append([]string(nil), req.Exclude...),
		Status:       StatusPending,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
		ExpiresAt:    time.Now().Add(m.config.TaskTTL),
		FanoutRemote: "",
	}
	m.mu.Lock()
	m.tasks[parent.ID] = parent
	m.mu.Unlock()
	_ = m.saveTask(parent)

	// 逐 remote 建子任务（独立执行；失败独立）。
	var created []string
	for _, r := range req.Remotes {
		sub := req
		sub.Remote = r
		sub.Remotes = nil
		child, isNew, err := m.CreateTask(sub)
		if err != nil {
			m.logger.Warn("扇出子任务创建失败", "remote", r, "error", err)
			continue // 单目标失败不阻塞其它
		}
		// 回写父引用（子任务 FanoutParentID + 父任务登记）。
		m.mu.Lock()
		if stored, ok := m.tasks[child.ID]; ok {
			stored.FanoutParentID = parent.ID
		}
		m.mu.Unlock()
		_ = m.saveTask(child)
		if isNew {
			created = append(created, r)
		}
	}

	m.logger.Info("sync fanout task created",
		"parent_id", parent.ID,
		"direction", req.Direction,
		"remotes", len(created),
		"src", req.Src,
	)
	return parent, true, nil
}

// FanoutChildren 返回父任务的全部子任务（按 remote 名）。
func (m *Manager) FanoutChildren(parentID string) []*SyncTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*SyncTask
	for _, t := range m.tasks {
		if t.FanoutParentID == parentID {
			out = append(out, t)
		}
	}
	return out
}

// GetFanoutChild 返回父任务的指定 remote 子任务。
func (m *Manager) GetFanoutChild(parentID, remote string) *SyncTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, t := range m.tasks {
		if t.FanoutParentID == parentID && t.Remote == remote {
			return t
		}
	}
	return nil
}

// FanoutChildStatus 返回指定 remote 子任务状态（不存在返回 ""）。
func (m *Manager) FanoutChildStatus(parentID, remote string) string {
	c := m.GetFanoutChild(parentID, remote)
	if c == nil {
		return ""
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.tasks[c.ID].Status
}

// FailFanoutChildForTest 测试注入：把指定 remote 子任务置为 failed（测试单目标失败独立语义）。
func (m *Manager) FailFanoutChildForTest(parentID, remote, errMsg string) error {
	c := m.GetFanoutChild(parentID, remote)
	if c == nil {
		return fmt.Errorf("子任务不存在: %s", remote)
	}
	m.mu.Lock()
	if stored, ok := m.tasks[c.ID]; ok {
		stored.Status = StatusFailed
		stored.Error = errMsg
		stored.UpdatedAt = time.Now()
	}
	m.mu.Unlock()
	_ = m.saveTask(c)
	return nil
}

// RetryRemote 重试指定 remote 的失败子任务（失败节点独立重试；复用 SubmitAndStart 排队执行）。
// 返回 RetryResult（是否重试 + 跳过原因）。只重试 failed/cancelled 子任务；活跃/成功跳过。
type RetryRemoteResult struct {
	Retried bool   `json:"retried"`
	Remote  string `json:"remote"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

func (m *Manager) RetryRemote(ctx context.Context, parentID, owner, remote string) (*RetryRemoteResult, error) {
	c := m.GetFanoutChild(parentID, remote)
	if c == nil {
		return nil, fmt.Errorf("%w: fanout child remote %s", ErrNotFound, remote)
	}
	if !ownerVisible(c.Owner, owner) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, parentID)
	}
	m.mu.RLock()
	st := m.tasks[c.ID].Status
	m.mu.RUnlock()
	if st != StatusFailed && st != StatusCancelled {
		return &RetryRemoteResult{Retried: false, Remote: remote, Status: st}, nil
	}
	// 重置子任务状态为 pending（清 Error），再 SubmitAndStart 重排队（executeSync 复查
	// 要求 pending/syncing/retrying；failed 会被跳过）。
	m.mu.Lock()
	if stored, ok := m.tasks[c.ID]; ok {
		stored.Status = StatusPending
		stored.Error = ""
		stored.UpdatedAt = time.Now()
	}
	m.mu.Unlock()
	_ = m.saveTask(c)
	// 复用 SubmitAndStart 重排队。
	_, _, err := m.SubmitAndStart(CreateRequest{
		Direction:      c.Direction,
		Remote:         c.Remote,
		Src:            c.Src,
		Dst:            c.Dst,
		Recursive:      c.Recursive,
		Include:        c.Include,
		Exclude:        c.Exclude,
		ConflictPolicy: c.ConflictPolicy,
		DeletePolicy:   c.DeletePolicy,
		SyncEmptyDirs:  c.SyncEmptyDirs,
		FollowSymlinks: c.FollowSymlinks,
		VerifyAfter:    c.VerifyAfter,
		Owner:          c.Owner,
	})
	if err != nil {
		return nil, fmt.Errorf("重试子任务失败: %w", err)
	}
	return &RetryRemoteResult{Retried: true, Remote: remote, Status: StatusPending}, nil
}

// FanoutSummary 返回父任务的扇出汇总（每 remote 子任务状态）。
type FanoutSummary struct {
	ParentID  string             `json:"parent_id"`
	Direction string             `json:"direction"`
	Total     int                `json:"total"`
	Completed int                `json:"completed"`
	Failed    int                `json:"failed"`
	Pending   int                `json:"pending"`
	Children  []FanoutChildState `json:"children"`
}

type FanoutChildState struct {
	Remote string `json:"remote"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// FanoutSummaryOf 聚合父任务子任务状态（供 GET /api/sync/tasks/{id} 返回）。
func (m *Manager) FanoutSummaryOf(parentID string) *FanoutSummary {
	children := m.FanoutChildren(parentID)
	sum := &FanoutSummary{ParentID: parentID, Total: len(children)}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, c := range children {
		stored := m.tasks[c.ID]
		st := StatusFailed
		errMsg := ""
		if stored != nil {
			st = stored.Status
			errMsg = stored.Error
		}
		sum.Children = append(sum.Children, FanoutChildState{Remote: c.Remote, Status: st, Error: errMsg})
		switch st {
		case StatusCompleted:
			sum.Completed++
		case StatusFailed, StatusCancelled:
			sum.Failed++
		default:
			sum.Pending++
		}
	}
	// 方向从父任务取。
	if p, ok := m.tasks[parentID]; ok {
		sum.Direction = p.Direction
	}
	return sum
}
