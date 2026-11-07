// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package recovery

// crash_e2e_test.go 是 **M3 真实进程崩溃 e2e**（不是同进程内「假装重启」）。
//
// 做法：测试二进制自我 re-exec，子进程写 WAL 作用域后**不 Close、不 Flush、直接 os.Exit(17)**
// （模拟被 kill），父进程再用**全新** Service + Coordinator 打开同一目录：
//   - ① 崩溃在 MarkDone 之前 ⇒ 必须重放全部状态（任务不丢）；
//   - ② 崩溃在 MarkDone 之后 ⇒ 必须**跳过**重放（已完成的副作用绝不重复执行）。
//
// 这验证的是 WAL 的核心承诺：**证据级落盘 + 无需优雅关闭即可续跑**（SyncAlways 已 fsync）。
//
// 🔎 **2026-10-20 查清并修复的真因（重要）**：曾观察到「提交记录没生效、作用域永远未完成」，
// 根因在 `pkg/audit`：`Service.seq` 是**纯内存** map ⇒ **进程重启后序号归零** ⇒ 重启后写入的
// `KindDone`（提交记录）只拿到 `Seq=1`，而历史 `KindState` 已是 `Seq=1/2` ⇒ 恢复路径以
// 「该作用域**最大 Seq** 的条目为末条」判定 ⇒ **提交记录被当成更早的条目而忽略** ⇒ 作用域永远
// 「未完成」⇒ **每轮启动都重复执行同一任务**（副作用重复，正是设计 §6.1 禁止的失败模式）。
// 修复：`Service.initSeq` —— 每个作用域在本进程首次写入前从 Journal 取已知最大 Seq 作为起点
// （只升不降；回归用例 `pkg/audit/seq_restart_test.go`，变异验证命中）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/audit"
)

const (
	crashChildEnv = "SPROXY_AUDIT_CRASH_CHILD"
	crashDirEnv   = "SPROXY_AUDIT_CRASH_DIR"
	crashModeEnv  = "SPROXY_AUDIT_CRASH_MODE"
	crashExitCode = 17
)

// crashModes 是子进程的崩溃点。
const (
	crashBeforeDone = "before-done" // 写完状态就崩（未提交）
	crashAfterDone  = "after-done"  // 写完提交记录后崩（已提交）
)

// crashAudit 打开（或新建）目录上的审计服务（父/子进程共用）。
func crashAudit(t *testing.T, dir string) *audit.Service {
	t.Helper()
	j, err := audit.NewFileJournal(audit.FileJournalConfig{
		Dir: dir, Layout: audit.LayoutPerScope, SyncMode: audit.SyncAlways,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	caps := audit.FullCapabilities()
	svc, err := audit.New(audit.Options{Journal: j, Caps: &caps})
	if err != nil {
		t.Fatalf("audit.New: %v", err)
	}
	return svc
}

// runCrashChild 以子进程方式模拟崩溃，返回子进程输出（断言退出码）。
func runCrashChild(t *testing.T, mode, dir string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCrashRecovery_RealKill$", "-test.v")
	cmd.Env = append(os.Environ(),
		crashChildEnv+"=1", crashDirEnv+"="+dir, crashModeEnv+"="+mode)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != crashExitCode {
		t.Fatalf("子进程应被「强杀」退出 %d，实际 err=%v\n输出:\n%s", crashExitCode, err, out)
	}
}

// TestCrashRecovery_RealKill 真实崩溃的两个方向（未提交 ⇒ 重放；已提交 ⇒ 跳过）。
func TestCrashRecovery_RealKill(t *testing.T) {
	// 子进程分支：写作用域后立即退出（不 Close/不 Flush）。
	if os.Getenv(crashChildEnv) == "1" {
		ctx := context.Background()
		dir := os.Getenv(crashDirEnv)
		svc := crashAudit(t, dir)
		sc, err := svc.Open(ctx, "task.cloud.crash1")
		if err != nil {
			t.Fatalf("child Open: %v", err)
		}
		if serr := sc.Save(ctx, "step", "download"); serr != nil {
			t.Fatalf("child Save: %v", serr)
		}
		if serr := sc.Save(ctx, "progress", 42); serr != nil {
			t.Fatalf("child Save: %v", serr)
		}
		if os.Getenv(crashModeEnv) == crashAfterDone {
			// 业务副作用已完成（这里以「写完提交记录」代表）⇒ 再崩。
			if merr := sc.MarkDone(ctx); merr != nil {
				t.Fatalf("child MarkDone: %v", merr)
			}
		}
		// **强杀**：不 Close、不 Flush，模拟进程被 kill -9。
		os.Exit(crashExitCode)
	}

	t.Run("crash-before-commit-replays", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "wal")
		runCrashChild(t, crashBeforeDone, dir)

		// 父进程：全新 Service（等价「重启后」）⇒ 协调器必须续跑。
		svc := crashAudit(t, dir)
		rec := &recorder{}
		c := New(svc, quietLogger())
		if err := c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
			Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true }}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		rep, err := c.Run(context.Background(), Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Total != 1 || rep.Resumed != 1 || rep.Failed != 0 || rep.Unknown != 0 {
			t.Fatalf("崩溃前未提交的作用域必须被恢复: %s", rep)
		}
		if len(rec.seqs) != 2 {
			t.Fatalf("应重放 2 条状态（未优雅关闭也必须可读）, got %v", rec.seqs)
		}
		// 提交后不再残留未完成作用域；再跑一次是幂等的。
		left, _ := svc.ListScopes(context.Background(), audit.ScopeStateFilter{UnfinishedOnly: true})
		if len(left) != 0 {
			t.Fatalf("恢复后不应残留未完成作用域: %+v", left)
		}
		rep2, _ := c.Run(context.Background(), Options{})
		if rep2.Total != 0 || len(rec.seqs) != 2 {
			t.Fatalf("第二次运行必须幂等: %s seqs=%v", rep2, rec.seqs)
		}
	})

	t.Run("crash-after-commit-skips", func(t *testing.T) {
		t.Parallel()
		dir := filepath.Join(t.TempDir(), "wal")
		runCrashChild(t, crashAfterDone, dir)

		svc := crashAudit(t, dir)
		rec := &recorder{}
		c := New(svc, quietLogger())
		if err := c.Register(Registration{Kind: "cloud", Prefix: "task.cloud",
			Factory: func(audit.ScopeID) (audit.ReplayApplier, bool) { return rec, true }}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		rep, err := c.Run(context.Background(), Options{})
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if rep.Total != 0 || rep.Resumed != 0 {
			t.Fatalf("已提交作用域不得被恢复处理: %s", rep)
		}
		if len(rec.seqs) != 0 {
			t.Fatalf("已提交作用域必须跳过重放（副作用不得重复执行）, got %v", rec.seqs)
		}
		// 直接 Resume 也应判为已完成（提交记录在崩溃前已落盘）。
		st, rerr := svc.Resume(context.Background(), "task.cloud.crash1", rec)
		if rerr != nil {
			t.Fatalf("Resume: %v", rerr)
		}
		if !st.Done || len(rec.seqs) != 0 {
			t.Fatalf("提交记录必须持久化: done=%v seqs=%v", st.Done, rec.seqs)
		}
		if st.States != 0 {
			t.Fatalf("已提交作用域不应重放状态: states=%d", st.States)
		}
	})
}
