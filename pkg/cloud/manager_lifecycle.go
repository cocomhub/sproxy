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

// rollbackResumeLocked 回滚一次**未能启动 goroutine** 的 resume（调用方必须已持有 m.mu 写锁）。
//
// 回滚四件事，缺一件就会留下永久遗留：
//  1. **running 标记**：CancelTask/DeleteTask 见到 running 为真会把租户 Scope 的释放推迟到
//     「goroutine 退出路径」（releaseAbandonedTaskScope），而本次 resume 不会启动 goroutine
//     ⇒ 释放永不发生；标记残留还会永久阻止后续 resume（waitTaskStopped 判据）。
//  2. **本次新落的占位**（resumeReserved>0）：无人回收，全局账本永久虚占。
//  3. **被改写的终态四字段**：只有当写入的 pending 仍被持有时才恢复——窗口内并发的
//     CancelTask/DeleteTask（同一把锁）可能已发布 cancelled 终态，终态一旦对外可见就不该被
//     后继调用者改回旧终态（与「先删文件、后发布终态」同源：观察者看到的终态必须自洽）。
//  4. **被推迟给「不存在的 goroutine」的租户 Scope 释放**：见函数末尾的补充释放。
//
// 既有占位（resumeReserved==0 时 ReservedSize 为失败任务保留 .partial 的磁盘占用）不回滚：
// 磁盘上确有字节占着，回滚会破坏 failTask「账本向磁盘实际收敛」的约定；该占用随 DeleteTask/
// 过期清理释放（清理后 running 已清 ⇒ 立即释放）。
func (m *CloudDownloadManager) rollbackResumeLocked(task *CloudTask, prevStatus, prevError string, prevUpdatedAt, prevExpiresAt time.Time, resumeReserved int64) {
	taskID := task.ID
	if stored, ok := m.tasks[taskID]; ok {
		// 占位是否仍归属本次 resume：并发的 CancelTask/DeleteTask 会整体释放并把 ReservedSize
		// 归零，此时再释放会把账本打成负数。用数值关系判定是**保守**方向（宁可少释放也不反负）；
		// 之所以成立：所有释放路径都会同步把 ReservedSize 归零，不存在「降字段却不释放账本」的路径。
		if resumeReserved > 0 && stored.ReservedSize >= resumeReserved {
			m.storage.ReleaseCloud(resumeReserved)
			stored.ReservedSize -= resumeReserved
		}
		if stored.Status == "pending" {
			stored.Status, stored.Error = prevStatus, prevError
			stored.UpdatedAt, stored.ExpiresAt = prevUpdatedAt, prevExpiresAt
		}
	}
	delete(m.running, taskID)

	// 补充释放：闭合并发放弃留下的「释放被推迟给不存在的 goroutine」窗口（独立复核发现）。
	// ResumeTask 在「置 pending/running 并解锁」与「重新取锁回滚」（本函数）之间不持锁；窗口中
	// 并发的 CancelTask/DeleteTask 会看到 running==true，按「goroutine 仍会 commit 字节」的语义把
	// 租户 Scope 的释放推迟到 releaseAbandonedTaskScope（goroutine 退出路径）。而本次 resume 不会
	// 启动 goroutine，不在此补齐就只剩一个永远不存在的 goroutine 作为释放点——cancel 变体还有
	// cleanupExpiredOnce 兜底（有界），**delete 变体任务已不在 m.tasks，过期清理看不到它，只能等
	// 进程重启按磁盘校准 ⇒ 真实账本泄漏**。
	// 判据与 releaseAbandonedTaskScope 同源：任务已不在 m.tasks（被删除）或已发布 cancelled 终态时
	// 才释放；其余情况（任务仍在且非 cancelled，典型为 failed 保留 .partial）不释放——那些字节确实
	// 占着磁盘，回滚会破坏 failTask「账本向磁盘实际收敛」的约定（resume_tenant_test.go 场景 2 钉住）。
	if stored, ok := m.tasks[taskID]; !ok || stored.Status == "cancelled" {
		m.releaseTaskScope(task)
	}
}

// resumeWindowHook 是**测试 seam**：ResumeTask 在「置 pending/running 并解锁」与「重新取锁做
// 回滚」之间的窗口里调用一次（生产恒为 nil ⇒ no-op）。
//
// 为什么需要它：该窗口内并发的 CancelTask/DeleteTask 会把租户 Scope 的释放推迟到「goroutine 退出
// 路径」，而本条 resume 不会启动 goroutine ⇒ 必须由 rollbackResumeLocked 补齐释放。这段交错无法
// 用真实调度确定性复现（窗口只有几条指令），只能靠 seam 注入——与同包 removeTaskFile seam 同一
// 思路：生产路径不替换，仅测试替换（替换者须自行恢复）。
var resumeWindowHook func(m *CloudDownloadManager, taskID string)

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

	// resume 前的终态快照：租户不可用（下方 taskDir 早退）时据此整体回滚，使内存状态
	// 与磁盘上已有的终态一致。
	prevStatus, prevError := task.Status, task.Error
	prevUpdatedAt, prevExpiresAt := task.UpdatedAt, task.ExpiresAt

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
	// resumeReserved 记录**本次调用**新落的占位（0 = 未新增，此时 task.ReservedSize 是任务失败时
	// 保留 .partial 的既有占位，早退回滚不得释放它）。
	var resumeReserved int64
	if task.ReservedSize == 0 {
		if err := m.storage.TryReserveCloud(cloudReservePlaceholder); err != nil {
			// 占位失败：本次 resume 不会启动 goroutine，必须**整体回滚**（见 rollbackResumeLocked）。
			// 旧实现只置 failed/写 Error 并删 running，代价是：① UpdatedAt/ExpiresAt 被改成
			// resume 尝试的时间（任务多活一个 TTL）；② 窗口内并发取消发布的 cancelled 终态被
			// 覆写成 failed（用户已成功取消，却读到失败）。存储不足的原因经返回值告知调用方。
			m.rollbackResumeLocked(task, prevStatus, prevError, prevUpdatedAt, prevExpiresAt, 0)
			m.mu.Unlock()
			return err
		}
		task.ReservedSize = cloudReservePlaceholder
		resumeReserved = cloudReservePlaceholder
	}
	m.mu.Unlock()

	// 测试 seam：在「置 pending/running 并解锁」与「重新取锁做回滚」之间的窗口里注入并发交错
	// （生产恒 nil）。语义见 resumeWindowHook 的说明。
	if resumeWindowHook != nil {
		resumeWindowHook(m, taskID)
	}

	taskDir := m.TaskDirFor(task.Owner, task.ID)
	if taskDir == "" {
		// 租户不可用（owner 失效 / 存储根卸载）：本次 resume 不会启动下载 goroutine，
		// 必须把上面已落地的内存改动整体回滚，否则会留下三处永久遗留：
		//   ① running 标记 —— CancelTask/DeleteTask 见到 running 为真就把租户 Scope 的释放
		//      推迟到「goroutine 退出路径」，而本任务永远不会有 goroutine ⇒ 释放永不发生；
		//   ② 本次新落的占位 —— 无人回收，全局账本永久虚占；
		//   ③ 无 goroutine 的 pending 状态 —— TTL 只覆盖终态，pending 的兜底清理（cleanupExpiredOnce
		//      的 pending 分支）要等 TaskTTL（默认 24h）才生效，而那个窗口里 findByURL 仍会把同 URL
		//      的新请求去重吸收到这条永不启动的任务上。
		// 既有占位（resumeReserved==0 时 task.ReservedSize 为失败任务保留 .partial 的占用）
		// 不回滚：磁盘上确有字节占着，回滚会破坏 failTask「账本向磁盘实际收敛」的约定；
		// 这类任务的占用随 DeleteTask/过期清理释放（清理后 running 已清 ⇒ 立即释放）。
		m.mu.Lock()
		m.rollbackResumeLocked(task, prevStatus, prevError, prevUpdatedAt, prevExpiresAt, resumeReserved)
		m.mu.Unlock()
		return fmt.Errorf("tenant unavailable for task %s", taskID)
	}
	destPath := filepath.Join(taskDir, task.Filename)
	if force {
		// force 丢弃产物（结果文件 + .partial + .partial.etag）后必须回拨其 Scope 占用：下载器
		// 看不到旧 partial，无法走它自己的 discardedSize 回拨路径，新会话会在旧占用基础上再记
		// 一遍全量，而成功路径的绝对值记账（QuotaCommitted = result.Size）使差额再也无法由
		// 释放路径抹平，只能等 ≤30 min 周期扫描。
		//
		// 只回拨**确实从磁盘消失**的字节（见 removeDiscardedTaskFiles）：删除失败（Windows 句柄
		// 占用/杀软短暂持有，重试耗尽）时字节仍占磁盘，照旧回拨会让账本低于磁盘（fail-open：
		// 租户短时可越过 max_storage_bytes）；偏高只由周期扫描收敛。
		// 此处无 goroutine（waitTaskStopped 已确认）且 running 已置位（并发 DeleteTask 会推迟
		// 释放）⇒ 锁内记账后由本路径独占释放，releaseTaskScope 的复调释放 0（幂等）。
		discarded := removeDiscardedTaskFiles(destPath, removeTaskFile)
		m.mu.Lock()
		// 不得释放超过本任务记录的占用：删除的字节多于本任务记的账时，超出部分属同桶邻居的
		// 份额，直接 ReleaseUsage 会吃掉它们（层内无归属，见 pkg/quota 的逐层钳制）。
		if task.account != nil && discarded > 0 {
			task.account.ReleaseCommitted(discarded)
		}
		m.mu.Unlock()
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
