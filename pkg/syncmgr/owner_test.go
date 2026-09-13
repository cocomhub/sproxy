// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"errors"
	"testing"
	"time"
)

// ownerTestMgr 构造一个带假凭据远程、mock 执行器（立即完成）的测试管理器。
// 任务创建不需要真实网络（mock 执行器不访问远程），仅需 remote 配置通过校验。
func ownerTestMgr(t *testing.T) *Manager {
	t.Helper()
	return newTestManager(t, nil, nil, nil, nil)
}

// mustCreate 创建同步任务并在失败时 Fatal。
func mustCreate(t *testing.T, mgr *Manager, src, owner string) *SyncTask {
	t.Helper()
	task, _, err := mgr.CreateTask(CreateRequest{
		Direction: "push",
		Remote:    "r1",
		Src:       src,
		Owner:     owner,
	})
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	return task
}

// containsID 判断列表是否包含指定任务 ID。
func containsID(t *testing.T, metas []SyncTaskMeta, id string) bool {
	t.Helper()
	for _, m := range metas {
		if m.ID == id {
			return true
		}
	}
	return false
}

// TestCreateTask_WritesOwner 验证创建时把请求 owner 写入任务（DoD：创建带 owner）。
func TestCreateTask_WritesOwner(t *testing.T) {
	mgr := ownerTestMgr(t)

	taskA := mustCreate(t, mgr, "a.txt", "ak-A")
	if taskA.Owner != "ak-A" {
		t.Fatalf("Owner = %q, want %q", taskA.Owner, "ak-A")
	}

	taskB := mustCreate(t, mgr, "b.txt", "ak-B")
	if taskB.Owner != "ak-B" {
		t.Fatalf("Owner = %q, want %q", taskB.Owner, "ak-B")
	}

	// 未认证（owner 空）创建 → 空 owner
	taskG := mustCreate(t, mgr, "g.txt", "")
	if taskG.Owner != "" {
		t.Fatalf("Owner = %q, want 空（未认证）", taskG.Owner)
	}
}

// TestCreateTask_DedupScopedByOwner 验证去重按 owner 隔离：
// 同 owner 同参任务去重复用；跨 owner 同参任务各自新建（不吸收他人任务，防信息泄露）。
func TestCreateTask_DedupScopedByOwner(t *testing.T) {
	mgr := ownerTestMgr(t)

	// A 创建 push src=a.txt
	taskA, isNewA, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "a.txt", Owner: "ak-A"})
	if err != nil || !isNewA {
		t.Fatalf("A 首次创建应为新建: isNew=%v err=%v", isNewA, err)
	}

	// 同 owner 同参 → 去重复用
	taskA2, isNewA2, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "a.txt", Owner: "ak-A"})
	if err != nil || isNewA2 || taskA2.ID != taskA.ID {
		t.Fatalf("同 owner 同参应去重: isNew=%v id=%q want %q", isNewA2, taskA2.ID, taskA.ID)
	}

	// 跨 owner 同参 → 新建独立任务（不吸收 A 的任务）
	taskB, isNewB, err := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "a.txt", Owner: "ak-B"})
	if err != nil || !isNewB {
		t.Fatalf("B 跨 owner 同参应新建: isNew=%v err=%v", isNewB, err)
	}
	if taskB.ID == taskA.ID || taskB.Owner != "ak-B" {
		t.Fatalf("B 的任务应独立且 owner=ak-B, got %+v", taskB)
	}

	// 全局空 owner 任务可被任意用户去重吸收（全局兼容）
	taskG, isNewG, _ := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "g.txt", Owner: ""})
	if !isNewG {
		t.Fatal("全局任务应新建")
	}
	taskG2, isNewG2, _ := mgr.CreateTask(CreateRequest{Direction: "push", Remote: "r1", Src: "g.txt", Owner: "ak-A"})
	if isNewG2 || taskG2.ID != taskG.ID {
		t.Fatalf("A 应去重吸收全局空 owner 任务, got id=%q want %q", taskG2.ID, taskG.ID)
	}
}

// TestList_FiltersByOwner 验证列表按 owner 过滤：
// 请求者 owner 非空 → 只含匹配 owner 与空 owner（全局兼容）的任务；空 owner → 全部。
func TestList_FiltersByOwner(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskA := mustCreate(t, mgr, "a.txt", "ak-A")
	taskB := mustCreate(t, mgr, "b.txt", "ak-B")
	taskG := mustCreate(t, mgr, "g.txt", "")

	// A 的列表：A + 空 owner，不含 B
	aList := mgr.List("ak-A")
	if len(aList) != 2 {
		t.Fatalf("List(ak-A) 长度 = %d, want 2（A + 空 owner）: %+v", len(aList), aList)
	}
	if !containsID(t, aList, taskA.ID) || !containsID(t, aList, taskG.ID) {
		t.Fatalf("List(ak-A) 应含 A 与空 owner 任务: %+v", aList)
	}
	if containsID(t, aList, taskB.ID) {
		t.Fatalf("List(ak-A) 不应含 B 的任务: %+v", aList)
	}

	// B 的列表：B + 空 owner
	bList := mgr.List("ak-B")
	if len(bList) != 2 || !containsID(t, bList, taskB.ID) || !containsID(t, bList, taskG.ID) {
		t.Fatalf("List(ak-B) 不符: %+v", bList)
	}
	if containsID(t, bList, taskA.ID) {
		t.Fatalf("List(ak-B) 不应含 A 的任务: %+v", bList)
	}

	// 空 owner（管理员/未认证）→ 全部
	all := mgr.List("")
	if len(all) != 3 {
		t.Fatalf("List(\"\") 长度 = %d, want 3（全部）: %+v", len(all), all)
	}
	for _, want := range []string{taskA.ID, taskB.ID, taskG.ID} {
		if !containsID(t, all, want) {
			t.Fatalf("List(\"\") 应含 %s: %+v", want, all)
		}
	}
}

// TestList_MetaCarriesOwner 验证列表元信息带 owner（DoD：API 返回任务带 owner）。
func TestList_MetaCarriesOwner(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskA := mustCreate(t, mgr, "a.txt", "ak-A")

	all := mgr.List("")
	if len(all) != 1 {
		t.Fatalf("List 长度 = %d, want 1", len(all))
	}
	if all[0].Owner != "ak-A" {
		t.Fatalf("Meta.Owner = %q, want %q", all[0].Owner, "ak-A")
	}
	if all[0].ID != taskA.ID {
		t.Fatalf("Meta.ID = %q, want %q", all[0].ID, taskA.ID)
	}
}

// TestGet_FiltersByOwner 验证 Get 按 owner 过滤（IDOR 防护）：跨 owner 视为不存在。
func TestGet_FiltersByOwner(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskA := mustCreate(t, mgr, "a.txt", "ak-A")
	taskB := mustCreate(t, mgr, "b.txt", "ak-B")
	taskG := mustCreate(t, mgr, "g.txt", "")

	// A 拿自己的任务 → 命中
	if got := mgr.Get(taskA.ID, "ak-A"); got == nil {
		t.Fatal("Get(自己的任务) 应命中")
	}
	// A 拿空 owner 任务 → 命中（空 owner 全局可见）
	if got := mgr.Get(taskG.ID, "ak-A"); got == nil {
		t.Fatal("Get(空 owner 任务) 对认证用户应命中（全局兼容）")
	}
	// A 拿 B 的任务 → nil（不泄露存在性）
	if got := mgr.Get(taskB.ID, "ak-A"); got != nil {
		t.Fatalf("Get(B 的任务) 对 A 应返回 nil，got %+v", got)
	}
	// 空 owner 请求者拿任意任务 → 命中
	if got := mgr.Get(taskB.ID, ""); got == nil {
		t.Fatal("Get(任意任务) 对空 owner（管理员/未认证）应命中")
	}
	// 不存在任务 → nil
	if got := mgr.Get("does-not-exist", "ak-A"); got != nil {
		t.Fatal("Get(不存在任务) 应返回 nil")
	}
}

// TestCancelTask_IDOR 验证取消按 owner 过滤：跨 owner 取消返回 ErrNotFound 且任务状态不变。
func TestCancelTask_IDOR(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskB := mustCreate(t, mgr, "b.txt", "ak-B")

	if err := mgr.CancelTask(taskB.ID, "ak-A"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A 取消 B 的任务应返回 ErrNotFound，got %v", err)
	}
	// B 的任务应保持 pending（未被 A 取消）
	if got := mgr.Get(taskB.ID, "ak-B"); got == nil || got.Status != StatusPending {
		t.Fatalf("B 的任务应保持 pending，got %+v", got)
	}

	// 空 owner（管理员/未认证）可取消
	if err := mgr.CancelTask(taskB.ID, ""); err != nil {
		t.Fatalf("空 owner 取消任务应成功，got %v", err)
	}
}

// TestDeleteTask_IDOR 验证删除按 owner 过滤：跨 owner 删除返回 ErrNotFound 且任务仍存在。
func TestDeleteTask_IDOR(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskB := mustCreate(t, mgr, "b.txt", "ak-B")

	if err := mgr.DeleteTask(taskB.ID, "ak-A"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("A 删除 B 的任务应返回 ErrNotFound，got %v", err)
	}
	if got := mgr.Get(taskB.ID, "ak-B"); got == nil {
		t.Fatal("B 的任务应仍存在（A 删除被拒）")
	}

	// 空 owner（管理员/未认证）可删除
	if err := mgr.DeleteTask(taskB.ID, ""); err != nil {
		t.Fatalf("空 owner 删除任务应成功，got %v", err)
	}
	if got := mgr.Get(taskB.ID, ""); got != nil {
		t.Fatal("删除后任务应不存在")
	}
}

// TestOwner_UnchangedByStateTransitions 验证 owner 在状态流转（终态）后保持不变。
func TestOwner_UnchangedByStateTransitions(t *testing.T) {
	mgr := ownerTestMgr(t)
	taskA, _, err := mgr.SubmitAndStart(CreateRequest{
		Direction: "push",
		Remote:    "r1",
		Src:       "a.txt",
		Owner:     "ak-A",
	})
	if err != nil {
		t.Fatalf("SubmitAndStart 失败: %v", err)
	}

	// 完成路径（mock 执行器立即返回 completed）后 owner 不变
	task := waitForStatus(t, mgr, taskA.ID, StatusCompleted, 10*time.Second)
	if task.Owner != "ak-A" {
		t.Fatalf("completed 后 Owner = %q, want %q", task.Owner, "ak-A")
	}
}

// TestOwner_PersistedAcrossRestart 验证 owner 随任务持久化，重启恢复后保留。
func TestOwner_PersistedAcrossRestart(t *testing.T) {
	base := t.TempDir()
	tenantRoot, listTenants := newTestTenantRoot(base)
	quota := newMockQuota(0)
	remotes := []RemoteConfig{testRemote("r1", "http://127.0.0.1:1")}
	cfg := &Config{MaxConcurrent: 3, TaskTTL: 24 * time.Hour}
	mgr1 := NewManager(tenantRoot, listTenants, quota, 0, remotes, nil, discardLogger(), cfg)
	ta := mustCreate(t, mgr1, "a.txt", "ak-A")
	tb := mustCreate(t, mgr1, "b.txt", "ak-B")
	mgr1.Stop()

	// 重建管理器（同一基目录）恢复任务
	mgr2 := NewManager(tenantRoot, listTenants, quota, 0, remotes, nil, discardLogger(), cfg)
	defer mgr2.Stop()
	if got := mgr2.Get(ta.ID, ""); got == nil || got.Owner != "ak-A" {
		t.Fatalf("重启后任务 A Owner = %+v, want ak-A", got)
	}
	if got := mgr2.Get(tb.ID, ""); got == nil || got.Owner != "ak-B" {
		t.Fatalf("重启后任务 B Owner = %+v, want ak-B", got)
	}
	// 跨 owner 过滤在恢复任务上仍生效
	if got := mgr2.Get(ta.ID, "ak-B"); got != nil {
		t.Fatal("重启后 B 不应能 Get A 的任务")
	}
}
