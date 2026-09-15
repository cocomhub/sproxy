// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// resume_tenant_test.go 钉住 ResumeTask「租户不可用」早退路径的收口语义。
//
// 该路径在状态翻转之后（status→pending、running 置位、按需新落 1 GiB 未知大小占位）
// 才发现任务目录不可建，于是不启动下载 goroutine 就返回。若不回滚，留下的 running
// 标记在 CancelTask/DeleteTask「goroutine 仍存活 ⇒ 把 Scope 释放推迟到 goroutine 退出」
// 的语义下会让该任务的配额释放**永不发生**（释放点只剩一个不存在的 goroutine）。
//
// 租户不可用由注入的 TenantResolver 制造（领域既有 seam：NewCloudDownloadManager 的
// tenantFor 参数；用例把它包装为「可按需返回 nil」）⇒ 完全确定性，无 sleep、无时序依赖、
// 无真实目录故障。
package cloud

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// resumeTenantEnv 是「租户可切换为不可用」的用例基座：真实租户缓存/配额 Scope +
// 真实全局容量账本（capacity.StorageManager）+ 可按需失效的租户解析器。
type resumeTenantEnv struct {
	mgr *CloudDownloadManager
	env *cloudTestEnv
	sm  *capacity.StorageManager

	// tenantOff=true 时租户解析器返回 nil（等价 owner 失效 / 存储根卸载）。
	// 用 atomic 是因为后台清理/持久化 goroutine 也会读解析器，而用例侧写这个开关。
	tenantOff atomic.Bool
}

// newResumeTenantEnv 在临时存储根下装配管理器（全局上限 10 GiB，租户配额不限）。
func newResumeTenantEnv(t *testing.T) *resumeTenantEnv {
	t.Helper()
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 10*1024*1024*1024, nil, testLogger())
	env := newCloudTestEnv(t, dir)
	re := &resumeTenantEnv{env: env, sm: sm}
	re.mgr = newCloudTestManagerInEnv(t, env, sm, func(owner string) *storage.Tenant {
		if re.tenantOff.Load() {
			return nil
		}
		return env.tenantFor(owner)
	}, &CloudDownloadConfig{
		SyncThreshold: 1,
		MaxConcurrent: 1,
		TaskTTL:       time.Hour,
		FailedTaskTTL: time.Hour,
	})
	return re
}

// cloudUsage 返回全局账本中云下载分类的占用（/api/stats 的 CategoryCloud）。
func (re *resumeTenantEnv) cloudUsage() int64 {
	return re.sm.UsageByCategory()[capacity.CategoryCloud]
}

// scopeUsage 返回 owner 云桶配额 Scope 的占用（租户配额账本）。
func (re *resumeTenantEnv) scopeUsage(owner string) int64 {
	return re.env.quotaBucketFor(owner, "cloud").Usage()
}

// isRunning 报告任务是否残留执行中标记（清理类断言用；同包用例读内部状态是既有做法）。
func (re *resumeTenantEnv) isRunning(taskID string) bool {
	re.mgr.mu.RLock()
	defer re.mgr.mu.RUnlock()
	return re.mgr.running[taskID]
}

func TestCloudDownloadManager_ResumeTaskTenantUnavailableRollsBack(t *testing.T) {
	t.Parallel()

	// 场景 1：resume 前任务无任何占用（取消后 ReservedSize=0）⇒ 本次 resume 会新落
	// 1 GiB 占位。租户不可用时它必须连同 running 标记一起回滚，且任务回到 resume 前的终态。
	t.Run("新落占位与 running 标记回滚", func(t *testing.T) {
		re := newResumeTenantEnv(t)
		mgr, owner := re.mgr, "alice"

		task, err := mgr.CreateTask("url", "https://example.com/rollback.bin", "rollback.bin", 200, owner)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		if err = mgr.CancelTask(task.ID, owner); err != nil {
			t.Fatalf("CancelTask: %v", err)
		}
		if got := re.cloudUsage(); got != 0 {
			t.Fatalf("前置条件不成立：取消后 cloud 账本=%d want 0", got)
		}

		// resume 前的四字段快照：status/error/updatedAt/expiresAt 必须**整体**回滚。
		before, ok := mgr.SnapshotTask(task.ID, owner)
		if !ok {
			t.Fatal("task disappeared")
		}

		re.tenantOff.Store(true) // 任务存活期间租户变不可用
		err = mgr.ResumeTask(task.ID, false, owner)
		if err == nil || !strings.Contains(err.Error(), "tenant unavailable") {
			t.Fatalf("ResumeTask err=%v want tenant unavailable", err)
		}
		if re.isRunning(task.ID) {
			t.Fatalf("ResumeTask 早退后 running[%s]=true 残留：后续 cancel/delete 的配额释放会被推迟给不存在的 goroutine",
				task.ID)
		}
		if got := re.cloudUsage(); got != 0 {
			t.Fatalf("ResumeTask 早退后 cloud 账本=%d want 0（本次新落的 1 GiB 占位未回滚）", got)
		}
		if got := re.scopeUsage(owner); got != 0 {
			t.Fatalf("ResumeTask 早退后租户 Scope=%d want 0", got)
		}
		// 任务必须回到 resume 前的终态：停在 pending 会让 cleanupExpired 永不清理它
		// （只处理 completed/failed/cancelled），且 findByURL 会把同 URL 请求吸收到这条
		// 没有 goroutine 的任务上。
		snap, ok := mgr.SnapshotTask(task.ID, owner)
		if !ok {
			t.Fatal("task disappeared")
		}
		if snap.Status != "cancelled" {
			t.Fatalf("ResumeTask 早退后 status=%q want cancelled（回滚到 resume 前）", snap.Status)
		}
		// 只回 status 而留着 resume 写入的 UpdatedAt/ExpiresAt（TaskTTL=1h）会让已取消/已失败
		// 任务在观察者与过期清理看来「刚被更新过」⇒ 整体回滚才是干净的。
		if !snap.UpdatedAt.Equal(before.UpdatedAt) || !snap.ExpiresAt.Equal(before.ExpiresAt) {
			t.Fatalf("ResumeTask 早退后 UpdatedAt/ExpiresAt=(%v/%v) want (%v/%v)（四字段需整体回滚）",
				snap.UpdatedAt, snap.ExpiresAt, before.UpdatedAt, before.ExpiresAt)
		}

		// 再次 resume 失败不得累积占位（回滚幂等）。
		if err := mgr.ResumeTask(task.ID, false, owner); err == nil {
			t.Fatal("租户仍不可用，ResumeTask 应继续失败")
		}
		if got := re.cloudUsage(); got != 0 {
			t.Fatalf("重复 resume 失败后 cloud 账本=%d want 0（占位累积）", got)
		}

		// 再 CancelTask：任务已回到终态，取消被拒绝且账本保持归零——不会把释放推给不存在的
		// goroutine（修复前 running 残留为 true，任何进入取消路径的调用都会推迟释放）。
		if err := mgr.CancelTask(task.ID, owner); err == nil {
			t.Fatal("ResumeTask 早退后任务应已回到终态 cancelled，CancelTask 应被拒绝")
		}
		if got := re.cloudUsage(); got != 0 {
			t.Fatalf("CancelTask 后 cloud 账本=%d want 0", got)
		}
		if got := re.scopeUsage(owner); got != 0 {
			t.Fatalf("CancelTask 后租户 Scope=%d want 0", got)
		}
	})

	// 场景 2：failed 任务保留了 90 字节 .partial，其既有占位（全局账本 + 租户 Scope）代表
	// 磁盘上的真实占用。早退不得回滚它（否则账本与磁盘实际脱钩），但 running 必须清干净——
	// 否则后续 DeleteTask 的 Scope 释放被推迟给不存在的 goroutine，永久占用。
	t.Run("失败任务既有占位不回滚且删除即释放", func(t *testing.T) {
		re := newResumeTenantEnv(t)
		mgr, owner := re.mgr, "bob"

		task, err := mgr.CreateTask("url", "https://example.com/partial.bin", "partial.bin", 90, owner)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		taskDir := filepath.Join(mgr.CloudDirFor(owner), task.ID)
		if err = os.MkdirAll(taskDir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err = os.WriteFile(filepath.Join(taskDir, "partial.bin.partial"), make([]byte, 90), 0o644); err != nil {
			t.Fatalf("写 .partial: %v", err)
		}
		mgr.failTask(task, "simulated failure") // 走正常失败路径：.partial 保留并计入账本
		if got := re.cloudUsage(); got != 90 {
			t.Fatalf("前置条件不成立：failTask 后 cloud 账本=%d want 90", got)
		}
		if got := re.scopeUsage(owner); got != 90 {
			t.Fatalf("前置条件不成立：failTask 后租户 Scope=%d want 90", got)
		}

		// resume 前的四字段快照（同场景 1：失败任务的终态也不得被改写）。
		before, ok := mgr.SnapshotTask(task.ID, owner)
		if !ok {
			t.Fatal("task disappeared")
		}

		re.tenantOff.Store(true)
		if err = mgr.ResumeTask(task.ID, false, owner); err == nil {
			t.Fatal("租户不可用时 ResumeTask 应失败")
		}
		if re.isRunning(task.ID) {
			t.Fatalf("ResumeTask 早退后 running[%s]=true 残留", task.ID)
		}
		// .partial 仍在盘上 ⇒ 既有占位不得被早退回滚。
		if got := re.cloudUsage(); got != 90 {
			t.Fatalf("早退不得释放 .partial 的既有占位：cloud 账本=%d want 90", got)
		}
		if got := re.scopeUsage(owner); got != 90 {
			t.Fatalf("早退不得释放 .partial 的既有 Scope 占用：%d want 90", got)
		}
		snap, ok := mgr.SnapshotTask(task.ID, owner)
		if !ok {
			t.Fatal("task disappeared")
		}
		if snap.Status != "failed" {
			t.Fatalf("ResumeTask 早退后 status=%q want failed（回滚到 resume 前）", snap.Status)
		}
		if !snap.UpdatedAt.Equal(before.UpdatedAt) || !snap.ExpiresAt.Equal(before.ExpiresAt) {
			t.Fatalf("ResumeTask 早退后 UpdatedAt/ExpiresAt=(%v/%v) want (%v/%v)（四字段需整体回滚）",
				snap.UpdatedAt, snap.ExpiresAt, before.UpdatedAt, before.ExpiresAt)
		}

		// running 已清 ⇒ 删除时 Scope 释放立即发生（修复前 running 残留 ⇒ 释放被推迟给
		// 不存在的 goroutine ⇒ 永久占用）。
		if err = mgr.DeleteTask(task.ID, owner); err != nil {
			t.Fatalf("DeleteTask: %v", err)
		}
		if got := re.cloudUsage(); got != 0 {
			t.Fatalf("DeleteTask 后 cloud 账本=%d want 0", got)
		}
		if got := re.scopeUsage(owner); got != 0 {
			t.Fatalf("DeleteTask 后租户 Scope=%d want 0（running 残留导致释放悬空）", got)
		}
	})
}
