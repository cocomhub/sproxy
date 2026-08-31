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
	mgr := NewManager(t.TempDir(), quota, 0, remotes, exec, discardLogger(), cfg)
	t.Cleanup(mgr.Stop)
	return mgr
}

// waitForStatus 轮询任务状态直到达到 want（或超时）。
func waitForStatus(t *testing.T, mgr *Manager, id, want string, timeout time.Duration) *SyncTask {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task := mgr.Get(id)
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
	if task := mgr.Get(id); task != nil {
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

func TestCreateTask_QuotaFull(t *testing.T) {
	quota := newMockQuota(1024) // 远小于 1 GiB 占位
	mgr := newTestManager(t, quota, nil, nil, nil)
	_, _, err := mgr.CreateTask(CreateRequest{Direction: "pull", Remote: "r1"})
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("应返回 ErrStorageFull，got %v", err)
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
	if st := mgr.Get(task.ID).Status; st != StatusPending && st != StatusSyncing {
		t.Fatalf("SubmitAndStart 返回后任务应处于 pending 或 syncing（执行中），got %q", st)
	}

	// 执行器阻塞 → 任务停在 syncing。用 started 信号确定性等 Run 被调用
	// （= 已拿信号量、进入 syncing），而非死等固定超时轮询状态。
	waitStarted(t, blocking)
	if got := mgr.Get(task.ID).Status; got != StatusSyncing {
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
	taskB, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dirb"})
	if err != nil {
		t.Fatal(err)
	}

	// A 取得唯一信号量进入 syncing；B 排队保持 pending。
	// 用 started 信号确定性等 A 的 executor.Run 被调用（= 已持有信号量、进入 syncing）。
	waitStarted(t, blocking)
	b := mgr.Get(taskB.ID)
	if b.Status != StatusPending {
		t.Fatalf("B 排队期间应保持 pending，got %q", b.Status)
	}
	if err := mgr.CancelTask(taskB.ID); err != nil {
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
	if err := mgr.CancelTask(task.ID); err != nil {
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
	persistFile := filepath.Join(mgr.persistDir, task.ID+".json")
	if _, err := os.Stat(persistFile); err != nil {
		t.Fatalf("持久化文件应存在: %v", err)
	}

	if err := mgr.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if mgr.Get(task.ID) != nil {
		t.Fatal("删除后任务应不可见")
	}
	if quota.Usage() != 0 {
		t.Fatalf("删除后应释放配额，got %d", quota.Usage())
	}
	if _, err := os.Stat(persistFile); !os.IsNotExist(err) {
		t.Fatalf("删除后持久化文件应移除: %v", err)
	}
	if err := mgr.DeleteTask(task.ID); err == nil {
		t.Fatal("二次删除应报 not found")
	}
}

// ---------------------------------------------------------------------------
// 配额对账
// ---------------------------------------------------------------------------

func TestQuota_ReconcileOnComplete_Pull(t *testing.T) {
	quota := newMockQuota(0)
	mgr := newTestManager(t, quota, nil, nil, nil)

	task, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "pull", Remote: "r1", Src: ""})
	if err != nil {
		t.Fatal(err)
	}
	if quota.Usage() != syncReservePlaceholder {
		t.Fatalf("创建时应预留占位 %d，got %d", syncReservePlaceholder, quota.Usage())
	}
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
	dir := t.TempDir()
	blocking := newBlockingMockExecutor()

	// 第一个管理器：创建 pull 任务，人为置 syncing 并落盘
	mgr1 := NewManager(dir, newMockQuota(0), 0, []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
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
	mgr2 := NewManager(dir, newMockQuota(0), 0, []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")},
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
	taskB, _, err := mgr.SubmitAndStart(CreateRequest{Direction: "push", Remote: "r1", Src: "dirb"})
	if err != nil {
		t.Fatal(err)
	}

	// 确定性同步（对齐 flaky-network-test-pattern）：用 channel 信号等 taskA 的
	// executor.Run 被调用（= 已拿到信号量、开始执行），而非死等固定超时轮询状态——
	// CI Windows -race + cover 下 goroutine 启动/状态流转慢，死等超时必然 flake
	// （5s→15s 仍破过）。started 信号由 mockExecutor.Run 首次调用时 close。
	select {
	case <-blocking.started:
	case <-time.After(30 * time.Second):
		t.Fatalf("taskA 未在 30s 内开始执行（executor.Run 未被调用）")
	}
	// taskA 已持有信号量（MaxConcurrent=1），taskB 必 pending（确定性，无时序依赖）。
	if b := mgr.Get(taskB.ID); b.Status != StatusPending {
		t.Fatalf("MaxConcurrent=1 时第二个任务应排队 pending，got %q", b.Status)
	}
	blocking.release()
	// release 后 taskA 的 Run 返回 → 回填完成 → 释放信号量 → taskB 执行并完成。
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
	metas := mgr.List()
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
	if n := len(mgr.List()); n != 1 {
		t.Fatalf("任务列表应有 1 个，got %d", n)
	}
	exec.release()
}

// TestRecoveredPullTask_NoDoubleReserve 验证恢复的 pull 任务完成对账不重新 TryReserve
// （审查 I-2 回归：磁盘已由启动扫描记账，二次预留会配额虚高/瞬时 507）。
func TestRecoveredPullTask_NoDoubleReserve(t *testing.T) {
	uploadsDir := t.TempDir()
	persistDir := filepath.Join(uploadsDir, syncDirName)
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
	mgr := NewManager(uploadsDir, quota, 0,
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
