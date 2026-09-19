// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestCloudTask_ConcurrentResumeCancel_NoLeak 钉住「cancel 后 resume 改回 pending 且
// goroutine 退出」的账本泄漏窗口（CI run 35419872637 Vault job 现场：
// `并发后 Scope Usage()=90 > 磁盘占用=0（虚高/泄漏）`）。
//
// 用 resumeWindowHook seam 在 ResumeTask 的 unlock→relock 窗口注入 CancelTask，
// 确定性构造「resume 置 pending/running 后立即被取消」：
//   - ResumeTask 置 Status=pending + running=true，解锁；
//   - 窗口内 CancelTask 置 cancelled + 删 .partial + removeTaskDir（Scope 释放因
//     running 为真推迟到 goroutine 退出路径）；
//   - ResumeTask 重新取锁后落在「租户/任务不可用」回滚（rollbackResumeLocked）→
//     清 running 但不启动 goroutine ⇒ releaseAbandonedTaskScope 的释放点悬空。
//
// 本测试钉住「新判据必须配新顺序」的耦合（防御性硬化，非行为修复——parent 上本
// 测试为绿，决定性验证见 progress.md T3 记录）：cleanupRunning 若恢复旧顺序（先释放
// 后清 running），判据见 running 为真跳过 ⇒ Scope 残留 90。
// 修复后：判据改为「m.running[taskID] 为真才不释放」——running 已清 ⇒ 必然释放。
func TestCloudTask_ConcurrentResumeCancel_NoLeak(t *testing.T) {
	t.Parallel()
	re := newResumeTenantEnv(t)
	mgr, owner := re.mgr, "alice"

	task, err := mgr.CreateTask("url", "https://example.com/leak.bin", "leak.bin", 100, owner)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// 首次下载失败保留 .partial（account committed=90）→ failed。
	taskDir := filepath.Join(mgr.CloudDirFor(owner), task.ID)
	if err = os.MkdirAll(taskDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err = os.WriteFile(filepath.Join(taskDir, "leak.bin.partial"), make([]byte, 90), 0o644); err != nil {
		t.Fatalf("写 .partial: %v", err)
	}
	mgr.failTask(task, "simulated failure")
	if got := re.scopeUsage(owner); got != 90 {
		t.Fatalf("前置不成立：failTask 后 Scope=%d want 90", got)
	}

	// resumeWindowHook：在 ResumeTask 的 unlock→relock 窗口里 CancelTask（置 cancelled +
	// 删文件 + 释放存储；Scope 因 running 推迟到 goroutine 退出）。
	origHook := resumeWindowHook
	resumeWindowHook = func(m *CloudDownloadManager, taskID string) {
		if taskID == task.ID {
			_ = m.CancelTask(taskID, owner)
		}
	}
	t.Cleanup(func() { resumeWindowHook = origHook })

	// 窗口内 cancel 后 ResumeTask 可成功（goroutine 启动后立刻发现 cancelled 早退）或
	// 失败（回滚）——泄漏窗口的核心判定是「终态 Scope 归零」，两种路径都不得残留。
	_ = mgr.ResumeTask(task.ID, false, owner)
	// 等 goroutine 完全退出（running 清除）再断言：ResumeTask 异步启动 goroutine，
	// cancelled 早退 + cleanupRunning 释放需要时间；未等即断言会误判泄漏（假红）。
	testutil.WaitFor(t, 30*time.Second, func() bool {
		return !re.isRunning(task.ID)
	}, "resume 启动的 goroutine 未退出")
	// 终态断言：Scope 必须归零（releaseAbandonedTaskScope 判据漏 pending 时残留 90）。
	if got := re.scopeUsage(owner); got != 0 {
		t.Fatalf("取消后 Scope=%d want 0（泄漏窗口：resume 改回 pending 且 goroutine 已退）", got)
	}
	if got := re.cloudUsage(); got != 0 {
		t.Fatalf("取消后全局账本=%d want 0", got)
	}
}
