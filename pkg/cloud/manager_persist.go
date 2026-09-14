// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_persist.go 是**持久化与恢复**：saveTask/saveGroup 落盘、recoverTasks/recoverGroups 启动恢复、
// diskUsageOfTask/reconcileReservedSize 用量核对、cleanupExpired* 过期清理、以及任务/组 ID 生成。//
// 拆分说明见 manager.go 顶部。

package cloud

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// saveTask 持久化单个任务到磁盘，返回写盘错误（供终态调用方显式处理）。
// 进度类保存（dirty flush）失败可容忍（调用方忽略返回值）；终态（completed/
// failed/cancelled）保存失败意味着重启后状态回滚，调用方必须记 Error 日志。
func (m *CloudDownloadManager) saveTask(t *CloudTask) error {
	// 检查任务是否已被删除（避免被删除后仍持久化）
	m.mu.RLock()
	_, exists := m.tasks[t.ID]
	m.mu.RUnlock()
	if !exists {
		m.logger.Debug("skip persisting deleted task", "id", t.ID)
		return nil
	}

	// 快照关键字段避免 data race（json.Marshal 期间任务可能被并发修改）
	m.mu.RLock()
	data, err := json.Marshal(t)
	m.mu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal task", "id", t.ID, "error", err)
		return err
	}
	persistDir := m.PersistDirFor(t.Owner)
	if persistDir == "" {
		m.logger.Warn("租户不可用，跳过任务持久化", "id", t.ID, "owner", t.Owner)
		return fmt.Errorf("tenant unavailable for task %s", t.ID)
	}
	if err := os.MkdirAll(persistDir, 0755); err != nil {
		m.logger.Warn("创建持久化目录失败", "dir", persistDir, "error", err)
		return err
	}
	taskFile := filepath.Join(persistDir, t.ID+".json")
	if err := os.WriteFile(taskFile, data, 0644); err != nil {
		m.logger.Warn("failed to persist task", "id", t.ID, "error", err)
		return err
	}
	return nil
}

// recoverTasks 从磁盘恢复所有任务（遍历所有租户的 meta/cloud/）。
// 仅重启 downloading 状态的任务（崩溃前正在下载中）。
// pending 任务不自动启动——避免 CreateTask 创建但未 SubmitAndStart 的任务在崩溃后意外启动。
func (m *CloudDownloadManager) recoverTasks() {
	recovered := 0
	restarted := 0
	for _, tenant := range m.listTenants() {
		persistDir := m.PersistDirFor(tenant)
		if persistDir == "" {
			continue
		}
		entries, err := os.ReadDir(persistDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(persistDir, e.Name()))
			if err != nil {
				m.logger.Warn("failed to read persisted task, skipping", "file", e.Name(), "error", err)
				continue
			}
			var task CloudTask
			if err := json.Unmarshal(data, &task); err != nil {
				m.logger.Warn("failed to unmarshal persisted task, skipping", "file", e.Name(), "error", err)
				continue
			}
			// 重启后以磁盘实际占用为基准重算 ReservedSize，
			// 与 StorageManager 启动扫描的计数器保持一致（不信任崩溃前持久化的占位值）
			m.reconcileReservedSize(&task)
			m.tasks[task.ID] = &task
			recovered++

			// 仅重启 downloading 状态的任务（崩溃前正在下载）。
			// pending 任务不自动启动——避免 CreateTask 创建但尚未 SubmitAndStart
			// 就崩溃导致意外启动的边界情况。
			if task.Status == "downloading" {
				m.logger.Info("restarting interrupted download", "task_id", task.ID, "url", task.URL)
				m.mu.Lock()
				m.running[task.ID] = true
				m.mu.Unlock()
				m.wg.Add(1)
				go m.executeDownload(context.Background(), &task)
				restarted++
			}
		}
	}
	if recovered > 0 {
		m.logger.Info("cloud download tasks recovered", "count", recovered, "restarted", restarted)
	}
}

// diskUsageOfTask 返回任务目录中所有普通文件的实际字节占用。
func (m *CloudDownloadManager) diskUsageOfTask(owner, taskID string) int64 {
	dir := m.TaskDirFor(owner, taskID)
	if dir == "" {
		return 0
	}
	var total int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}

// reconcileReservedSize 以任务目录实际占用为基准重算 ReservedSize。
// 进程重启后 StorageManager 的计数器来自磁盘扫描，这里让每个任务的预留量
// 与扫描结果一致，避免后续删除/清理时多退或少退。
func (m *CloudDownloadManager) reconcileReservedSize(task *CloudTask) {
	task.ReservedSize = m.diskUsageOfTask(task.Owner, task.ID)
}

// recoverGroups 从磁盘恢复任务组（遍历所有租户的 meta/cloud/groups/），并修剪已不存在的任务引用。
func (m *CloudDownloadManager) recoverGroups() {
	recovered := 0
	for _, tenant := range m.listTenants() {
		groupsDir := m.groupsDirFor(tenant)
		if groupsDir == "" {
			continue
		}
		entries, err := os.ReadDir(groupsDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(groupsDir, e.Name()))
			if err != nil {
				continue
			}
			var group CloudTaskGroup
			if err := json.Unmarshal(data, &group); err != nil {
				continue
			}
			group.TaskIDs = m.pruneGroupTaskIDs(group.TaskIDs)
			if len(group.TaskIDs) == 0 {
				_ = os.Remove(filepath.Join(groupsDir, e.Name()))
				continue
			}
			m.groupMu.Lock()
			m.groups[group.ID] = &group
			m.groupMu.Unlock()
			recovered++
		}
	}

	// 兼容旧数据：为带 GroupID 但缺少组记录的孤儿任务重建最小组
	var orphanGroups []*CloudTaskGroup
	m.mu.RLock()
	for _, t := range m.tasks {
		if t.GroupID == "" {
			continue
		}
		if _, ok := m.groups[t.GroupID]; ok {
			continue
		}
		group := &CloudTaskGroup{
			ID:         t.GroupID,
			Owner:      t.Owner, // 继承子任务 owner，避免带 owner 的组被重建为全局可见（组级隔离漏洞）
			Name:       t.GroupID,
			Status:     "pending",
			TaskIDs:    []string{t.ID},
			TotalTasks: 1,
			CreatedAt:  t.CreatedAt,
			UpdatedAt:  t.UpdatedAt,
			ExpiresAt:  t.ExpiresAt,
		}
		orphanGroups = append(orphanGroups, group)
	}
	m.mu.RUnlock()
	for _, group := range orphanGroups {
		m.groupMu.Lock()
		m.groups[group.ID] = group
		m.groupMu.Unlock()
		_ = m.saveGroup(group)
		recovered++
	}

	if recovered > 0 {
		m.logger.Info("cloud download groups recovered", "count", recovered)
	}
}

// pruneGroupTaskIDs 返回仍存在于 tasks 中的任务 ID 列表。
func (m *CloudDownloadManager) pruneGroupTaskIDs(ids []string) []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var kept []string
	for _, id := range ids {
		if _, ok := m.tasks[id]; ok {
			kept = append(kept, id)
		}
	}
	return kept
}

// groupsDirPath 返回 owner 租户的组持久化目录（<root>/<tenant>/meta/cloud/groups）。
func (m *CloudDownloadManager) groupsDirPath(owner string) string {
	return m.groupsDirFor(owner)
}

// saveGroup 持久化任务组到磁盘（按组 owner 落租户 meta/cloud/groups），返回写盘错误。
// 组状态（completed/partial/archive_file 等）持久化失败意味着重启后组元数据丢失，
// 调用方（组状态变更点）必须显式处理；清理类保存可忽略返回值。
func (m *CloudDownloadManager) saveGroup(g *CloudTaskGroup) error {
	// 串行化整个 marshal+write：并发调用 saveGroup 时，若某个持有旧快照的保存
	// 在更新的保存之后落盘，重启会恢复出陈旧组状态（进度/状态回退）。持锁期间
	// marshal 反映当时的在内存最新状态，写盘按获取锁的顺序落盘，最后写盘者必为
	// 最新状态触发的保存（所有组状态变更路径都调用 saveGroup）。
	m.groupSaveMu.Lock()
	defer m.groupSaveMu.Unlock()

	m.groupMu.RLock()
	data, err := json.Marshal(g)
	m.groupMu.RUnlock()
	if err != nil {
		m.logger.Warn("failed to marshal group", "id", g.ID, "error", err)
		return err
	}
	dir := m.groupsDirPath(g.Owner)
	if dir == "" {
		m.logger.Warn("租户不可用，跳过组持久化", "id", g.ID, "owner", g.Owner)
		return fmt.Errorf("tenant unavailable for group %s", g.ID)
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		m.logger.Warn("failed to create groups dir", "dir", dir, "error", err)
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, g.ID+".json"), data, 0644); err != nil {
		m.logger.Warn("failed to persist group", "id", g.ID, "error", err)
		return err
	}
	return nil
}

// removeGroupFile 删除 owner 租户下组的持久化文件。
func (m *CloudDownloadManager) removeGroupFile(groupID, owner string) {
	dir := m.groupsDirPath(owner)
	if dir == "" {
		return
	}
	_ = os.Remove(filepath.Join(dir, groupID+".json"))
}

// cleanupExpiredOnce 执行一次性的过期任务清理，返回清理的任务数量。
// 不包含循环，供测试直接调用。
//
// 注意：函数内部会先释放 m.mu 再执行 I/O 删除操作，调用者不应假设调用期间 mu 一直被持有。
func (m *CloudDownloadManager) cleanupExpiredOnce() int {
	now := time.Now()

	// 在锁内收集需要清理的 ID 及相关信息，避免锁内 I/O 阻塞
	type expiredItem struct {
		id             string
		taskID         string
		filename       string
		owner          string
		reservedSize   int64
		scopeCommitted int64
	}
	m.mu.Lock()
	var expired []expiredItem
	for id, t := range m.tasks {
		var ttl time.Duration
		switch t.Status {
		case "completed":
			ttl = m.config.TaskTTL
		case "failed", "cancelled":
			ttl = m.config.FailedTaskTTL
		default:
			continue
		}
		if now.After(t.UpdatedAt.Add(ttl)) {
			expired = append(expired, expiredItem{
				id:             id,
				taskID:         t.ID,
				filename:       t.Filename,
				owner:          t.Owner,
				reservedSize:   t.ReservedSize,
				scopeCommitted: t.QuotaCommitted,
			})
			t.ReservedSize = 0   // 释放后归零，防二次释放
			t.QuotaCommitted = 0 // 同上
			delete(m.tasks, id)
		}
	}
	m.mu.Unlock()

	if len(expired) == 0 {
		return 0
	}

	// 锁外执行 I/O 和 checksum 操作（按任务 owner 落租户桶）
	cleaned := 0
	for _, item := range expired {
		if persistDir := m.PersistDirFor(item.owner); persistDir != "" {
			_ = os.Remove(filepath.Join(persistDir, item.id+".json"))
		}
		m.removeTaskDir(item.owner, item.taskID)
		if item.reservedSize > 0 {
			m.storage.ReleaseCloud(item.reservedSize)
		}
		// P4 租户配额：终态任务的 Scope 占用随过期清理释放（QuotaCommitted ReleaseUsage）。
		if item.scopeCommitted > 0 {
			if scope := m.quotaScope(item.owner); scope != nil {
				scope.ReleaseUsage(item.scopeCommitted)
			}
		}
		if cs := m.checksumStoreFor(item.owner); cs != nil {
			relKey := filepath.ToSlash(filepath.Join("cloud", item.taskID, item.filename))
			cs.Delete(relKey)
		}
		cleaned++
	}

	// 清理引用已全部过期任务的空组（saveGroup 在锁外调用，避免 RLock 重入死锁）。
	// 锁序说明：此处 m.groupMu 下调用 pruneGroupTaskIDs（内部取 m.mu.RLock），
	// 嵌套顺序为 groupMu → mu；全代码库所有嵌套获取均遵循 groupMu → mu，
	// 无反向路径（m.mu → groupMu），因此不存在 ABBA 死锁。
	var toSave []*CloudTaskGroup
	m.groupMu.Lock()
	for gid, g := range m.groups {
		kept := m.pruneGroupTaskIDs(g.TaskIDs)
		if len(kept) == 0 {
			delete(m.groups, gid)
			m.removeGroupFile(gid, g.Owner)
		} else if len(kept) != len(g.TaskIDs) {
			g.TaskIDs = kept
			g.TotalTasks = len(kept)
			toSave = append(toSave, g)
		}
	}
	m.groupMu.Unlock()
	for _, g := range toSave {
		_ = m.saveGroup(g)
	}

	m.logger.Info("expired cloud download tasks cleaned up", "count", cleaned)
	return cleaned
}

// cleanupExpired 定期清理过期任务。
func (m *CloudDownloadManager) cleanupExpired() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.cleanupExpiredOnce()
		case <-m.stopCleanup:
			m.cleanupExpiredOnce() // 退出前清理一次
			return
		}
	}
}

func newGroupID() string {
	return newIDWithPrefix("group")
}

func newTaskID() string {
	return newIDWithPrefix("cloud")
}

func newIDWithPrefix(prefix string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	idCounter.mu.Lock()
	idCounter.n++
	n := idCounter.n
	idCounter.mu.Unlock()
	return fmt.Sprintf("%s-%s-%d", prefix, hex.EncodeToString(b), n)
}

var idCounter struct {
	mu sync.Mutex
	n  int64
}
