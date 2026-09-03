// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// testRemote 构造一个带假凭据的 RemoteConfig（mock 执行器不真正访问网络）。
func testRemote(name, url string) RemoteConfig {
	return RemoteConfig{Name: name, URL: url, AccessKey: "test-ak", AccessKeySecret: strings.Repeat("a", 64)}
}

// completedResult 返回一个"完成"的 RunResult（1 文件 5 字节）。
func completedResult() *RunResult {
	return &RunResult{
		Status:     StatusCompleted,
		FilesTotal: 1,
		FilesDone:  1,
		BytesTotal: 5,
		BytesDone:  5,
		Results:    []SyncFileResult{{Path: "f.txt", Action: "created", Size: 5}},
	}
}

// newTestManager 创建测试管理器（默认：无配额上限、单远程 r1、mock 执行器立即完成）。
// 租户根解析器由测试基目录派生（newTestTenantRoot）。
func newTestManager(t *testing.T, quota *mockQuota, remotes []RemoteConfig, exec Executor, cfg *Config) *Manager {
	t.Helper()
	if quota == nil {
		quota = newMockQuota(0)
	}
	if cfg == nil {
		cfg = &Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour}
	}
	if exec == nil {
		exec = newMockExecutor(completedResult())
	}
	if remotes == nil {
		remotes = []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")}
	}
	tenantRoot, listTenants := newTestTenantRoot(t.TempDir())
	mgr := NewManager(tenantRoot, listTenants, quota, 0, remotes, exec, discardLogger(), cfg)
	t.Cleanup(mgr.Stop)
	return mgr
}

// waitForStatus 轮询任务状态直到达到 want（或超时）。
func waitForStatus(t *testing.T, mgr *Manager, id, want string, timeout time.Duration) *SyncTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task := mgr.Get(id, "")
		if task == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if task.Status == want {
			return task
		}
		if task.Status == "failed" && want != "failed" {
			t.Fatalf("task %s 失败（want %s）: %s", id, want, task.Error)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cur := "<deleted>"
	if task := mgr.Get(id, ""); task != nil {
		cur = task.Status
	}
	t.Fatalf("task %s 未在 %v 内达到 %s，当前 %v", id, timeout, want, cur)
	return nil
}

// waitStarted 确定性等待 mock 执行器的 Run 被调用（= 任务已拿信号量、进入执行）。
// 对齐 flaky-network-test-pattern：用 channel 信号而非死等固定超时轮询状态——
// CI Windows -race + cover 下 goroutine 启动/状态流转慢，死等必然 flake。
func waitStarted(t *testing.T, m *mockExecutor) {
	t.Helper()
	if m.started == nil {
		t.Fatal("waitStarted 需要带 started 信号的 mock 执行器（newBlockingMockExecutor）")
	}
	select {
	case <-m.started:
	case <-time.After(30 * time.Second):
		t.Fatalf("executor.Run 未被调用（任务未在 30s 内开始执行）")
	}
}

// ---------------------------------------------------------------------------
// CreateTask 校验
// ---------------------------------------------------------------------------

func TestCreateTask_RemoteMissing(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "nope"})
	if err == nil {
		t.Fatal("未配置的 remote 应报错")
	}
	if !strings.Contains(err.Error(), "nope") {
		t.Fatalf("错误应包含 remote 名: %v", err)
	}
}

func TestCreateTask_RemoteBadURL(t *testing.T) {
	mgr := newTestManager(t, nil, []RemoteConfig{testRemote("r1", "not-a-url")}, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1"})
	if err == nil {
		t.Fatal("非法 URL 的 remote 应报错")
	}
}

func TestCreateTask_RemoteNoCredentials(t *testing.T) {
	mgr := newTestManager(t, nil, []RemoteConfig{{Name: "r1", URL: "http://127.0.0.1:1"}}, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1"})
	if err == nil {
		t.Fatal("未配置凭据的 remote 创建任务应 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "access_key") {
		t.Fatalf("错误应提及 access_key: %v", err)
	}
}

func TestCreateTask_InvalidDirection(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "sideways", Remote: "r1"})
	if err == nil {
		t.Fatal("非法 direction 应报错")
	}
}

func TestCreateTask_InvalidConflictPolicy(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", ConflictPolicy: "clobber"})
	if err == nil {
		t.Fatal("非法 conflict_policy 应报错")
	}
}

func TestCreateTask_AbsolutePath(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	for _, tc := range []struct {
		name string
		req  CreateRequest
	}{
		{"src absolute", CreateRequest{Direction: "push", Remote: "r1", Src: "/etc/passwd"}},
		{"src drive", CreateRequest{Direction: "push", Remote: "r1", Src: "C:\\evil"}},
		{"dst absolute", CreateRequest{Direction: "push", Remote: "r1", Dst: "/tmp"}},
		{"src traversal", CreateRequest{Direction: "push", Remote: "r1", Src: "../etc"}},
	} {
		if _, _, err := mgr.CreateTask(tc.req); err == nil {
			t.Fatalf("%s: 应拒绝", tc.name)
		}
	}
}

// TestValidateSyncPath 锁定 validateSyncPath 在布局迁移后的真实语义：
// 仍拒绝路径穿越/绝对路径/盘符/空字节；不再拒绝功能桶名与 .__ 前缀段（用户路径在
// user 桶内与功能桶物理隔离，审查 I-3 的原 .__ 拒绝逻辑已随迁移删除）。"." 段按
// 当前实现不被特殊拒绝（非 ".."），测试锁定实际行为。
func TestValidateSyncPath(t *testing.T) {
	t.Run("Rejected", func(t *testing.T) {
		for _, tc := range []struct {
			name, p string
		}{
			{"traversal single", ".."},
			{"traversal nested", "a/../b"},
			{"traversal deep", "a/../../b"},
			{"traversal leading", "../etc"},
			{"traversal backslash", `..\etc`},
			{"absolute posix", "/abs"},
			{"absolute drive", "C:\\abs"},
			{"absolute drive slash", "C:/abs"},
			{"nul byte", "fi\x00le"},
		} {
			if err := validateSyncPath(tc.p, "src"); err == nil {
				t.Errorf("%s: %q 应被拒绝", tc.name, tc.p)
			}
		}
	})

	t.Run("Accepted", func(t *testing.T) {
		for _, tc := range []struct {
			name, p string
		}{
			{"fs root", ""},
			{"plain file", "dir/file.txt"},
			{"subdir", "a/b/c.txt"},
			{"with spaces", "sub dir/file.txt"},
			{"dot segment", "./meta/x"}, // "." 段不特殊拒绝（非 ".."），锁定实际行为
			{"bucket user", "user"},
			{"bucket meta", "meta"},
			{"bucket cloud", "cloud"},
			{"bucket archive", "archive"},
			{"bucket chunk", "chunk"},
			{"bucket version", "version"},
			{"legacy magic prefix", ".__foo"},
			{"legacy magic nested", "dir/.__versions__/x"},
		} {
			if err := validateSyncPath(tc.p, "dst"); err != nil {
				t.Errorf("%s: %q 不应被拒绝，got %v", tc.name, tc.p, err)
			}
		}
	})
}

func TestCreateTask_Dedup(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	req := CreateRequest{Direction: "push", Remote: "r1", Src: "dir", Dst: "dstdir", Recursive: true}
	t1, _, err := mgr.CreateTask(req)
	if err != nil {
		t.Fatal(err)
	}
	t2, _, err := mgr.CreateTask(req)
	if err != nil {
		t.Fatal(err)
	}
	if t1.ID != t2.ID {
		t.Fatalf("同 direction+remote+src+dst 应去重复用，got %q vs %q", t1.ID, t2.ID)
	}
	req.Dst = "other"
	t3, _, err := mgr.CreateTask(req)
	if err != nil {
		t.Fatal(err)
	}
	if t3.ID == t1.ID {
		t.Fatal("不同 dst 应创建新任务")
	}
}

func TestCreateTask_QuotaPull(t *testing.T) {
	quota := newMockQuota(0)
	mgr := newTestManager(t, quota, nil, nil, nil)
	if _, _, err := mgr.CreateTask(CreateRequest{Direction: "pull", Remote: "r1"}); err != nil {
		t.Fatal(err)
	}
	if quota.Usage() != syncReservePlaceholder {
		t.Fatalf("pull 方向应预留占位大小 %d，got %d", syncReservePlaceholder, quota.Usage())
	}
}

func TestCreateTask_QuotaPush(t *testing.T) {
	quota := newMockQuota(0)
	mgr := newTestManager(t, quota, nil, nil, nil)
	if _, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1"}); err != nil {
		t.Fatal(err)
	}
	if quota.Usage() != 0 {
		t.Fatalf("push 方向不应本地预留，got %d", quota.Usage())
	}
}

// TestCreateTask_QuotaHeadroomDegrades 验证占位预留在配额不足时降级而非拒绝：
// owner_quota < 1GiB 的租户（或全局余量 < 1GiB）仍可创建 pull 任务（ReservedSize=0 按需预留），
// 配额由 reconcileQuotaLocked 在下载字节实际到达时强制（见 TestReconcileQuota_TryReserveFailOnCompletion）。
func TestCreateTask_QuotaHeadroomDegrades(t *testing.T) {
	quota := newMockQuota(1024) // 远小于 1 GiB 占位 → 头部预占失败，降级为按需预留
	mgr := newTestManager(t, quota, nil, nil, nil)
	task, _, err := mgr.CreateTask(CreateRequest{Direction: "pull", Remote: "r1"})
	if err != nil {
		t.Fatalf("占位预留在配额不足时应降级而非拒绝，got %v", err)
	}
	if task.ReservedSize != 0 {
		t.Fatalf("降级后 ReservedSize 应为 0（按需预留），got %d", task.ReservedSize)
	}
	if quota.Usage() != 0 {
		t.Fatalf("降级后 quota 不应有预留，got %d", quota.Usage())
	}
}

// ---------------------------------------------------------------------------
// SubmitAndStart / 状态机
// ---------------------------------------------------------------------------

func TestSubmitAndStart_StateMachine(t *testing.T) {
	blocking := newBlockingMockExecutor()
	mgr := newTestManager(t, nil, nil, blocking, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dira"})
	if err != nil {
		t.Fatal(err)
	}
	// 注意：SubmitAndStart 返回的是内部共享指针，后台 goroutine 可能已把状态推进到
	// syncing，直接读 task.Status 是数据竞争（-race 偶发捕获）。用 mgr.Get 取加锁快照：
	// 刚提交的任务必为 pending，或已被后台 goroutine 快速推进到 syncing（均非终态，合法）。
	if st := mgr.Get(task.ID, "").Status; st != StatusPending && st != StatusSyncing {
		t.Fatalf("SubmitAndStart 返回后任务应处于 pending 或 syncing（执行中），got %q", st)
	}

	// 执行器阻塞 → 任务停在 syncing。用 started 信号确定性等 Run 被调用
	// （= 已拿信号量、进入 syncing），而非死等固定超时轮询状态。
	waitStarted(t, blocking)
	if got := mgr.Get(task.ID, "").Status; got != StatusSyncing {
		t.Fatalf("执行器阻塞期间任务应停在 syncing，got %q", got)
	}

	blocking.release()
	done := waitForStatus(t, mgr, task.ID, "completed", 10*time.Second)
	if done.FilesDone != 1 || done.FilesTotal != 1 {
		t.Fatalf("进度字段不符: files=%d/%d", done.FilesDone, done.FilesTotal)
	}
	if done.BytesDone != 5 {
		t.Fatalf("BytesDone 应为 5，got %d", done.BytesDone)
	}
	// 执行器应收到正确的任务与远程
	if blocking.lastTask == nil || blocking.lastTask.Remote != "r1" || blocking.lastTask.Direction != "push" {
		t.Fatalf("执行器收到的任务不符: %+v", blocking.lastTask)
	}
}

func TestSubmitAndStart_ExecutorError_Fails(t *testing.T) {
	mock := newMockExecutor(nil)
	mock.err = errors.New("boom")
	mgr := newTestManager(t, nil, nil, mock, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForStatus(t, mgr, task.ID, "failed", 5*time.Second)
	if !strings.Contains(failed.Error, "boom") {
		t.Fatalf("失败任务应记录执行器错误: %q", failed.Error)
	}
}

func TestSubmitAndStart_ExecutorNilResult_Fails(t *testing.T) {
	mock := newMockExecutor(nil)
	mgr := newTestManager(t, nil, nil, mock, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "failed", 5*time.Second)
}

// ---------------------------------------------------------------------------
// 取消 / 删除
// ---------------------------------------------------------------------------

func TestCancelTask_Queued(t *testing.T) {
	blocking := newBlockingMockExecutor()
	mgr := newTestManager(t, nil, nil, blocking, &Config{MaxConcurrent: 1, TaskTTL: time.Hour})

	taskA, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dira"})
	if err != nil {
		t.Fatal(err)
	}
	// 用 started 信号确定性等 A 的 executor.Run 被调用（= A 已拿到唯一信号量、进入 syncing）。
	// 注意：started 信号只保证「某个任务」的 Run 被调用，若 A/B 都先提交，B 也可能先抢到
	// 信号量（goroutine 调度非确定，CI 已复现）。故先等 A 拿到信号量，再提交 B——此时 B 必排队。
	waitStarted(t, blocking)
	if got := mgr.Get(taskA.ID, "").Status; got != StatusSyncing {
		t.Fatalf("A 应持有信号量进入 syncing，got %q", got)
	}

	taskB, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dirb"})
	if err != nil {
		t.Fatal(err)
	}
	// B 在 A 持有唯一信号量后才提交 → 必排队保持 pending（无时序依赖）。
	b := mgr.Get(taskB.ID, "")
	if b.Status != StatusPending {
		t.Fatalf("B 排队期间应保持 pending，got %q", b.Status)
	}
	if err := mgr.CancelTask(taskB.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, taskB.ID, "cancelled", 5*time.Second)

	// A 不受影响，释放后完成
	blocking.release()
	waitForStatus(t, mgr, taskA.ID, "completed", 10*time.Second)
}

func TestCancelTask_Running(t *testing.T) {
	blocking := newBlockingMockExecutor()
	mgr := newTestManager(t, nil, nil, blocking, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	// 用 started 信号确定性等 executor.Run 被调用（= 已持有信号量、进入 syncing）。
	waitStarted(t, blocking)
	if err := mgr.CancelTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "cancelled", 10*time.Second)
}

func TestDeleteTask(t *testing.T) {
	quota := newMockQuota(0)
	mgr := newTestManager(t, quota, nil, nil, nil)

	task, _, err := mgr.CreateTask(CreateRequest{Direction: "pull", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	if quota.Usage() != syncReservePlaceholder {
		t.Fatalf("创建 pull 任务应预留配额，got %d", quota.Usage())
	}
	persistFile := filepath.Join(mgr.persistDirFor(task.Owner), task.ID+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("持久化文件应存在: %v", err)
	}

	if err := mgr.DeleteTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(task.ID, "") != nil {
		t.Fatal("删除后任务应不可见")
	}
	if quota.Usage() != 0 {
		t.Fatalf("删除后应释放配额，got %d", quota.Usage())
	}
	if _, err := os.Stat(persistFile); !os.IsNotExist(err) {
		t.Fatalf("删除后持久化文件应移除: %v", err)
	}
	if err := mgr.DeleteTask(task.ID, ""); err == nil {
		t.Fatal("二次删除应报 not found")
	}
}

// ---------------------------------------------------------------------------
// 配额对账
// ---------------------------------------------------------------------------

func TestQuota_ReconcileOnComplete_Pull(t *testing.T) {
	quota := newMockQuota(0)
	// 用阻塞 executor：任务在 Run 内阻塞（不会立即对账），使"创建时预留占位"断言
	// 确定成立（而非依赖 SubmitAndStart 返回后 goroutine 尚未完成的时序）。
	// release 后返回 completed（1 文件 5 字节），对账应收敛到 5。
	exec := newBlockingMockExecutor()
	mgr := newTestManager(t, quota, nil, exec, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	// 等 Run 真正开始（已拿信号量）后再断言占位，杜绝"任务已完成/未启动"竞态。
	<-exec.started
	if quota.Usage() != syncReservePlaceholder {
		t.Fatalf("创建时应预留占位 %d，got %d", syncReservePlaceholder, quota.Usage())
	}
	exec.release()
	waitForStatus(t, mgr, task.ID, "completed", 5*time.Second)
	// 完成后按 BytesDone 对账：只预留实际 5 字节
	if quota.Usage() != 5 {
		t.Fatalf("完成后配额应收敛到实际 5 字节，got %d", quota.Usage())
	}
}

func TestQuota_ReconcileOnFail_Pull(t *testing.T) {
	quota := newMockQuota(0)
	mock := newMockExecutor(nil)
	mock.err = errors.New("transfer failed")
	mgr := newTestManager(t, quota, nil, mock, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, "failed", 5*time.Second)
	if quota.Usage() != 0 {
		t.Fatalf("失败后应释放预留配额，got %d", quota.Usage())
	}
}

// ---------------------------------------------------------------------------
// 重启恢复
// ---------------------------------------------------------------------------

func TestRecoverTasks_RestartSyncing(t *testing.T) {
	base := t.TempDir()
	tenantRoot, listTenants := newTestTenantRoot(base)
	blocking := newBlockingMockExecutor()

	// 第一个管理器：创建 pull 任务，人为置 syncing 并落盘
	mgr1 := NewManager(tenantRoot, listTenants, newMockQuota(0), 0, []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
		blocking, discardLogger(), &Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	task, _, err := mgr1.CreateTask(CreateRequest{Direction: "pull", Remote: "r1", Src: "", Dst: "restored"})
	if err != nil {
		t.Fatal(err)
	}
	task.Status = StatusSyncing
	task.UpdatedAt = time.Now()
	if err := mgr1.saveTask(task); err != nil {
		t.Fatal(err)
	}
	mgr1.Stop()

	// 第二个管理器：recoverTasks 只重启 syncing 任务
	mgr2 := NewManager(tenantRoot, listTenants, newMockQuota(0), 0, []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
		blocking, discardLogger(), &Config{MaxConcurrent: 3, TaskTTL: time.Hour})
	t.Cleanup(mgr2.Stop)

	// 恢复的 syncing 任务应重启执行：用 started 信号确定性等 executor.Run 被调用。
	waitStarted(t, blocking)
	blocking.release()
	recovered := waitForStatus(t, mgr2, task.ID, "completed", 10*time.Second)
	if recovered.Direction != "pull" || recovered.Src != "" || recovered.Dst != "restored" {
		t.Fatalf("恢复的任务应保留字段: %+v", recovered)
	}
	if blocking.callCount() < 1 {
		t.Fatalf("恢复的 syncing 任务应重新执行（executor 至少被调用一次）")
	}
}

// ---------------------------------------------------------------------------
// 并发信号量
// ---------------------------------------------------------------------------

func TestConcurrency_Semaphore(t *testing.T) {
	blocking := newBlockingMockExecutor()
	mgr := newTestManager(t, nil, nil, blocking, &Config{MaxConcurrent: 1, TaskTTL: time.Hour})

	taskA, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dira"})
	if err != nil {
		t.Fatal(err)
	}

	// 确定性同步（对齐 flaky-network-test-pattern）：用 channel 信号等 A 的 executor.Run
	// 被调用（= A 已拿到信号量、开始执行），而非死等固定超时轮询状态——CI Windows -race +
	// cover 下 goroutine 启动/状态流转慢，死等超时必然 flake（5s→15s 仍破过）。started 信号
	// 由 mockExecutor.Run 首次调用时 close。
	// 注意：started 只保证「某个任务」的 Run 被调用。若 A/B 都先提交，B 也可能先抢到信号量
	// （goroutine 调度非确定，CI 已复现 B 先 syncing 而 A pending）。故先等 A 拿到信号量，
	// 再提交 B——此时 B 必排队 pending（真正无时序依赖）。
	waitStarted(t, blocking)
	if got := mgr.Get(taskA.ID, "").Status; got != StatusSyncing {
		t.Fatalf("A 应持有信号量进入 syncing，got %q", got)
	}

	taskB, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dirb"})
	if err != nil {
		t.Fatal(err)
	}
	if b := mgr.Get(taskB.ID, ""); b.Status != StatusPending {
		t.Fatalf("MaxConcurrent=1 时第二个任务应排队 pending，got %q", b.Status)
	}
	blocking.release()
	// release 后 A 的 Run 返回 → 回填完成 → 释放信号量 → B 执行并完成。
	// completed 是「任务确定在跑」后的收尾状态，30s 宽限足够（本地 0.02s，CI 慢也秒级）。
	waitForStatus(t, mgr, taskA.ID, "completed", 30*time.Second)
	waitForStatus(t, mgr, taskB.ID, "completed", 30*time.Second)
}

// ---------------------------------------------------------------------------
// List
// ---------------------------------------------------------------------------

func TestList_ReturnsMeta(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, nil)
	task, _, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "x"})
	if err != nil {
		t.Fatal(err)
	}
	metas := mgr.List("")
	if len(metas) != 1 {
		t.Fatalf("List 应有 1 个任务，got %d", len(metas))
	}
	m := metas[0]
	if m.ID != task.ID || m.Direction != "push" || m.Status != StatusPending {
		t.Fatalf("meta 字段不符: %+v", m)
	}
}

// TestCreateTask_ConcurrentDedup 验证并发同 key 的 CreateTask/SubmitAndStart 只创建一个
// 新任务（审查 I-1 回归：写锁内去重闭合 TOCTOU，避免双任务并发写同一 dst 路径）。
// 用 blocking executor 使首个任务保持 syncing，后续请求能命中去重。
func TestCreateTask_ConcurrentDedup(t *testing.T) {
	exec := newBlockingMockExecutor()
	mgr := newTestManager(t, nil, nil, exec, nil)
	req := CreateRequest{Direction: "push", Remote: "r1", Src: "a", Dst: "b"}

	var wg sync.WaitGroup
	var mu sync.Mutex
	newCount := 0
	for range 10 {
		wg.Go(func() {
			_, isNew, err := mgr.SubmitAndStart(req)
			if err != nil {
				t.Errorf("SubmitAndStart error: %v", err)
				return
			}
			if isNew {
				mu.Lock()
				newCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if newCount != 1 {
		t.Fatalf("并发同 key 应只有 1 个新建任务，got %d", newCount)
	}
	// 任务列表应只有 1 个活跃任务
	if n := len(mgr.List("")); n != 1 {
		t.Fatalf("任务列表应有 1 个，got %d", n)
	}
	exec.release()
}

// TestRecoveredPullTask_NoDoubleReserve 验证恢复的 pull 任务完成对账不重新 TryReserve
// （审查 I-2 回归：磁盘已由启动扫描记账，二次预留会配额虚高/瞬时 507）。
func TestRecoveredPullTask_NoDoubleReserve(t *testing.T) {
	base := t.TempDir()
	tenantRoot, listTenants := newTestTenantRoot(base)
	// 匿名租户（owner==""）的 meta/sync 持久化目录
	persistDir := filepath.Join(base, "anonymous", "meta", "sync")
	if err := os.MkdirAll(persistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃前持久化的 syncing pull 任务
	persisted := &SyncTask{
		ID: "sync-test-recover-1", Direction: string(DirectionPull), Remote: "r1",
		Status: StatusSyncing, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persistDir, persisted.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	quota := newMockQuota(0)
	mgr := NewManager(tenantRoot, listTenants, quota, 0,
		[]RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
		newMockExecutor(completedResult()), discardLogger(), &Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour})
	defer mgr.Stop()

	waitForStatus(t, mgr, persisted.ID, StatusCompleted, 5*time.Second)
	// 恢复的 pull 任务完成：Restored 路径不 TryReserve/Release，quota 保持 0
	if got := quota.Usage(); got != 0 {
		t.Fatalf("恢复的 pull 任务完成不应动配额（磁盘扫描已记账），quota.used=%d", got)
	}
}

// TestReconcileQuota_TryReserveFailOnCompletion 验证 pull 完成时实际写入超过配额余量
// → 释放占位 + 任务 failed（不破坏已写入文件）。
func TestReconcileQuota_TryReserveFailOnCompletion(t *testing.T) {
	big := &RunResult{Status: StatusCompleted, FilesTotal: 1, FilesDone: 1,
		BytesTotal: syncReservePlaceholder + 200, BytesDone: syncReservePlaceholder + 200}
	// 占位 1GiB 可预留；完成后需补 200，但余量只有 100 → TryReserve 失败
	quota := newMockQuota(syncReservePlaceholder + 100)
	mgr := newTestManager(t, quota, nil, newMockExecutor(big), nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1"})
	if err != nil {
		t.Fatalf("SubmitAndStart 应成功（占位可预留）: %v", err)
	}
	tk := waitForStatus(t, mgr, task.ID, StatusFailed, 5*time.Second)
	if !strings.Contains(tk.Error, "storage full") {
		t.Fatalf("Error 应含 storage full，got %q", tk.Error)
	}
	// 占位已释放，quota 归 0
	if got := quota.Usage(); got != 0 {
		t.Fatalf("失败后占位应释放，quota.used=%d", got)
	}
}

// panicExecutor 在 Run 中 panic，验证 Manager 的 panic recovery。
type panicExecutor struct{}

func (panicExecutor) Run(context.Context, *SyncTask, RemoteConfig) (*RunResult, error) {
	panic("boom")
}

// TestExecutor_PanicRecovery 验证执行器 panic 被 recovery 捕获 → 任务 failed 而非挂死。
func TestExecutor_PanicRecovery(t *testing.T) {
	mgr := newTestManager(t, nil, nil, panicExecutor{}, nil)
	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	tk := waitForStatus(t, mgr, task.ID, StatusFailed, 5*time.Second)
	if !strings.Contains(tk.Error, "panic") {
		t.Fatalf("Error 应含 panic，got %q", tk.Error)
	}
}

// ---------------------------------------------------------------------------
// 自动重试（阶段 6 工作项 A）
// ---------------------------------------------------------------------------

// TestRetry_TransientErrorThenSuccess 验证瞬时网络错误自动重试：第 1 次 Run 返回
// Retryable 错误、第 2 次成功 → 任务最终 completed（不再需要手动重建）。
func TestRetry_TransientErrorThenSuccess(t *testing.T) {
	exec := newRetryMockExecutor([]*RunResult{
		{Status: StatusFailed, Retryable: true, Error: "网络错误: connection refused"},
		completedResult(),
	})
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: 10 * time.Millisecond, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitForStatus(t, mgr, task.ID, StatusCompleted, 10*time.Second)
	if exec.callCount() != 2 {
		t.Fatalf("瞬时错误应重试后成功，期望 2 次 Run，got %d", exec.callCount())
	}
	if done.Retries != 1 {
		t.Fatalf("重试计数应为 1，got %d", done.Retries)
	}
	if done.FilesDone != 1 || done.BytesDone != 5 {
		t.Fatalf("进度字段不符: files=%d/%d bytes=%d", done.FilesDone, done.FilesTotal, done.BytesDone)
	}
	if done.Error != "" {
		t.Fatalf("完成任务 Error 应为空，got %q", done.Error)
	}
}

// TestRetry_MaxRetriesExhausted 验证重试上限：mock 恒返回 Retryable → 达 MaxRetries
// 后转 failed，错误信息含"已重试 N 次"。
func TestRetry_MaxRetriesExhausted(t *testing.T) {
	exec := newRetryMockExecutor([]*RunResult{
		{Status: StatusFailed, Retryable: true, Error: "网络错误: connection refused"},
		{Status: StatusFailed, Retryable: true, Error: "网络错误: timeout"},
		{Status: StatusFailed, Retryable: true, Error: "网络错误: 5xx"},
	})
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 2, RetryDelay: 5 * time.Millisecond, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForStatus(t, mgr, task.ID, StatusFailed, 10*time.Second)
	if exec.callCount() != 3 {
		t.Fatalf("MaxRetries=2 应执行 3 次（初始+2 重试），got %d", exec.callCount())
	}
	if failed.Retries != 2 {
		t.Fatalf("重试计数应为 2，got %d", failed.Retries)
	}
	if !strings.Contains(failed.Error, "已重试 2 次") {
		t.Fatalf("失败错误应含已重试次数: %q", failed.Error)
	}
	if !strings.Contains(failed.Error, "5xx") {
		t.Fatalf("失败错误应包含最后一次错误文本: %q", failed.Error)
	}
}

// TestRetry_BackoffDelayComputation 验证指数退避延迟计算的纯函数（base * backoff^(n-1)，
// 封顶 base*10）。
func TestRetry_BackoffDelayComputation(t *testing.T) {
	mgr := newTestManager(t, nil, nil, nil, &Config{
		MaxConcurrent: 1, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: time.Second, RetryBackoff: 2,
	})
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{5, 10 * time.Second}, // 封顶 base*10
		{10, 10 * time.Second},
	}
	for _, tc := range cases {
		if got := mgr.retryBackoffDelay(tc.attempt); got != tc.want {
			t.Fatalf("retryBackoffDelay(%d) 应为 %v，got %v", tc.attempt, tc.want, got)
		}
	}
}

// TestRetry_BackoffTiming 验证重试间隔符合指数退避（仅断言下界：
// 第 1 次重试 >= base，第 2 次 >= base*2——-race/CI 负载只会让间隔变长，下界断言鲁棒）。
func TestRetry_BackoffTiming(t *testing.T) {
	base := 20 * time.Millisecond
	exec := newRetryMockExecutor([]*RunResult{
		{Status: StatusFailed, Retryable: true, Error: "e1"},
		{Status: StatusFailed, Retryable: true, Error: "e2"},
		completedResult(),
	})
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: base, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, StatusCompleted, 10*time.Second)
	gaps := exec.gaps()
	if len(gaps) != 2 {
		t.Fatalf("应有 2 个重试间隔，got %d: %v", len(gaps), gaps)
	}
	if gaps[0] < base {
		t.Fatalf("第 1 次重试间隔应 >= %v，got %v", base, gaps[0])
	}
	if gaps[1] < 2*base {
		t.Fatalf("第 2 次重试间隔应 >= %v（指数退避），got %v", 2*base, gaps[1])
	}
}

// TestRetry_StatusRetryingVisible 验证重试期间任务 Status == retrying：
// 第 1 次 Run 返回可重试错误后进入 retrying；第 2 次 Run 执行期间保持 retrying。
func TestRetry_StatusRetryingVisible(t *testing.T) {
	exec := newBlockingRetryExecutor()
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: 100 * time.Millisecond, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exec.started:
	case <-time.After(30 * time.Second):
		t.Fatal("第 1 次 Run 未被调用")
	}
	// 释放第 1 次 Run（返回可重试错误）→ 进入退避，状态应为 retrying
	exec.releaseFirst()
	waitForStatus(t, mgr, task.ID, StatusRetrying, 5*time.Second)
	// 退避结束后第 2 次 Run 开始执行，执行期间状态应保持 retrying
	select {
	case <-exec.secondStarted:
	case <-time.After(30 * time.Second):
		t.Fatal("第 2 次 Run 未被调用")
	}
	if got := mgr.Get(task.ID, "").Status; got != StatusRetrying {
		t.Fatalf("重试执行期间状态应为 retrying，got %q", got)
	}
	if got := mgr.Get(task.ID, "").Retries; got != 1 {
		t.Fatalf("重试计数应为 1，got %d", got)
	}
	exec.releaseSecond()
	waitForStatus(t, mgr, task.ID, StatusCompleted, 10*time.Second)
}

// TestRetry_BusinessErrorNoRetry 验证业务失败（Retryable=false）不重试，直接 failed。
func TestRetry_BusinessErrorNoRetry(t *testing.T) {
	exec := newRetryMockExecutor([]*RunResult{
		{Status: StatusFailed, Retryable: false, Error: "路径校验失败"},
	})
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: 5 * time.Millisecond, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	failed := waitForStatus(t, mgr, task.ID, StatusFailed, 5*time.Second)
	if exec.callCount() != 1 {
		t.Fatalf("业务失败不应重试，期望 1 次 Run，got %d", exec.callCount())
	}
	if !strings.Contains(failed.Error, "路径校验失败") {
		t.Fatalf("失败错误应保留业务错误: %q", failed.Error)
	}
	if failed.Retries != 0 {
		t.Fatalf("业务失败重试计数应为 0，got %d", failed.Retries)
	}
}

// TestRetry_CancelDuringBackoff 验证重试等待（退避）期间取消立即生效：
// 取消后任务转 cancelled，不再继续重试。
func TestRetry_CancelDuringBackoff(t *testing.T) {
	exec := newBlockingRetryExecutor()
	// 退避 1 小时：若不支持取消，任务会长时间卡在 retrying
	mgr := newTestManager(t, nil, nil, exec, &Config{
		MaxConcurrent: 3, TaskTTL: time.Hour, MaxRetries: 3, RetryDelay: time.Hour, RetryBackoff: 2,
	})

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exec.started:
	case <-time.After(30 * time.Second):
		t.Fatal("第 1 次 Run 未被调用")
	}
	exec.releaseFirst()
	// 进入 1 小时退避，状态为 retrying
	waitForStatus(t, mgr, task.ID, StatusRetrying, 5*time.Second)
	// 退避等待期间取消 → 立即 cancelled（不等退避结束）
	if err := mgr.CancelTask(task.ID, ""); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr, task.ID, StatusCancelled, 5*time.Second)
	if exec.callCount() != 1 {
		t.Fatalf("取消后不应再重试，期望 1 次 Run，got %d", exec.callCount())
	}
}

// TestRetry_RetriesPersisted 验证 retries 计数落盘：retrying 任务重启恢复后保留计数，
// 且 retrying 任务与 syncing 一样在重启后自动恢复执行。
func TestRetry_RetriesPersisted(t *testing.T) {
	base := t.TempDir()
	tenantRoot, listTenants := newTestTenantRoot(base)
	// 匿名租户（owner==""）的 meta/sync 持久化目录
	persistDir := filepath.Join(base, "anonymous", "meta", "sync")
	if err := os.MkdirAll(persistDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 模拟崩溃前持久化的 retrying 任务（重试计数 2）
	persisted := &SyncTask{
		ID: "sync-retry-1", Direction: string(DirectionPush), Remote: "r1",
		Status: StatusRetrying, Retries: 2, CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	data, err := json.Marshal(persisted)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(persistDir, persisted.ID+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	blocking := newBlockingMockExecutor()
	mgr := NewManager(tenantRoot, listTenants, newMockQuota(0), 0,
		[]RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
		blocking, discardLogger(), &Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour, MaxRetries: 3, RetryDelay: 10 * time.Millisecond, RetryBackoff: 2})
	defer mgr.Stop()

	// 恢复的 retrying 任务应重启执行（用 started 信号确定性等待，对齐恢复 syncing 的模式）
	waitStarted(t, blocking)
	if got := mgr.Get(persisted.ID, "").Retries; got != 2 {
		t.Fatalf("恢复后 Retries 应保留 2，got %d", got)
	}
	blocking.release()
	done := waitForStatus(t, mgr, persisted.ID, StatusCompleted, 10*time.Second)
	if done.Retries != 2 {
		t.Fatalf("完成后 Retries 应保持 2，got %d", done.Retries)
	}
}

// TestQuota_ReconcileOnFailed_Released 验证（审查 M-2）：pull 任务 failed 终态释放
// 预留配额（创建时 TryReserve 1GiB 占位），不永久钉住配额。
func TestQuota_ReconcileOnFailed_Released(t *testing.T) {
	quota := newMockQuota(0)
	// 非阻塞执行器，预先设 err（Run 立即失败 → runResult nil → failTask 释放配额）。
	mock := newMockExecutor(nil)
	mock.mu.Lock()
	mock.err = errors.New("transfer failed (business error)")
	mock.mu.Unlock()
	mgr := newTestManager(t, quota, nil, mock, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	// 确定性等待 failed（Run 立即失败 → failTask 释放配额）。
	done := waitForStatus(t, mgr, task.ID, StatusFailed, 5*time.Second)
	if done == nil {
		t.Fatal("业务错误应直接 failed")
	}
	// failed 终态应释放预留配额（M-2）。
	if quota.Usage() != 0 {
		t.Fatalf("failed 后预留配额应释放为 0，got %d", quota.Usage())
	}
}

// newTestManagerPF 与 newTestManager 相同但启用 PerFileReserve（逐文件 guard 模式）。
func newTestManagerPF(t *testing.T, quota *mockQuota) *Manager {
	t.Helper()
	if quota == nil {
		quota = newMockQuota(0)
	}
	cfg := &Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour, PerFileReserve: true}
	tenantRoot, listTenants := newTestTenantRoot(t.TempDir())
	mgr := NewManager(tenantRoot, listTenants, quota, 0,
		[]RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
		newMockExecutor(completedResult()), discardLogger(), cfg)
	t.Cleanup(mgr.Stop)
	return mgr
}

// TestReconcileQuota_PerFileReserve_ReleasesPlaceholderOnly 验证逐文件 guard 模式下
// completed 对账释放占位预留、不再按 BytesDone 补账（逐文件已等额入账，delta 对账会双计）。
func TestReconcileQuota_PerFileReserve_ReleasesPlaceholderOnly(t *testing.T) {
	quota := newMockQuota(0)
	mgr := newTestManagerPF(t, quota)
	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1"})
	if err != nil {
		t.Fatalf("创建应成功: %v", err)
	}
	// 模拟逐文件 guard 已把 5 字节（1 文件）入账到的 user 桶（mockQuota 单一计数器）：
	// 先补入 5（在 reconcile 前）。reconcile 应只释放占位 P，最终 used == 5。
	// 注意：mock 执行器立即完成，需在驱动前补入（用新任务可等待完成后再断言更稳——
	// 但 reconcile 在完成时读 BytesDone=5、仅释放 P，若补入 5 在 reconcile 后则被保留）。
	// 直接用另一个并发的 mock 执行器不可行（阻塞）；这里 CreateTask 后手动触发 reconcile
	// 更可控——但 CreateTask 不启动。改为：SubmitAndStart（自动执行一次完成）后再手动
	// 检查 reconcile 已经把占位释放到 0（BytesDone=5 补账被 guard 语义取代）。
	// 为确定性，这里只用 completedResult() 执行器（1 文件 5 字节），断言：
	// 完成前 quota.Used == P（占位）；完成后 == 5（BytesDone 由 guard 语义保留，占位释放）。
	done := waitForStatus(t, mgr, task.ID, StatusCompleted, 5*time.Second)
	if done == nil {
		t.Fatal("任务应完成")
	}
	// guard 语义：占位 P 已被释放（Release），completion 不补账；mock 里 BytesDone=5 保留。
	// 但 mock 执行器没有真的逐文件入账——它的 BytesDone=5 是虚构的"已完成字节"。在 guard
	// 模式 reconcile 只释放占位，因此 used = 0（没有 guard 入账）。这个断言区分"双计"：
	// 若 reconcile 仍按 delta 补账，会把 used 从 0 拉回字节数（错误）。
	if got := quota.Usage(); got != 0 {
		t.Fatalf("逐文件 guard 模式 completed 后 quota.used=%d want 0（安全：guard 未真实入账则不为 0；双计会 >0）", got)
	}
	if done.ReservedSize != 5 {
		t.Fatalf("ReservedSize 应记录 BytesDone=5, got %d", done.ReservedSize)
	}
}
