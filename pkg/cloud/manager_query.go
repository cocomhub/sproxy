// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// manager_query.go 是**任务查询与生命周期操作**：Get/Snapshot/List（按 owner 可见性过滤）、
// CancelTask（取消 + 释放预留）、DeleteTask（删除 + 清理磁盘与台账）。//
// 拆分说明见 manager.go 顶部。

package cloud

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (m *CloudDownloadManager) GetTask(id, owner string) (*CloudTask, bool) {
	return m.SnapshotTask(id, owner)
}

// SnapshotTask 返回任务的快照（副本），避免并发修改导致 data race。
// 按请求者 owner 过滤：跨 owner 任务返回 (nil, false)（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) SnapshotTask(id, owner string) (*CloudTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		return nil, false
	}
	c := *t
	// 快照对外不暴露运行时配额句柄（QW 由下载 goroutine 独占使用；读者无需也不应触碰）。
	c.account = nil
	return &c, true
}

// ListTasks 列出任务，支持按 status 过滤与 offset/limit 分页。
// offset<0 时不偏移；limit<=0 时返回全部（兼容现有语义）。
// 排序：CreatedAt 降序 + ID 降序 tie-break。ID 含随机 4 字节 hex，CreatedAt 相等时按
// 随机值排序，但排序本身确定（同输入同输出），分页跨页仍稳定——仅同纳秒创建的任务
// 顺序不代表创建序，属可接受（创建时间戳通常唯一）。
// total 为按 status 过滤后的任务总数（不受分页影响）。
// owner 非空时只返回匹配 owner 与空 owner（全局兼容）的任务；空 owner（管理员/未认证）返回全部。
// SnapshotTasks 按 ID 列表批量取任务快照：只返回**请求者可见**者（same ownerVisible 规则，
// IDOR 防护），并一次持锁遍历——与调用方逐条调用 SnapshotTask 相比不产生嵌套 RLock
// （Go 的 RWMutex 在写者等待时嵌套 RLock 会死锁），故批量场景必须走本方法。
// 快照同样不携带运行时配额句柄（与 SnapshotTask 一致）。
func (m *CloudDownloadManager) SnapshotTasks(ids []string, owner string) []*CloudTask {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*CloudTask
	for _, id := range ids {
		t, ok := m.tasks[id]
		if !ok || !ownerVisible(t.Owner, owner) {
			continue
		}
		c := *t
		c.account = nil
		out = append(out, &c)
	}
	return out
}

func (m *CloudDownloadManager) ListTasks(status string, offset, limit int, owner string) ([]*CloudTask, int) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var all []*CloudTask
	for _, t := range m.tasks {
		if (status == "" || t.Status == status) && ownerVisible(t.Owner, owner) {
			c := *t
			c.account = nil // 快照不暴露运行时配额句柄（同 SnapshotTask）
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
	// 防止 offset+limit 溢出（limit 极大如 MaxInt64 时偏移相加可能回绕为负，导致
	// all[offset:end] slice bounds panic）。先钳制 limit 到剩余条数再相加。
	end := offset + min(limit, total-offset)
	return all[offset:end], total
}

// CancelTask 取消正在进行的任务。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) CancelTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}
	if status := t.Status; status != "pending" && status != "downloading" {
		// 注意：status 须在解锁前捕获——fmt.Errorf 若直接引用 t.Status 会在 m.mu 释放后
		// 读共享字段，与 ResumeTask 的 task.Status = "pending"（持锁写）构成数据竞争。
		m.mu.Unlock()
		return fmt.Errorf("cannot cancel task in status %q", status)
	}
	t.Status = "cancelled"
	t.UpdatedAt = time.Now()
	t.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)

	// 释放实际预留的存储空间（ReservedSize 为准，释放后归零防二次释放）
	if t.ReservedSize > 0 {
		m.storage.ReleaseCloud(t.ReservedSize)
		t.ReservedSize = 0
	}
	// P4 租户配额：取消即放弃。但下载 goroutine 仍存活（running）时不得在此回拨——
	// 它还会继续 commit 字节，提前归零会被抬回（账本泄漏，CI run 34941359725 的
	// `cancel 后 cloud 桶 Usage()=200 want 0`）；此时把释放推迟到 goroutine 退出路径
	// （releaseAbandonedTaskScope，与清理 running 标记同一临界区，保证「goroutine 已停止
	// ⇒ 配额已归零」）。无运行 goroutine（pending 未启动 / goroutine 已退出）时不会有后续
	// commit，立即释放（releaseTaskScope 幂等）。
	if !m.running[id] {
		m.releaseTaskScope(t)
	}

	// 触发下载取消（排队中任务也已在 cancelFuncs 注册，可立即生效）
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}
	m.mu.Unlock()

	// 取消即放弃：清理任务文件（含 .partial/.partial.etag），使磁盘占用与已归零
	// 的存储账本一致——否则 partial 残留但账本释放，可累计突破 max_storage_bytes
	// 配额。goroutine 可能仍在响应 cancel 收尾（Windows 下删除被占用文件会失败），
	// 删除失败时由 executeDownload 的取消路径（removeTaskDir）兜底。
	m.removeTaskDir(t.Owner, id)

	// 终态持久化失败会丢失 cancelled 状态（重启后可能被当作 downloading 重启），必须显式报错
	if err := m.saveTask(t); err != nil {
		m.logger.Error("persist cancelled task state, state may be lost on restart",
			"task_id", id, "error", err)
	}
	m.metrics.TasksCancelled.Add(1)
	m.logger.Info("cloud download task cancelled", "task_id", id)
	return nil
}

// DeleteTask 删除任务及其云端文件。
// 按请求者 owner 过滤：跨 owner 任务返回 not found（404 防枚举，不泄露存在性）。
func (m *CloudDownloadManager) DeleteTask(id, owner string) error {
	m.mu.Lock()
	t, ok := m.tasks[id]
	if !ok || !ownerVisible(t.Owner, owner) {
		m.mu.Unlock()
		return fmt.Errorf("task not found: %s", id)
	}

	// 如果正在下载，先取消
	if cancel, ok := m.cancelFuncs[id]; ok {
		cancel()
		delete(m.cancelFuncs, id)
	}

	delete(m.tasks, id)

	// 释放实际预留的存储空间（ReservedSize 为准，释放后归零防二次释放）。
	// 必须在锁内释放：failTask 在持有 m.mu 期间读取 ReservedSize 并执行 I/O，
	// 若 DeleteTask 在锁外释放，failTask 可能读到已释放的旧值并再次释放（double release）。
	reserved := t.ReservedSize
	if reserved > 0 {
		t.ReservedSize = 0
	}
	// 锁内捕获 running：决定租户 Scope 释放可否立即执行（同 CancelTask：goroutine 仍
	// 写盘时推迟到其退出路径，否则后续 commit 会把归零的账本抬回）。
	running := m.running[id]
	// 锁内捕获 t.Status：Unlock 后读取会与下载 goroutine 的 failTask/CancelTask 持锁写构成数据竞争。
	delStatus := t.Status
	m.mu.Unlock()

	if reserved > 0 {
		m.storage.ReleaseCloud(reserved)
		m.logger.Debug("storage released", "task_id", id, "size", reserved)
	}
	// P4 租户配额：删除即放弃。goroutine 仍存活时由 releaseAbandonedTaskScope（任务已从
	// m.tasks 删除 ⇒ 判定为已放弃）在退出时回拨；否则立即回拨（含下载中边写边记字节）。
	if !running {
		m.releaseTaskScope(t)
	}

	m.logger.Info("deleting cloud download task", "task_id", id, "filename", t.Filename, "status", delStatus)

	// 删除云端文件（按任务 owner 落租户 cloud 桶）
	taskDir := m.TaskDirFor(t.Owner, t.ID)
	if taskDir != "" {
		filePath := filepath.Join(taskDir, t.Filename)
		if err := os.Remove(filePath); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove cloud file", "task_id", id, "path", filePath, "error", err)
		}
		if err := os.Remove(taskDir); err != nil && !os.IsNotExist(err) {
			// 目录可能非空（有其他文件），使用 RemoveAll
			if err := os.RemoveAll(taskDir); err != nil {
				m.logger.Warn("failed to remove task dir", "task_id", id, "path", taskDir, "error", err)
			}
		}
	}

	// 删除持久化文件（按任务 owner 落租户 meta/cloud）
	if persistDir := m.PersistDirFor(t.Owner); persistDir != "" {
		persistFile := filepath.Join(persistDir, t.ID+".json")
		if err := os.Remove(persistFile); err != nil && !os.IsNotExist(err) {
			m.logger.Warn("failed to remove persist file", "task_id", id, "error", err)
		}
	}

	// 清理 checksum（per-tenant store + 相对租户根 key，与写入端一致；ToSlash 归一见写端注释）
	if cs := m.checksumStoreFor(t.Owner); cs != nil {
		relKey := filepath.ToSlash(filepath.Join("cloud", t.ID, t.Filename))
		cs.Delete(relKey)
		m.logger.Debug("checksum deleted", "task_id", id, "rel", relKey)
	}

	m.logger.Info("cloud download task deleted and cleaned up", "task_id", id)
	return nil
}
