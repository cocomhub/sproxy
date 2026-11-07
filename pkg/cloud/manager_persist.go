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
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// saveTask 持久化单个任务到磁盘，返回写盘错误（供终态调用方显式处理）。
// 进度类保存（dirty flush）失败可容忍（调用方忽略返回值）；终态（completed/
// failed/cancelled）保存失败意味着重启后状态回滚，调用方必须记 Error 日志。
// cloudStateRel 是 owner 租户云任务**状态**的租户根相对目录：任务状态 JSON 落
// `<cloudStateRel>/<taskID>.json`、组状态落 `<cloudStateRel>/groups/<groupID>.json`。
//
// **状态一律经 storage.Root 读写**（卷自己的语义，使用方不理解加密）：at-rest 加密卷上
// 由卷加密落盘——状态 JSON 含 URL、文件名、目标路径等**用户可见元数据**，此前裸 os I/O +
// 绝对路径会让它在加密卷上明文落盘（`docs/config.md` 的加密承诺落空）；同时经 Root 即
// 天然继承封印判定（封装卷占用的区间写入恒拒），不再有绕过封印写元数据的路径。
// 绝对路径（PersistDirFor/groupsDirFor）**仅供装配层测试按布局断言与运维排查**，不得用于 I/O。
const cloudStateRel = "meta/cloud"

// maxCloudStateBytes 是状态 JSON 的读取上界（防注入超大 JSON 的 OOM；状态体量是
// 「条数 × 记录大小」，8MiB 已远超正常形态）。超限 = 视为损坏 → 跳过该条。
const maxCloudStateBytes = 8 << 20

// writeRootFile 经 Root 写文件（卷决定是否加密；非加密卷 = 普通写）。语义与
// os.WriteFile(..., 0o644) 对齐（非原子；需要原子性时调用方自行 tmp + AtomicRename）。
func writeRootFile(root *storage.Root, rel string, data []byte) error {
	w, err := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, werr := w.Write(data); werr != nil {
		_ = w.Close()
		return werr
	}
	return w.Close()
}

// readRootFile 经 Root 读文件（卷决定是否解密；非加密卷 = 普通读）+ 上界保护。
func readRootFile(root *storage.Root, rel string) ([]byte, error) {
	rc, err := root.OpenDecrypted(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, maxCloudStateBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxCloudStateBytes {
		return nil, fmt.Errorf("状态文件超出上界（%d 字节）: %s", maxCloudStateBytes, rel)
	}
	return data, nil
}

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
	root := m.cloudStateRootFor(t.Owner)
	if root == nil {
		m.logger.Warn("租户不可用，跳过任务持久化", "id", t.ID, "owner", t.Owner)
		return fmt.Errorf("tenant unavailable for task %s", t.ID)
	}
	if err := root.MkdirAll(cloudStateRel, 0o755); err != nil {
		m.logger.Warn("创建持久化目录失败", "dir", cloudStateRel, "error", err)
		return err
	}
	// **原子写**：唯一 tmp（随机段）+ AtomicRename。`writeRootFile` 是 O_TRUNC 截断写
	// （非原子），completed 终态保存（persistCompleted）与 flushLoop 进度批量保存**并发**
	// 写同一任务文件时，截断+写交错会写出损坏 JSON ⇒ 重启恢复 unmarshal 失败、任务丢失
	// （TestCloudDownloadManager_GroupLifecycleAndPersistence 在 CI -race 下实测偶发）。
	// 每次保存写**自己的 tmp**（互不截断），rename 原子 ⇒ 正式文件恒为某个**完整**快照。
	// tmp 以 `.tmp.<hex>` 结尾（非 `.json`）⇒ 恢复扫描（按 `.json` 后缀过滤）不会误读半写文件。
	var suffix [4]byte
	_, _ = rand.Read(suffix[:])
	rel := cloudStateRel + "/" + t.ID + ".json"
	tmpRel := rel + ".tmp." + hex.EncodeToString(suffix[:])
	if err := writeRootFile(root, tmpRel, data); err != nil {
		m.logger.Warn("failed to persist task", "id", t.ID, "error", err)
		return err
	}
	if err := root.AtomicRename(tmpRel, rel); err != nil {
		_ = root.Remove(tmpRel) // rename 失败清理 tmp；正式文件保持旧版本（不损坏）
		m.logger.Warn("failed to persist task (rename)", "id", t.ID, "error", err)
		return err
	}
	return nil
}

// recoverTasks 从磁盘恢复所有任务（遍历所有租户的 meta/cloud/）。
// 仅重启 downloading 状态的任务（崩溃前正在下载中）。
// pending 任务不自动启动——避免 CreateTask 创建但未 SubmitAndStart 的任务在崩溃后意外启动。
//
// 但「不自动启动」与「可以继续留在 pending」是**两个不同的决定**：重启后仍处于 pending 的
// 任务必然是「已落盘、却从未启动」的孤儿（本进程重建时没有任何 goroutine 会去启动它），
// 必须转终态，否则 findByURL 会把同 URL 的新请求永久吸收到它身上（见 failOrphanPendingTask）。
//
// 返回值：因孤儿 pending 转终态而需要刷新状态的组 ID。调用方必须在 recoverGroups **之后**
// 调用 UpdateGroupStatus——组记录比任务晚恢复，此刻刷新会因组不存在而空转。
func (m *CloudDownloadManager) recoverTasks(ctx context.Context) (int, []string) {
	recovered := 0
	restarted := 0
	unreadable := 0
	var orphanPendingGroups []string
	for _, tenant := range m.listTenants() {
		root := m.cloudStateRootFor(tenant)
		if root == nil {
			continue
		}
		n1, n2, orphans, skipped := m.recoverTasksFromDir(ctx, root)
		recovered += n1
		restarted += n2
		unreadable += skipped
		orphanPendingGroups = append(orphanPendingGroups, orphans...)
	}
	// NEW-2：**不可读/坏 JSON 的任务必须可观测**（此前只逐条 Warn ⇒ 汇总层面「全部成功」）。
	if unreadable > 0 {
		m.logger.Warn("cloud tasks unreadable on recovery (skipped, not resumable)",
			"skipped", unreadable, "recovered", recovered)
	}
	if recovered > 0 {
		m.logger.Info("cloud download tasks recovered", "count", recovered, "restarted", restarted,
			"skipped", unreadable)
	}
	return recovered, orphanPendingGroups
}

// RecoverAll 是云任务**启动恢复的唯一入口**（M3 统一恢复协调器通过 legacy 注册调用）。
//
//   - **幂等**：只真正执行一次；重复/并发调用直接返回 0（绝不重复挂载任务或重复重启下载 ✗）。
//   - **ctx 有界**：取消即停止继续恢复（装配层传入 startupRecoveryTimeout；此前签名无 ctx
//     ⇒ 超时预算无效，恢复挂住会导致服务永不监听）。
//   - 构造期默认已执行（`DeferRecovery=false`，即零行为变更 ✓）；装配层若把触发点交给协调器，
//     则置 `DeferRecovery=true`，由协调器在启动恢复阶段调用本方法。
//   - **顺序契约（不可颠倒）**：先 `recoverTasks`（按 m.tasks 修剪组内任务引用）→ 再
//     `recoverGroups` → 最后按孤儿 pending 组刷新组状态（否则组尚不存在时 UpdateGroupStatus 空转，
//     Web UI 会一直看到 downloading）。
func (m *CloudDownloadManager) RecoverAll(ctx context.Context) (int, error) {
	if m == nil {
		return 0, nil
	}
	if m.recovered.Swap(true) {
		return 0, nil // 已执行（构造期或前一次调用）⇒ 幂等 no-op
	}
	recovered, orphanPendingGroups := m.recoverTasks(ctx)
	m.recoverGroups()
	for _, gid := range orphanPendingGroups {
		m.UpdateGroupStatus(gid)
	}
	return recovered, nil
}

// recoverTasksFromDir 解析单个租户持久化目录下的任务 JSON 并挂入 m.tasks，同时重启崩溃前
// downloading 的任务、把孤儿 pending 转终态。返回恢复数、重启数与需要刷新状态的孤儿组 ID。
func (m *CloudDownloadManager) recoverTasksFromDir(ctx context.Context, root *storage.Root) (int, int, []string, int) {
	entries, err := root.ReadDir(cloudStateRel)
	if err != nil {
		return 0, 0, nil, 0
	}
	recovered, restarted, unreadable := 0, 0, 0
	var orphanPendingGroups []string
	// 先**收集**，循环内不动 `m.tasks`：循环里会为 downloading 任务启动下载 goroutine，
	// 而无锁写与它们的持锁访问并发 ⇒ data race（`-race` 实测），极端时序下
	// `fatal error: concurrent map read and map write` 崩溃，且发生在启动阶段（任务状态已被
	// 部分改写 ⇒ 重启再次触发）。评审 P1，2026-10-18。
	var pending []recoveredTask
	for _, e := range entries {
		// **启动恢复必须有界**（P2 修复，2026-10-21）：此前 RecoverAll 不接受 ctx（装配层传的
		// startupRecoveryTimeout 被丢弃）⇒ 状态目录很大或底层 I/O 挂死时恢复永不结束，而恢复
		// 是 **ListenAndServe 之前**同步执行的 ⇒ 服务永不监听、日志也看不到超时原因。
		// 契约：取消即**停止继续恢复**（已插入的条目保持原状态，下一轮启动再继续；不产生半提交）。
		if cerr := ctx.Err(); cerr != nil {
			m.logger.Warn("启动恢复被取消（剩余任务留待下次启动）", "error", cerr)
			break
		}
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := readRootFile(root, cloudStateRel+"/"+e.Name())
		if err != nil {
			m.logger.Warn("failed to read persisted task, skipping", "file", e.Name(), "error", err)
			unreadable++
			continue
		}
		var task CloudTask
		if err := json.Unmarshal(data, &task); err != nil {
			m.logger.Warn("failed to unmarshal persisted task, skipping", "file", e.Name(), "error", err)
			unreadable++
			continue
		}
		// 重启后以磁盘实际占用为基准重算 ReservedSize，
		// 与 StorageManager 启动扫描的计数器保持一致（不信任崩溃前持久化的占位值）
		m.reconcileReservedSize(&task)
		recovered++
		// C1：**已提交过**（Submitted）的 pending 任务 = 排队中/正在等并发槽 ⇒ 重启必须继续；
		// 只有「从未被 SubmitAndStart 接管」的 pending 才是真孤儿（CreateTask 与启动之间崩溃）。
		restart := task.Status == "downloading" || (task.Status == "pending" && task.Submitted)
		pending = append(pending, recoveredTask{task: &task, restart: restart})
	}

	// 持锁**一次性**插入：此后 `m.tasks` 对一切持锁访问者可见且完整（不再有无锁写）。
	m.insertRecoveredTasksLocked(pending)

	// 再按状态处理（启动下载 / 孤儿 pending）：此刻循环已结束，不存在无锁写与 goroutine 并发。
	restarted, orphanPendingGroups = m.activateRecoveredTasks(pending, orphanPendingGroups)
	if unreadable > 0 {
		// 指标在**唯一解析处**累计：无论调用方是 RecoverAll 还是测试，口径一致。
		m.metrics.RecoverySkipped.Add(int64(unreadable))
	}
	return recovered, restarted, orphanPendingGroups, unreadable
}

// recoveredTask 是恢复循环收集到的一条任务及其后续处置。
type recoveredTask struct {
	task    *CloudTask
	restart bool
}

// insertRecoveredTasksLocked 持锁**一次性**插入恢复集（此后 `m.tasks` 对一切持锁访问者
// 可见且完整）。与 `activateRecoveredTasks` 拆开，就是为了让「map 写入」与「启动 goroutine」
// 在时间上彻底分离（评审 P1：边插边启会与 goroutine 的持锁访问并发 ⇒ data race）。
func (m *CloudDownloadManager) insertRecoveredTasksLocked(pending []recoveredTask) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range pending {
		m.tasks[p.task.ID] = p.task
	}
}

// activateRecoveredTasks 在恢复循环**结束之后**按状态处置：重启 downloading（崩溃前正在
// 下载）、把孤儿 pending 转失败。返回 restarted 计数与孤儿组名。
func (m *CloudDownloadManager) activateRecoveredTasks(pending []recoveredTask, orphanGroups []string) (int, []string) {
	restarted := 0
	for _, p := range pending {
		if p.restart {
			m.logger.Info("restarting interrupted download", "task_id", p.task.ID, "url", p.task.URL)
			m.mu.Lock()
			m.markRunningLocked(p.task.ID)
			m.mu.Unlock()
			m.wg.Add(1)
			go m.executeDownload(context.Background(), p.task)
			restarted++
			continue
		}
		// 仅重启「downloading」与「已提交但仍在排队（pending+Submitted）」；**从未提交**的
		// pending 不自动启动——避免 CreateTask 创建但尚未 SubmitAndStart 就崩溃导致意外启动。
		if p.task.Status == "pending" && !p.task.Submitted {
			// 孤儿 pending：已落盘却从未启动（详见 failOrphanPendingTask）。
			m.failOrphanPendingTask(p.task)
			if p.task.GroupID != "" {
				orphanGroups = append(orphanGroups, p.task.GroupID)
			}
		}
	}
	return restarted, orphanGroups
}

// failOrphanPendingTask 把恢复出来的「已持久化 pending、但从未启动」任务转终态。
//
// 为什么必须转终态，而不是留在 pending：
//   - **崩溃窗口**：CreateTask 先 saveTask(pending)，之后才由 SubmitAndStart 同步置 running 并
//     启动 goroutine；进程若死在两者之间，这条任务永远没有人会去启动它。组路径会放大：
//     CreateGroup 批量落盘 pending 子任务，SubmitAndStartGroup 是另一次独立调用，循环中途崩溃
//     会留下多条孤儿；
//   - findByURL 只匹配 pending/downloading ⇒ 同 URL 的新请求会被去重**吸收**到这条永不启动
//     的任务上：API 报成功、下载永不发生；
//   - TTL 只覆盖终态（completed/failed/cancelled）；pending 的兜底清理（cleanupExpiredOnce 的
//     pending 分支）要等 TaskTTL（默认 24h）才生效 ⇒ 那个窗口里任务与其占位一直驻留；
//   - 上述两点叠加后，同 URL 在本次进程生命周期内都无法再下载（直到进程再次重启）。
//
// 为什么**不回拨占位**：reconcileReservedSize 已把 ReservedSize 校准为任务目录的实际占用
// （重启后全局账本本身来自磁盘扫描），此处释放会让账本低于磁盘实际占用，破坏 failTask
// 「账本向磁盘实际收敛」的既有约定；占用随 FailedTaskTTL 过期清理释放（该路径同时删文件）。
func (m *CloudDownloadManager) failOrphanPendingTask(task *CloudTask) {
	m.mu.Lock()
	m.clearRunningLocked(task.ID) // 防御：恢复期不应存在（本进程尚未为它启动 goroutine）
	task.Status = "failed"
	// 文案含「or while queued」：SubmitAndStart/SubmitAndStartGroup 先置 running，磁盘状态要等
	// executeDownload 拿到信号量之后才写成 downloading，故 URL 数超过 MaxConcurrent（默认 3）时
	// 必然存在「已启动但排队、磁盘仍是 pending」的任务——重启后它同样不会自行恢复为下载中。
	task.Error = "interrupted before start or while queued; resume to retry"
	task.UpdatedAt = time.Now()
	task.ExpiresAt = time.Now().Add(m.config.FailedTaskTTL)
	m.signalStoppedLocked(task.ID)
	m.mu.Unlock()

	if err := m.saveTask(task); err != nil {
		m.logger.Error("persist orphan pending task state, state may be lost on restart",
			"task_id", task.ID, "error", err)
	}
	m.refreshTaskGroup(task)
	m.metrics.TasksFailed.Add(1)
	m.logger.Warn("orphan pending task marked failed on recovery (never started, or queued before shutdown)",
		"task_id", task.ID, "url", task.URL, "owner", task.Owner, "group_id", task.GroupID)
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
//
// **只校准全局容量账本，不重建租户 Scope 的 QuotaCommitted**（两者同为 `json:"-"`）。于是重启后
// 恢复出来的任务在删除/过期时 Scope 释放量为 0：桶会比磁盘**偏高**（差额 = 被删除字节），方向是
// **fail-closed**——只会偏严（租户可能误报 507），不会让租户超限，也不污染祖先层（祖先 ≥ 子树
// 之和仍成立），由 ≤30 min 周期扫描以磁盘为准收敛；而 CategoryCloud 因本函数已对齐，删除时可
// 精确释放。
//
// 刻意不在此重算 Scope 占用：那等于为「磁盘为准」的周期扫描引入并行的第二事实源——恢复后文件
// 被外部改动时，释放量会与实际移除字节不符，方向从 fail-closed 翻转为 **fail-open**（欠计 ⇒ 租户
// 可短时超限）。若将来确实要改变该语义（持久化字段或恢复期重算），必须同步更新这里与
// TestCloudQuotaRestart_DeleteStaysFailClosedUntilRescan。
func (m *CloudDownloadManager) reconcileReservedSize(task *CloudTask) {
	task.ReservedSize = m.diskUsageOfTask(task.Owner, task.ID)
}

// recoverGroups 从磁盘恢复任务组（遍历所有租户的 meta/cloud/groups/），并修剪已不存在的任务引用。
func (m *CloudDownloadManager) recoverGroups() {
	recovered := 0
	for _, tenant := range m.listTenants() {
		root := m.cloudStateRootFor(tenant)
		if root == nil {
			continue
		}
		recovered += m.recoverGroupsFromDir(root)
	}
	// 兼容旧数据：为带 GroupID 但缺少组记录的孤儿任务重建最小组。
	recovered += m.recoverOrphanGroups()
	if recovered > 0 {
		m.logger.Info("cloud download groups recovered", "count", recovered)
	}
}

// recoverGroupsFromDir 解析单个租户组目录下的组 JSON，修剪已不存在的任务引用并挂入 m.groups。
func (m *CloudDownloadManager) recoverGroupsFromDir(root *storage.Root) int {
	groupsRel := cloudStateRel + "/groups"
	entries, err := root.ReadDir(groupsRel)
	if err != nil {
		return 0
	}
	recovered := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := readRootFile(root, groupsRel+"/"+e.Name())
		if err != nil {
			continue
		}
		var group CloudTaskGroup
		if err := json.Unmarshal(data, &group); err != nil {
			continue
		}
		group.TaskIDs = m.pruneGroupTaskIDs(group.TaskIDs)
		if len(group.TaskIDs) == 0 {
			_ = root.Remove(groupsRel + "/" + e.Name())
			continue
		}
		m.groupMu.Lock()
		m.groups[group.ID] = &group
		m.groupMu.Unlock()
		recovered++
	}
	return recovered
}

// recoverOrphanGroups 为带 GroupID 但缺少组记录的孤儿任务重建最小组（兼容旧数据）。
func (m *CloudDownloadManager) recoverOrphanGroups() int {
	// P1 修复（2026-10-21）：按 **GroupID 聚合**后一次建组。
	//
	// 此前在 m.groups 尚未填充的扫描阶段逐任务新建组，且 `TaskIDs` 硬编码为 `[t.ID]`、
	// `TotalTasks: 1` ⇒ 同一 GroupID 的 N 个孤儿任务产生 N 个组对象，`m.groups[gid]` 最终
	// 只留**最后一个**任务。后果：组视图丢成员、单个成员完成即被 `UpdateGroupStatus` 判整组
	// completed、组级 cancel/archive/删除只作用于 1 个任务（其余继续跑并占并发/配额）。
	//
	// 可达路径均为生产路径：`CreateGroup` 先建子任务、最后才 `saveGroup`（该窗口崩溃/磁盘满），
	// 以及组文件损坏（`recoverGroupsFromDir` 解析失败跳过）。
	existing := make(map[string]struct{})
	m.groupMu.RLock()
	for id := range m.groups {
		existing[id] = struct{}{}
	}
	m.groupMu.RUnlock()

	agg := make(map[string]*CloudTaskGroup)
	m.mu.RLock()
	for _, t := range m.tasks {
		if t.GroupID == "" {
			continue
		}
		if _, ok := existing[t.GroupID]; ok {
			continue
		}
		mergeOrphanTask(agg, t)
	}
	m.mu.RUnlock()

	recovered := 0
	for _, group := range agg {
		// 任务来自 map 遍历 ⇒ ID 顺序不确定；排序保证重建结果可重现（测试/日志可比对）。
		slices.Sort(group.TaskIDs)
		m.groupMu.Lock()
		m.groups[group.ID] = group
		m.groupMu.Unlock()
		_ = m.saveGroup(group)
		recovered++
	}
	return recovered
}

// mergeOrphanTask 把一个孤儿任务并入其 GroupID 的聚合组（不存在则新建）。调用方持 m.mu.RLock。
func mergeOrphanTask(agg map[string]*CloudTaskGroup, t *CloudTask) {
	g, ok := agg[t.GroupID]
	if !ok {
		// 继承子任务 owner，避免带 owner 的组被重建为全局可见（组级隔离漏洞）。
		g = &CloudTaskGroup{
			ID:        t.GroupID,
			Owner:     t.Owner,
			Name:      t.GroupID,
			Status:    "pending",
			CreatedAt: t.CreatedAt,
			ExpiresAt: t.ExpiresAt,
		}
		agg[t.GroupID] = g
	}
	g.TaskIDs = append(g.TaskIDs, t.ID)
	g.TotalTasks++
	if !t.CreatedAt.IsZero() && (g.CreatedAt.IsZero() || t.CreatedAt.Before(g.CreatedAt)) {
		g.CreatedAt = t.CreatedAt
	}
	if t.UpdatedAt.After(g.UpdatedAt) {
		g.UpdatedAt = t.UpdatedAt
	}
	if t.ExpiresAt.After(g.ExpiresAt) {
		g.ExpiresAt = t.ExpiresAt
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
	root := m.cloudStateRootFor(g.Owner)
	if root == nil {
		m.logger.Warn("租户不可用，跳过组持久化", "id", g.ID, "owner", g.Owner)
		return fmt.Errorf("tenant unavailable for group %s", g.ID)
	}
	groupsRel := cloudStateRel + "/groups"
	if err := root.MkdirAll(groupsRel, 0o755); err != nil {
		m.logger.Warn("failed to create groups dir", "dir", groupsRel, "error", err)
		return err
	}
	if err := writeRootFile(root, groupsRel+"/"+g.ID+".json", data); err != nil {
		m.logger.Warn("failed to persist group", "id", g.ID, "error", err)
		return err
	}
	return nil
}

// removeGroupFile 删除 owner 租户下组的持久化文件。
func (m *CloudDownloadManager) removeGroupFile(groupID, owner string) {
	root := m.cloudStateRootFor(owner)
	if root == nil {
		return
	}
	_ = root.Remove(cloudStateRel + "/groups/" + groupID + ".json")
}

// removeTaskAuditFile 删除任务对应的审计 sink 文件（任务删除/TTL 过期清理路径调用，防
// <用户缓存目录>/sproxy/audit/<taskID>.audit.log 每任务一文件无限累积——评审 R1）。
// 文件命名与 newTaskAuditScope 的 sink 一致：<auditSinkDir()>/<taskID>.audit.log。
// 删除失败容忍（_ =，失败仅残留磁盘卫生、不阻断删除/过期清理）：删除成功与否都不影响
// 任务状态与账本。有界重试（removeWithRetry）缓解 Windows 句柄共享违规瞬时占用。
func (m *CloudDownloadManager) removeTaskAuditFile(taskID string) {
	if taskID == "" {
		return
	}
	_ = removeWithRetry(func() error { return os.Remove(filepath.Join(m.auditSinkDir(), taskID+".audit.log")) })
}

// cleanupExpiredOnce 执行一次性的过期任务清理，返回清理的任务数量。
// 不包含循环，供测试直接调用。
//
// 注意：函数内部会先释放 m.mu 再执行 I/O 删除操作，调用者不应假设调用期间 mu 一直被持有。
// accountCommittedOf 返回任务 account 的已确认占用（account nil 时 0）。
// cleanupExpiredOnce 在锁内收集快照时调用（account.Committed() 内部自锁，安全）。
func accountCommittedOf(t *CloudTask) int64 {
	if t.account == nil {
		return 0
	}
	return t.account.Committed()
}

func (m *CloudDownloadManager) cleanupExpiredOnce() int {
	expired := m.collectExpiredCloudTasks(time.Now())
	if len(expired) == 0 {
		return 0
	}

	// 锁外执行 I/O 和 checksum 操作（按任务 owner 落租户桶）
	cleaned := 0
	for _, item := range expired {
		m.cleanupExpiredCloudItem(item)
		cleaned++
	}

	// 清理引用已全部过期任务的空组（saveGroup 在锁外调用，避免 RLock 重入死锁）。
	m.cleanupExpiredGroups()

	m.logger.Info("expired cloud download tasks cleaned up", "count", cleaned)
	return cleaned
}

// expiredCloudTask 描述一个到期应清理的任务条目（在锁内收集的快照）。
type expiredCloudTask struct {
	id             string
	taskID         string
	filename       string
	owner          string
	reservedSize   int64
	scopeCommitted int64
	wasPending     bool // 仅用于日志：区分「孤儿 pending 兜底清理」与普通终态过期
	// task 指向原任务对象：删除失败时回填重试（独立复审 P2，2026-10-18）。nil = 已确认清理。
	task *CloudTask
}

// collectExpiredCloudTasks 在锁内扫描任务表，收集到期应清理的条目并同步从内存移除。
// 避免锁内 I/O 阻塞（I/O 由调用方在锁外执行）。
func (m *CloudDownloadManager) collectExpiredCloudTasks(now time.Time) []expiredCloudTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	var expired []expiredCloudTask
	for id, t := range m.tasks {
		ttl, ok := m.expiredCloudTaskTTL(t)
		if !ok || !now.After(t.UpdatedAt.Add(ttl)) {
			continue
		}
		expired = append(expired, expiredCloudTask{
			id:             id,
			taskID:         t.ID,
			filename:       t.Filename,
			owner:          t.Owner,
			reservedSize:   t.ReservedSize,
			scopeCommitted: accountCommittedOf(t),
			wasPending:     t.Status == "pending",
			task:           t,
		})
		// **不在收集阶段删表项/归零**（独立复审 P2，2026-10-18）：删除是 I/O（锁外执行）且可能失败，
		// 若此处就丢弃任务，失败后既无法重试、又必须归还账本（否则永久虚占）⇒ 只能 fail-open
		// （字节仍在盘上而账本已归还）。改为：由 cleanupExpiredCloudItem **确认删除成功后**再摘除。
	}
	return expired
}

// expiredCloudTaskTTL 返回任务参与过期清理的 TTL 与是否应参与；false 表示不清理该任务。
func (m *CloudDownloadManager) expiredCloudTaskTTL(t *CloudTask) (time.Duration, bool) {
	switch t.Status {
	case "completed":
		return m.config.TaskTTL, true
	case "failed", "cancelled":
		return m.config.FailedTaskTTL, true
	case "pending":
		// 安全网：pending 正常情况下没有 TTL——它要么被 SubmitAndStart 启动（转
		// downloading），要么在恢复期被判定为孤儿并转终态。这里兜住将来新增的
		// 「写入 pending 后失败早退」路径，避免退化成永久 pending（永久占用 + 被
		// findByURL 吸收同 URL 请求）。只清**没有 goroutine** 且已超 TaskTTL 的
		// pending：刚创建的 pending 与仍在跑的下载（running 为真）一律不动。
		if m.running[t.ID] {
			return 0, false
		}
		return m.config.TaskTTL, true
	default:
		return 0, false
	}
}

// cleanupExpiredCloudItem 锁外为一个过期任务条目执行 I/O 与 checksum 清理（按 owner 落租户桶）。
func (m *CloudDownloadManager) cleanupExpiredCloudItem(item expiredCloudTask) {
	if item.wasPending {
		// 走到这里的 pending 是兜底清理出来的孤儿：它没有正常生命周期终点，必须发声。
		m.logger.Warn("expired orphan pending task cleaned up (never started, no goroutine)",
			"task_id", item.taskID, "owner", item.owner)
	}
	if root := m.cloudStateRootFor(item.owner); root != nil {
		_ = root.Remove(cloudStateRel + "/" + item.id + ".json")
	}
	// **删除确认后再归还**（独立复审 P2，2026-10-18）：删除失败 ⇒ 保留占账并**回填任务**由下一轮
	// 过期清理重试（否则"保留占账"会变成永久虚占，而"照常归还"是 fail-open）。
	if rerr := m.removeTaskDir(item.owner, item.taskID); rerr != nil {
		m.logger.Error("expired task dir removal failed; keeping quota charged and will retry",
			"task_id", item.taskID, "owner", item.owner, "error", rerr)
		if item.task != nil {
			m.mu.Lock()
			item.task.UpdatedAt = time.Now() // 顺延，下一轮 TTL 到期再试
			m.tasks[item.id] = item.task
			m.mu.Unlock()
		}
		return
	}
	m.mu.Lock()
	// 删除已确认：摘除表项并归零（防二次释放）。
	if item.task != nil {
		item.task.ReservedSize = 0
		delete(m.tasks, item.id)
	}
	m.mu.Unlock()
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
	// 清理审计 sink 文件（R1：过期清理路径同步清，防 <用户缓存目录>/sproxy/audit/ 无限累积）。
	m.removeTaskAuditFile(item.taskID)
}

// cleanupExpiredGroups 清理引用已全部过期任务的空组，并保存被裁剪引用组的更新状态。
//
// 锁序说明：此处 m.groupMu 下调用 pruneGroupTaskIDs（内部取 m.mu.RLock），
// 嵌套顺序为 groupMu → mu；全代码库所有嵌套获取均遵循 groupMu → mu，
// 无反向路径（m.mu → groupMu），因此不存在 ABBA 死锁。
func (m *CloudDownloadManager) cleanupExpiredGroups() {
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
		// 再按剩余子任务重算组状态与计数（Failed/Cancelled/Status）：只裁剪 TaskIDs 会让组停留在
		// 被清理前的计数/状态上。与 C1 恢复期刷新组状态同源（failOrphanPendingTask→refreshTaskGroup），
		// 否则「C1 补齐了组刷新、C4 没补」的口径不一致。必须在 m.groupMu 解锁后调用：
		// UpdateGroupStatus 内部按 groupMu → m.mu 取锁，与 pruneGroupTaskIDs 的嵌套顺序一致，无反向路径。
		m.UpdateGroupStatus(g.ID)
	}
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
