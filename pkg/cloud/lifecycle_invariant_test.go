// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// lifecycle_invariant_test.go 钉住云任务生命周期的两条不变量（「并发新案例」的回归门禁）：
//
//  1. **不留下无人负责的任务**：写入 `pending` 之后若没能启动 goroutine（崩溃窗口 / 启动前
//     早退），任务必须转终态——否则 `findByURL`（只匹配 pending/downloading）会把同 URL 的
//     新请求**去重吸收**到这条永不启动的任务上：API 报成功、下载永不发生。TTL 只覆盖终态，
//     pending 的兜底清理（`cleanupExpiredOnce` 的 pending 分支）要等 TaskTTL（默认 24h）才
//     生效 ⇒ 恢复期必须立即转终态，而不是等兜底。
//  2. **不留下无人负责的 running 标记**：`running` 为真意味着「仍有 goroutine 会执行清理」。
//     #290 之后，取消/删除路径把租户配额的释放**推迟到 goroutine 退出路径**
//     （`releaseAbandonedTaskScope`），所以「running 为真但永无 goroutine」等于永久配额占用。
//
// 用例只驱动领域对象（不经装配层 HTTP 路由），与 manager_test_common_test.go 的基座一致；
// 全部确定性（无 sleep、无时序依赖）。绝大多数用例可 t.Parallel()，唯一例外是
// ResumeRollbackReleasesDeferredScope——它要替包级 seam resumeWindowHook（已在函数体内标注）。

import (
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// errReserveRefusedInTest 是 refusingStorage 的固定错误，供「存储不足」早退路径断言。
var errReserveRefusedInTest = errors.New("storage full (test double)")

// refusingStorage 是「存储恒不足」的 StorageManager 替身：让 ResumeTask 的重新占位必然失败，
// 用于验证该失败早退必须**整体回滚**（不得改写 UpdatedAt/ExpiresAt，不得把并发取消发布的
// cancelled 终态覆写成 failed）。
// usage 用 atomic：管理器的后台 goroutine（flushLoop / cleanupExpired）也会调用本替身，
// 普通 int64 在「该替身下跑过期清理」类用例里会变成跨 goroutine 读写（-race 假红）。
type refusingStorage struct{ usage atomic.Int64 }

func (s *refusingStorage) TryReserveCloud(int64) error { return errReserveRefusedInTest }
func (s *refusingStorage) ReleaseCloud(n int64)        { s.usage.Add(-n) }
func (s *refusingStorage) Usage() int64                { return s.usage.Load() }
func (s *refusingStorage) MaxBytes() int64             { return 0 }

// newRefusingStorageManager 用 refusingStorage 装配域管理器（真实租户缓存 + 真实配额 Scope，
// 仅为提供 quotaScope 与持久化目录；存储侧恒拒绝占位）。
func newRefusingStorageManager(t *testing.T, dir string, cfg *CloudDownloadConfig) *CloudDownloadManager {
	t.Helper()
	env := newCloudTestEnv(t, dir)
	mgr := NewCloudDownloadManager(env.root, &refusingStorage{},
		env.tenantFor, env.checksumStoreFor, env.listTenantIDs, testLogger(), cfg,
		func(owner string) *quota.Scope { return env.quotaBucketFor(owner, "cloud") })
	t.Cleanup(mgr.Close)
	return mgr
}

// lifecycleNoRunningMark 断言任务没有残留的 running 标记（running 为真 = 有 goroutine 负责清理）。
func lifecycleNoRunningMark(t *testing.T, mgr *CloudDownloadManager, taskID, stage string) {
	t.Helper()
	mgr.mu.RLock()
	_, running := mgr.running[taskID]
	mgr.mu.RUnlock()
	if running {
		t.Fatalf("%s 之后不得残留 running 标记（task=%s）：running 为真意味着仍有 goroutine 负责释放配额与清标记", stage, taskID)
	}
}

// TestCloudDownloadManager_OrphanPendingRecoveredAsFailed 覆盖 C1 崩溃窗口：
// CreateTask 先落盘 pending、之后才由 SubmitAndStart 启动 goroutine；进程若死在两者之间，
// 重启后这条任务既不会自动启动（recoverTasks 的既有决定）也不该继续留在 pending。
func TestCloudDownloadManager_OrphanPendingRecoveredAsFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4<<30, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr1, _ := newCloudTestManager(t, dir, sm, cfg)

	const orphanURL = "https://example.com/orphan.bin"
	task, err := mgr1.CreateTask("url", orphanURL, "orphan.bin", 1024, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Status != "pending" {
		t.Fatalf("CreateTask 后应为 pending, got %q", task.Status)
	}
	// 崩溃窗口的精确形态：pending 已落盘，但 SubmitAndStart 从未执行 ⇒ 没有任何 goroutine。
	persistFile := filepath.Join(mgr1.PersistDirFor(""), task.ID+".json")
	if _, statErr := os.Stat(persistFile); statErr != nil {
		t.Fatalf("pending 任务应已落盘（崩溃窗口的前提）: %v", statErr)
	}
	mgr1.Close()

	// 模拟进程重启：recoverTasks 在构造期运行。
	mgr2, _ := newCloudTestManager(t, dir, sm, cfg)
	snap, ok := mgr2.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatalf("任务应在重启后恢复: %s", task.ID)
	}
	if snap.Status == "pending" {
		t.Fatalf("孤儿 pending 不得保持 pending（否则 findByURL 会把同 URL 新请求永久吸收到永不启动的任务）：%+v", snap)
	}
	if snap.Status != "failed" {
		t.Fatalf("孤儿 pending 应转 failed, got %q", snap.Status)
	}
	if snap.Error == "" {
		t.Fatal("转终态必须带可解释原因（用户要据此决定是否 resume）")
	}
	lifecycleNoRunningMark(t, mgr2, task.ID, "recoverTasks")

	// 最关键的用户可见后果：同 URL 的新请求必须**不再**被吸收到这条任务上。
	fresh, err := mgr2.CreateTask("url", orphanURL, "orphan.bin", 1024, "")
	if err != nil {
		t.Fatalf("同 URL 重建任务: %v", err)
	}
	if fresh.ID == task.ID {
		t.Fatalf("同 URL 新请求仍被去重吸收到孤儿任务 %s（下载永不发生）", task.ID)
	}
}

// TestCloudDownloadManager_OrphanPendingGroupRecoveredAsFailed 覆盖 C1 的组放大形态：
// CreateGroup 批量落盘 pending 子任务，SubmitAndStartGroup 是独立的一次调用（循环中途崩溃会
// 留下多条孤儿）。同时钉住组状态不滞后：子任务转终态后组状态必须重算为 failed。
func TestCloudDownloadManager_OrphanPendingGroupRecoveredAsFailed(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4<<30, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr1, _ := newCloudTestManager(t, dir, sm, cfg)

	group, err := mgr1.CreateGroup("crash-group", []cloudfilename.Entry{
		{URL: "https://example.com/g1.bin", Filename: "g1.bin"},
		{URL: "https://example.com/g2.bin", Filename: "g2.bin"},
	}, "")
	if err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	if len(group.TaskIDs) != 2 {
		t.Fatalf("组应有 2 个子任务, got %d", len(group.TaskIDs))
	}
	for _, tid := range group.TaskIDs {
		snap, ok := mgr1.SnapshotTask(tid, "")
		if !ok || snap.Status != "pending" {
			t.Fatalf("子任务 %s 应为 pending（未启动）", tid)
		}
	}
	mgr1.Close()

	mgr2, _ := newCloudTestManager(t, dir, sm, cfg)
	for _, tid := range group.TaskIDs {
		snap, ok := mgr2.SnapshotTask(tid, "")
		if !ok {
			t.Fatalf("子任务 %s 未恢复", tid)
		}
		if snap.Status == "pending" {
			t.Fatalf("组内孤儿 pending 不得保持 pending：%+v", snap)
		}
		if snap.Status != "failed" || snap.Error == "" {
			t.Fatalf("组内孤儿 pending 应转 failed 且带原因, got status=%q error=%q", snap.Status, snap.Error)
		}
		lifecycleNoRunningMark(t, mgr2, tid, "recoverTasks")
	}
	// 组状态派生自子任务：全部转 failed 后不得停留在 pending/downloading。
	g2, ok := mgr2.GetGroup(group.ID, "")
	if !ok {
		t.Fatalf("组未恢复: %s", group.ID)
	}
	if g2.Status != "failed" {
		t.Fatalf("组内子任务全部 failed 后组状态应为 failed, got %q", g2.Status)
	}
}

// TestCloudDownloadManager_CleanupExpiredOrphanPending 覆盖 C4 安全网：
// 若将来再出现「写入 pending 后失败早退」的路径，任务会一直驻留到 TaskTTL（默认 24h）才被兜底
// 清理（且该窗口内同 URL 请求会被 findByURL 吸收）；
// 兜底判据为 pending ∧ 无 running ∧ 超 TaskTTL，且**不得**误伤新鲜 pending 与仍在运行的 pending。
func TestCloudDownloadManager_CleanupExpiredOrphanPending(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4<<30, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)

	stale := time.Now().Add(-2 * cfg.TaskTTL)
	mk := func(id, status string, updatedAt time.Time) *CloudTask {
		return &CloudTask{
			ID: id, Owner: "", URL: "https://example.com/" + id, Filename: id + ".bin",
			Status: status, CreatedAt: updatedAt, UpdatedAt: updatedAt,
			ExpiresAt: updatedAt.Add(cfg.TaskTTL),
		}
	}
	orphan := mk("cloud-orphan-stale", "pending", stale)
	freshPending := mk("cloud-pending-fresh", "pending", time.Now())
	runningPending := mk("cloud-pending-running", "pending", stale)

	mgr.mu.Lock()
	mgr.tasks[orphan.ID] = orphan
	mgr.tasks[freshPending.ID] = freshPending
	mgr.tasks[runningPending.ID] = runningPending
	mgr.running[runningPending.ID] = true
	mgr.mu.Unlock()
	t.Cleanup(func() {
		mgr.mu.Lock()
		delete(mgr.running, runningPending.ID)
		mgr.mu.Unlock()
	})

	if n := mgr.cleanupExpiredOnce(); n < 1 {
		t.Fatalf("过期孤儿 pending 应被清理, cleaned=%d", n)
	}
	mgr.mu.RLock()
	_, orphanExists := mgr.tasks[orphan.ID]
	_, freshExists := mgr.tasks[freshPending.ID]
	_, runningExists := mgr.tasks[runningPending.ID]
	mgr.mu.RUnlock()
	if orphanExists {
		t.Fatal("过期孤儿 pending 应被清理（否则永久驻留且被 findByURL 吸收）")
	}
	if !freshExists {
		t.Fatal("新鲜 pending 不得被误伤")
	}
	if !runningExists {
		t.Fatal("仍有 running 的 pending（在途下载）不得被误伤")
	}
}

// TestCloudDownloadManager_ResumeStorageFullRollsBackTerminal 覆盖 C2：
// ResumeTask 在「重新占位失败」早退时必须整体回滚——旧实现只改 Status/Error 并删 running，
// 导致 ① UpdatedAt/ExpiresAt 被改成 resume 尝试的时间（任务多活）② 窗口内并发取消发布的
// cancelled 终态被覆写成 failed（用户取消成功却读到失败）。
func TestCloudDownloadManager_ResumeStorageFullRollsBackTerminal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := defaultCloudDownloadConfig()
	mgr := newRefusingStorageManager(t, dir, cfg)

	prevUpdated := time.Now().Add(-30 * time.Minute)
	prevExpires := prevUpdated.Add(cfg.FailedTaskTTL)

	// 终态 + ReservedSize==0 ⇒ resume 会尝试重新占位（本替身必然拒绝）。
	seed := func(id, status, errText string) *CloudTask {
		task := &CloudTask{
			ID: id, Owner: "", URL: "https://example.com/" + id, Filename: id + ".bin",
			Status: status, Error: errText, TotalSize: 1024, ReservedSize: 0,
			CreatedAt: prevUpdated, UpdatedAt: prevUpdated, ExpiresAt: prevExpires,
		}
		mgr.mu.Lock()
		mgr.tasks[id] = task
		mgr.mu.Unlock()
		return task
	}

	for _, tc := range []struct {
		name   string
		status string
		err    string
	}{
		{name: "failed", status: "failed", err: "original failure"},
		{name: "cancelled", status: "cancelled", err: ""},
	} {
		// 每种终态各起一个子用例：两个终态的被覆写风险不同（failed 是错误文本/时间戳被改，
		// cancelled 是终态语义被改写），分开报告才能在回归时一眼看出是哪一路退化。
		t.Run(tc.name, func(t *testing.T) {
			task := seed("cloud-resume-rb-"+tc.name, tc.status, tc.err)

			if err := mgr.ResumeTask(task.ID, false, ""); err == nil {
				t.Fatalf("存储不足时 resume 必须返回错误")
			}
			lifecycleNoRunningMark(t, mgr, task.ID, "ResumeTask(占位失败)")

			snap, ok := mgr.SnapshotTask(task.ID, "")
			if !ok {
				t.Fatal("任务应仍存在")
			}
			if snap.Status != tc.status {
				t.Fatalf("占位失败的早退必须整体回滚，终态不得被改写, got %q want %q", snap.Status, tc.status)
			}
			if snap.Error != tc.err {
				t.Fatalf("错误文本应回滚为原值, got %q want %q", snap.Error, tc.err)
			}
			if !snap.UpdatedAt.Equal(prevUpdated) {
				t.Fatalf("UpdatedAt 应回滚为原值, got %v want %v", snap.UpdatedAt, prevUpdated)
			}
			if !snap.ExpiresAt.Equal(prevExpires) {
				t.Fatalf("ExpiresAt 应回滚为原值（否则任务生命周期被 resume 尝试改写）, got %v want %v", snap.ExpiresAt, prevExpires)
			}
		})
	}
}

// TestCloudDownloadManager_LifecycleLeavesNoOrphanRunning 是收口不变量：创建/取消/删除这条
// 最常用的生命周期序列（均不启动 goroutine）走完后，不得留下 running 标记或幽灵任务。
func TestCloudDownloadManager_LifecycleLeavesNoOrphanRunning(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 4<<30, nil, testLogger())
	cfg := defaultCloudDownloadConfig()
	mgr, _ := newCloudTestManager(t, dir, sm, cfg)

	task, err := mgr.CreateTask("url", "https://example.com/life.bin", "life.bin", 1024, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	lifecycleNoRunningMark(t, mgr, task.ID, "CreateTask")

	if err := mgr.CancelTask(task.ID, ""); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	lifecycleNoRunningMark(t, mgr, task.ID, "CancelTask")
	if snap, ok := mgr.SnapshotTask(task.ID, ""); !ok || snap.Status != "cancelled" {
		t.Fatalf("取消后应为 cancelled, got %+v", snap)
	}

	if err := mgr.DeleteTask(task.ID, ""); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}
	lifecycleNoRunningMark(t, mgr, task.ID, "DeleteTask")
	mgr.mu.RLock()
	_, exists := mgr.tasks[task.ID]
	mgr.mu.RUnlock()
	if exists {
		t.Fatalf("删除后任务不应仍在 m.tasks 中: %s", task.ID)
	}
}

// TestCloudDownloadManager_ResumeRollbackReleasesDeferredScope 覆盖独立复核发现的窄窗口：
// ResumeTask 在「置 pending/running 并解锁」与「重新取锁回滚」之间可能被并发 CancelTask/
// DeleteTask 命中——对方看到 running==true，按「goroutine 仍会 commit 字节」的语义把租户 Scope
// 的释放**推迟**到 releaseAbandonedTaskScope（goroutine 退出路径）；而本条 resume 不会启动
// goroutine ⇒ 释放点只剩一个永远不存在的 goroutine（cancel 变体还有 cleanupExpiredOnce 兜底、
// 有界；delete 变体任务已不在 m.tasks，只能等进程重启按磁盘校准 ⇒ 真实泄漏）。
//
// 该交错无法用真实调度确定性复现（窗口只有几条指令），故用包级 seam resumeWindowHook 注入
// （与 removeTaskFile seam 同一思路：生产路径不替换）。因为是包级变量，本用例不并行。
func TestCloudDownloadManager_ResumeRollbackReleasesDeferredScope(t *testing.T) {
	// sproxy:serial: 需要替包级 seam resumeWindowHook，与并行用例互斥。

	for _, tc := range []struct {
		name        string
		abandon     func(t *testing.T, mgr *CloudDownloadManager, owner, taskID string)
		wantStatus  string
		wantMissing bool // 放弃后任务是否应从 m.tasks 消失（DeleteTask 会，CancelTask 不会）
	}{
		{
			name: "窗口内并发删除（delete 变体：无兜底，修复前真实泄漏）",
			abandon: func(t *testing.T, mgr *CloudDownloadManager, owner, taskID string) {
				t.Helper()
				if err := mgr.DeleteTask(taskID, owner); err != nil {
					t.Fatalf("窗口内 DeleteTask: %v", err)
				}
			},
			wantMissing: true,
		},
		{
			name: "窗口内并发取消（cancel 变体：修复前由 cleanupExpiredOnce 兜底、有界）",
			abandon: func(t *testing.T, mgr *CloudDownloadManager, owner, taskID string) {
				t.Helper()
				if err := mgr.CancelTask(taskID, owner); err != nil {
					t.Fatalf("窗口内 CancelTask: %v", err)
				}
			},
			wantStatus: "cancelled",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			re := newResumeTenantEnv(t)
			mgr, owner := re.mgr, "carol"

			// 前置：续传失败的任务保留 90 字节 .partial ⇒ 全局账本与租户 Scope 都记 90。
			task, err := mgr.CreateTask("url", "https://example.com/window.bin", "window.bin", 90, owner)
			if err != nil {
				t.Fatalf("CreateTask: %v", err)
			}
			taskDir := filepath.Join(mgr.CloudDirFor(owner), task.ID)
			if err = os.MkdirAll(taskDir, 0o755); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err = os.WriteFile(filepath.Join(taskDir, "window.bin.partial"), make([]byte, 90), 0o644); err != nil {
				t.Fatalf("写 .partial: %v", err)
			}
			mgr.failTask(task, "simulated failure")
			if got := re.scopeUsage(owner); got != 90 {
				t.Fatalf("前置条件不成立：failTask 后租户 Scope=%d want 90", got)
			}

			// 在 ResumeTask 的 unlock→relock 窗口里注入本次放弃。
			origHook := resumeWindowHook
			resumeWindowHook = func(m *CloudDownloadManager, taskID string) {
				if taskID == task.ID {
					tc.abandon(t, m, owner, taskID)
				}
			}
			t.Cleanup(func() { resumeWindowHook = origHook })

			re.tenantOff.Store(true) // 让 ResumeTask 落在「租户不可用」回滚路径上
			if err := mgr.ResumeTask(task.ID, false, owner); err == nil {
				t.Fatal("租户不可用时 ResumeTask 应失败")
			}
			lifecycleNoRunningMark(t, mgr, task.ID, "ResumeTask(窗口内并发放弃)")

			// 核心断言：回滚必须补齐「被推迟给不存在的 goroutine」的释放。
			if got := re.scopeUsage(owner); got != 0 {
				t.Fatalf("回滚必须补齐被推迟的租户 Scope 释放：Scope=%d want 0（否则释放点只剩一个不存在的 goroutine）", got)
			}
			if got := re.cloudUsage(); got != 0 {
				t.Fatalf("回滚后全局账本=%d want 0（为负说明双释放）", got)
			}

			// 放弃本身的终态语义不变：删除后任务消失、取消后停在 cancelled。
			mgr.mu.RLock()
			_, exists := mgr.tasks[task.ID]
			mgr.mu.RUnlock()
			if exists == tc.wantMissing {
				t.Fatalf("窗口内放弃后任务存在性=%v，want exist=%v", exists, !tc.wantMissing)
			}
			if tc.wantStatus != "" {
				snap, ok := mgr.SnapshotTask(task.ID, owner)
				if !ok || snap.Status != tc.wantStatus {
					t.Fatalf("窗口内放弃后的终态应保持 %q, got %+v", tc.wantStatus, snap)
				}
			}
		})
	}
}
