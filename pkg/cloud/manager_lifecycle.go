// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_lifecycle.go 是**运行时生命周期**：waitTaskStopped（等下载 goroutine 退出）、
// ResumeTask/ResumeGroup（断点续传入口）、markDirty/flushLoop/flushDirty（脏任务延迟落盘）、
// UpdateGroupStatus、以及 Close（停服收口）。//
// 拆分说明见 manager.go 顶部。

package cloud

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// waitTaskStopped 等待任务的下载 goroutine 完全退出（running 标记清除）。
// 最多等待 timeout。返回 false 表示超时仍未退出。
// 调用方不得持有 m.mu（本函数内部需取读锁）。
func (m *CloudDownloadManager) waitTaskStopped(taskID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		m.mu.RLock()
		running := m.running[taskID]
		m.mu.RUnlock()
		if !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ResumeTask 恢复失败的下载任务。
// force=true 时删除已有部分文件重新下载；force=false 时保留 .partial 由下载器
// 通过 Range 续传（不再改名成 destPath，避免续传退化为全量下载）。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举）。
func (m *CloudDownloadManager) ResumeTask(taskID string, force bool, owner string) error {
	m.mu.Lock()
	task, ok := m.tasks[taskID]
	if !ok || !ownerVisible(task.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	if status := task.Status; status != "failed" && status != "cancelled" {
		// 解锁前捕获 status：fmt.Errorf 若直接引用 task.Status 会在 m.mu 释放后读共享字段，
		// 与 CancelTask 的 t.Status = "cancelled"（持锁写）构成数据竞争。
		m.mu.Unlock()
		return fmt.Errorf("task %s is in status %q, only failed/cancelled tasks can be resumed", taskID, status)
	}
	// 释放写锁再等待：waitTaskStopped 内部需取读锁，持有写锁会死锁。
	// 等待期间任务可能被删除或状态被并发修改，之后会重新校验。
	m.mu.Unlock()

	// cancelled 状态由 CancelTask 提前写入，不代表旧 goroutine 已停止写盘：
	// 等待其完全退出（running 标记清除），避免新旧 goroutine 并发 append 同一
	// .partial 文件导致损坏。failed 状态由旧 goroutine 自身在写盘结束后写入，
	// running 此时已清除，本等待立即返回。
	if !m.waitTaskStopped(taskID, m.config.IdleTimeout+5*time.Second) {
		return fmt.Errorf("task %s is still finishing previous download, try again later", taskID)
	}

	m.mu.Lock()
	task, ok = m.tasks[taskID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", taskID)
	}
	if status := task.Status; status != "failed" && status != "cancelled" {
		m.mu.Unlock()
		return fmt.Errorf("task %s is in status %q, only failed/cancelled tasks can be resumed", taskID, status)
	}
	if m.running[taskID] {
		m.mu.Unlock()
		return fmt.Errorf("task %s is still running, cannot resume now", taskID)
	}

	// 状态先切 pending + running 同步置位：并发双 resume 中第二个会因 running 已置
	// 被上面的检查拦截，避免两个 goroutine 并发写同一 .partial（Critical 修复）。
	// UpdatedAt 在 running 置位后更新（值语义）：waitTaskStopped 以 running 为终态信号，
	// 先置 running 保证 watcher 一旦观察到即可视为本次 resume 已生效；UpdatedAt 紧随写入。
	task.Status = "pending"
	m.running[taskID] = true
	task.Error = ""
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.TaskTTL)

	// 释放过存储的任务需要重新占位（全局 storageMgr；Scope 侧由下载流 QuotaWriter 边写边记重建）
	if task.ReservedSize == 0 {
		if err := m.storage.TryReserveCloud(cloudReservePlaceholder); err != nil {
			// 占位失败：撤销 pending 切换并清除 running，避免 running 残留
			// 永久阻止后续 resume（goroutine 从未启动）。
			task.Status = "failed"
			task.Error = "storage full, cannot resume"
			delete(m.running, taskID)
			m.mu.Unlock()
			if saveErr := m.saveTask(task); saveErr != nil {
				m.logger.Error("persist resume-failure task state, state may be lost on restart",
					"task_id", taskID, "error", saveErr)
			}
			return err
		}
		task.ReservedSize = cloudReservePlaceholder
	}
	m.mu.Unlock()

	taskDir := m.TaskDirFor(task.Owner, task.ID)
	if taskDir == "" {
		return fmt.Errorf("tenant unavailable for task %s", taskID)
	}
	destPath := filepath.Join(taskDir, task.Filename)
	if force {
		_ = os.Remove(destPath)
		_ = os.Remove(destPath + ".partial")
		_ = os.Remove(destPath + ".partial.etag")
	}

	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist resumed task state, state may be lost on restart",
			"task_id", taskID, "error", err)
	}

	m.wg.Add(1)
	go m.executeDownload(context.Background(), task)
	return nil
}

// ResumeGroup 恢复组内所有失败/取消任务。
// 按请求者 owner 过滤：跨 owner 组返回 not found（404 防枚举）。
func (m *CloudDownloadManager) ResumeGroup(groupID string, force bool, owner string) error {
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	m.groupMu.RUnlock()
	if !ok || !ownerVisible(group.Owner, owner) {
		return fmt.Errorf("group not found: %s", groupID)
	}

	var errs []error
	for _, tid := range group.TaskIDs {
		if err := m.ResumeTask(tid, force, owner); err != nil {
			// 组内任务被单独删除后，组级恢复不应因此报错（与 CancelGroup/DeleteGroup 一致，
			// 否则整个组 resume 会误返回 404 让用户以为组不存在）。
			if strings.Contains(err.Error(), "task not found") {
				continue
			}
			errs = append(errs, err)
		}
	}
	m.UpdateGroupStatus(groupID)
	return errors.Join(errs...)
}

// SetGroupArchiveFile 记录组的归档文件路径并持久化。
func (m *CloudDownloadManager) SetGroupArchiveFile(groupID, archiveFile string) {
	m.groupMu.Lock()
	g, ok := m.groups[groupID]
	if !ok {
		m.groupMu.Unlock()
		return
	}
	g.ArchiveFile = archiveFile
	g.UpdatedAt = time.Now()
	m.groupMu.Unlock()
	if err := m.saveGroup(g); err != nil {
		m.logger.Error("persist group archive_file failed, group state may be lost on restart",
			"group_id", groupID, "error", err)
	}
}

// markDirty 将任务标记为"脏"（进度已更新），由 flushLoop 批量持久化。
func (m *CloudDownloadManager) markDirty(id string) {
	m.dirtyMu.Lock()
	m.dirtyTasks[id] = struct{}{}
	m.dirtyMu.Unlock()
}

// flushLoop 每 30 秒批量持久化脏任务的进度更新。
func (m *CloudDownloadManager) flushLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			m.flushDirty()
		case <-m.flushNow:
			m.flushDirty()
		case <-m.stopFlush:
			m.flushDirty()
			return
		}
	}
}

// flushDirty 将所有脏任务的当前状态持久化到磁盘。
func (m *CloudDownloadManager) flushDirty() {
	m.dirtyMu.Lock()
	ids := make([]string, 0, len(m.dirtyTasks))
	for id := range m.dirtyTasks {
		ids = append(ids, id)
	}
	m.dirtyTasks = make(map[string]struct{})
	m.dirtyMu.Unlock()

	if len(ids) == 0 {
		return
	}

	for _, id := range ids {
		m.mu.RLock()
		task, ok := m.tasks[id]
		m.mu.RUnlock()
		if !ok {
			continue
		}
		// 进度类批量持久化 best-effort（saveTask 内部已 Warn），失败不阻塞
		_ = m.saveTask(task)
	}
}

// UpdateGroupStatus 根据子任务状态更新组状态（导出方法，供 handler 调用）。
func (m *CloudDownloadManager) UpdateGroupStatus(groupID string) {
	// 在 groupMu 下取 TaskIDs 局部副本，避免与 cleanupExpiredOnce 的写入
	// 形成跨锁竞争（groupMu.Lock vs m.mu.RLock 无 happens-before）。
	m.groupMu.RLock()
	group, ok := m.groups[groupID]
	var taskIDs []string
	if ok {
		taskIDs = append([]string(nil), group.TaskIDs...)
	}
	m.groupMu.RUnlock()
	if !ok {
		return
	}

	m.mu.RLock()
	completed, failed, cancelled, active, pending := 0, 0, 0, 0, 0
	for _, tid := range taskIDs {
		task, exists := m.tasks[tid]
		if !exists {
			continue
		}
		switch task.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		case "downloading":
			active++
		default:
			pending++
		}
	}
	total := completed + failed + cancelled + active + pending
	m.mu.RUnlock()

	m.groupMu.Lock()
	changed := group.Completed != completed ||
		group.Failed != failed ||
		group.Cancelled != cancelled ||
		group.TotalTasks != total
	var newStatus string
	// 只要还有未终止的任务（downloading 或 pending），组状态为 downloading。
	// 一旦所有子任务进入终态，按 failed/cancelled/completed 优先级判定。
	switch {
	case total == 0:
		// 所有子任务已删除，组已完成其生命周期
		newStatus = "completed"
	case active > 0 || pending > 0:
		newStatus = "downloading"
	case failed > 0:
		newStatus = "failed"
	case cancelled > 0:
		newStatus = "cancelled"
	default:
		newStatus = "completed"
	}
	if newStatus != group.Status {
		group.Status = newStatus
		changed = true
	}
	if !changed {
		// Web UI 轮询会高频调用本方法，状态与计数未变化时跳过落盘
		m.groupMu.Unlock()
		return
	}
	group.Completed = completed
	group.Failed = failed
	group.Cancelled = cancelled
	group.TotalTasks = total
	group.UpdatedAt = time.Now()
	m.groupMu.Unlock()
	// 组状态变更持久化失败会导致重启后组进度回退，必须显式报错
	if err := m.saveGroup(group); err != nil {
		m.logger.Error("persist group status failed, group state may be lost on restart",
			"group_id", groupID, "error", err)
	}
}

// Close 停止所有后台 goroutine（flushLoop 和 cleanupExpired）并等待下载完成。
// 在进程退出前应调用一次。多次调用安全。
// 注意：优雅关闭不取消进行中的下载任务——下载 goroutine 在进程退出时自然终止，
// .partial 文件保留，重启后通过 recoverTasks 恢复并通过 Range 续传继续。
// wg.Wait 最多等待 30 秒，超时后返回（防止下载 goroutine 卡在 I/O 上永久阻塞）。
func (m *CloudDownloadManager) Close() {
	m.closeOnce.Do(func() {
		close(m.stopFlush)
		close(m.stopCleanup)

		// 带超时的 Wait，防止下载 goroutine 卡在 I/O 上永久阻塞
		done := make(chan struct{})
		go func() {
			m.wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			m.logger.Warn("cloud download manager Close timed out waiting for goroutines")
		}
	})
}

// FlushNow 立即触发一次批量持久化（测试用）。
func (m *CloudDownloadManager) FlushNow() {
	m.flushDirty()
}
