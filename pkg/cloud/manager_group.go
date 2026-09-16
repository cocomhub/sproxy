// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_group.go 是**任务组**：创建（含文件名冲突校验与子任务批量创建）、批量启动、查询、
// 取消与删除。//
// 拆分说明见 manager.go 顶部。

package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// CreateGroup 创建下载任务组。
// owner 是请求认证派生的组归属，子任务写入同 owner（组级多租户隔离）。
// 校验文件名冲突，创建子任务。
func (m *CloudDownloadManager) CreateGroup(name string, urls []cloudfilename.Entry, owner string) (*CloudTaskGroup, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("at least one URL is required")
	}

	// 校验文件名冲突
	filenameSet := make(map[string]int)
	for _, entry := range urls {
		fn, err := cloudfilename.ResolveFilename(entry)
		if err != nil {
			return nil, fmt.Errorf("invalid filename for %s: %w", entry.URL, err)
		}
		filenameSet[fn]++
	}
	var conflicts []string
	for fn, count := range filenameSet {
		if count > 1 {
			conflicts = append(conflicts, fn)
		}
	}
	if len(conflicts) > 0 {
		return nil, fmt.Errorf("filename conflicts detected: %s; please specify unique filenames via request", strings.Join(conflicts, ", "))
	}

	groupID := newGroupID()
	now := time.Now()

	group := &CloudTaskGroup{
		ID:         groupID,
		Owner:      owner,
		Name:       name,
		Status:     "pending",
		TotalTasks: len(urls),
		CreatedAt:  now,
		UpdatedAt:  now,
		ExpiresAt:  now.Add(m.config.TaskTTL),
	}

	var taskIDs []string
	var newTaskIDs []string  // 本次新建的任务（回滚时删除）
	var absorbedIDs []string // 去重吸收的既有任务（回滚时清除组归属但不删除）
	seen := make(map[string]bool, len(urls))
	// rollback 在循环中途失败时清理"本次新建"的任务与存储预留，防止泄漏 pending 任务。
	// 去重吸收的既有独立任务不属于本组创建，回滚时不得删除（否则误删用户已有下载）。
	rollback := func() {
		// 先清除被吸收任务的组归属，避免悬挂引用到不存在的组
		for _, id := range absorbedIDs {
			var snap *CloudTask
			m.mu.Lock()
			t, ok := m.tasks[id]
			if ok {
				t.GroupID = ""
				c := *t
				c.qw = nil // 内部拷贝不携带运行时配额句柄（saveTask json 亦不含）
				snap = &c
			}
			m.mu.Unlock()
			if ok {
				_ = m.saveTask(snap)
			}
		}
		for _, newTaskID := range slices.Backward(newTaskIDs) {
			if err := m.DeleteTask(newTaskID, owner); err != nil {
				m.logger.Warn("failed to rollback group task", "task_id", newTaskID, "error", err)
			}
		}
	}
	for _, entry := range urls {
		fn, err := cloudfilename.ResolveFilename(entry)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("invalid filename for %s: %w", entry.URL, err)
		}
		// 该 URL 已有**对请求者可见**的活跃任务 → 本次是去重吸收既有任务，回滚时不删除。
		// 跨 owner 的同 URL 任务不可见，不吸收（各自独立下载，防组归属性混乱）。
		absorbed := m.findByURL(entry.URL, owner) != nil

		task, err := m.CreateTask("url", entry.URL, fn, -1, owner)
		if err != nil {
			rollback()
			return nil, fmt.Errorf("create task for %s: %w", entry.URL, err)
		}
		// 更新存储任务（而非 CreateTask 返回的副本）的 GroupID。
		// CreateTask 去重命中时返回的是快照副本，只改副本会导致内存与磁盘不一致。
		m.mu.Lock()
		stored, ok := m.tasks[task.ID]
		if !ok {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("task disappeared during group creation: %s", task.ID)
		}
		// 去重命中：同组重复 URL 或已属其他组的活跃任务不允许重复入组
		if stored.GroupID != "" && stored.GroupID != groupID {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("duplicate URL %s already belongs to group %s", entry.URL, stored.GroupID)
		}
		if seen[stored.ID] {
			m.mu.Unlock()
			rollback()
			return nil, fmt.Errorf("duplicate URL in group: %s", entry.URL)
		}
		stored.GroupID = groupID
		m.mu.Unlock()
		_ = m.saveTask(stored)
		taskIDs = append(taskIDs, stored.ID)
		if absorbed {
			absorbedIDs = append(absorbedIDs, stored.ID)
		} else {
			newTaskIDs = append(newTaskIDs, stored.ID)
		}
		seen[stored.ID] = true
	}

	group.TaskIDs = taskIDs

	m.groupMu.Lock()
	m.groups[groupID] = group
	m.groupMu.Unlock()
	if err := m.saveGroup(group); err != nil {
		m.logger.Error("persist new group failed, group may be lost on restart",
			"group_id", groupID, "error", err)
	}

	m.logger.Info("cloud download group created",
		"group_id", groupID,
		"name", name,
		"task_count", len(urls),
	)
	return group, nil
}

// SubmitAndStartGroup 创建组并启动所有子任务下载。
func (m *CloudDownloadManager) SubmitAndStartGroup(name string, urls []cloudfilename.Entry, owner string) (*CloudTaskGroup, error) {
	group, err := m.CreateGroup(name, urls, owner)
	if err != nil {
		return nil, err
	}

	for _, taskID := range group.TaskIDs {
		// 在写锁内检查 Status + 同步置位 running，闭合"检查→启动"竞态窗口：
		// 并发 SubmitAndStartGroup 对同一任务会有一个拿到 running 后另一个跳过，
		// 避免两个 goroutine 并发写同一 .partial。已在 running 的任务（可能是
		// 去重命中的既有任务）跳过启动。
		m.mu.Lock()
		task, exists := m.tasks[taskID]
		if exists && task.Status == "pending" && !m.running[taskID] {
			m.running[taskID] = true
		} else {
			task = nil
		}
		m.mu.Unlock()
		if task == nil {
			continue
		}
		m.wg.Add(1)
		go m.executeDownload(context.Background(), task)
	}

	m.UpdateGroupStatus(group.ID)
	return group, nil
}

// GetGroup 获取组详情，按请求者 owner 过滤（跨 owner 组视为不存在，404 防枚举）。
func (m *CloudDownloadManager) GetGroup(id, owner string) (*CloudTaskGroup, bool) {
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()
	g, ok := m.groups[id]
	if !ok || !ownerVisible(g.Owner, owner) {
		return nil, false
	}
	c := *g
	return &c, true
}

// ListGroups 列出组，支持按 status 过滤与 offset/limit 分页。
// offset<0 时不偏移；limit<=0 时返回全部（兼容现有语义）。
// 排序：CreatedAt 降序 + ID 降序 tie-break（同 ListTasks 注释，确定性排序）。
// total 为按 status 过滤后的组总数（不受分页影响）。
// owner 非空时只返回匹配 owner 与空 owner（全局兼容）的组；空 owner（管理员/未认证）返回全部。
func (m *CloudDownloadManager) ListGroups(status string, offset, limit int, owner string) ([]*CloudTaskGroup, int) {
	m.groupMu.RLock()
	defer m.groupMu.RUnlock()

	var all []*CloudTaskGroup
	for _, g := range m.groups {
		if (status == "" || g.Status == status) && ownerVisible(g.Owner, owner) {
			c := *g
			all = append(all, &c)
		}
	}
	// CreatedAt 降序，ID 降序 tie-break（保持稳定排序）
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].CreatedAt.Equal(all[j].CreatedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].CreatedAt.After(all[j].CreatedAt)
	})
	total := len(all)
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		return all, total
	}
	if offset >= total {
		return nil, total
	}
	// 防止 offset+limit 溢出（同 ListTasks）
	end := offset + min(limit, total-offset)
	return all[offset:end], total
}

// CancelGroup 取消组内所有 pending/downloading 任务（已完成任务跳过）。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) CancelGroup(groupID, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.CancelTask(tid, owner); err != nil {
			// 已完成/已失败任务不可取消、任务已被单独删除，均不视为组取消失败
			if strings.Contains(err.Error(), "cannot cancel") || strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}
	m.UpdateGroupStatus(groupID)
	return errors.Join(errs...)
}

// DeleteGroup 删除组记录及所有子任务。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) DeleteGroup(groupID, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.DeleteTask(tid, owner); err != nil {
			// 组内任务被单独删除后，组级删除不应因此报错
			if strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}

	m.groupMu.Lock()
	delete(m.groups, groupID)
	m.groupMu.Unlock()
	m.removeGroupFile(groupID, group.Owner)

	return errors.Join(errs...)
}
