// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

// Windows 专属用例：Job Object 的两条保证（都是 2026-09-16 修复的真实缺口）。
//
// 背景（审阅者在 Windows 上实测）：被监视命令经 `sh -c "sleep N & wait"` 派生的后代，在终止后
// **仍存活**（父 `sh` 已死 ⇒ 被重新挂载而逃逸），且日志里没有任何降级提示 ⇒ 说明 `taskkill /T /F`
// 返回成功但没真正收割树（已知行为：先杀掉中间进程后，孤儿脱离其树）。
// 开发机是 Windows，卡死会残留 `*.test`/`go` 进程占文件与端口 ⇒ 必须改用 Job Object：
// 创建带 `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE` 的 Job、启动后立即 `AssignProcessToJobObject`
// （后代默认继承该 Job），终止时 `TerminateJobObject`；`taskkill /T /F` 仅作兜底。

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestRun_JobReapsOrphansOnExit 钉住 **KILL_ON_JOB_CLOSE**：被监视命令**正常退出**后，它留下的
// 后代（孤儿）必须随 Job 关闭被收割——否则「卡死 → 杀掉 go test → 残留 *.test」的形态依旧。
//
// 该用例是**确定性**的（不依赖 taskkill 的树枚举竞态）：helper 派生一个永久阻塞的后代后立即退出 0。
// 变异：不创建/不分配 Job ⇒ 后代永久存活 ⇒ 本用例红。
func TestRun_JobReapsOrphansOnExit(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "5s", "-poll", "50ms", "-log", logPath, "--"},
		helperArgs("orphan")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != 0 {
		t.Fatalf("被监视命令正常退出 ⇒ 退出码应为 0，实际 %d\nstdout:\n%s\nstderr:\n%s", rc, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "派生 orphan 子进程失败") {
		t.Fatalf("前置条件不成立：helper 未能派生出后代\nstdout:\n%s", out.String())
	}
	orphan := grandchildPID(t, out.String())
	t.Cleanup(func() { killByPID(orphan) }) // 若修复失效（红跑），别把孤儿留在开发机上
	testutil.WaitFor(t, 10*time.Second, func() bool { return !processAlive(orphan) },
		"被监视命令退出后，其遗留后代必须被 Job 收割（KILL_ON_JOB_CLOSE）；"+
			"否则卡死后会残留 *.test/go 进程占文件与端口")
}

// TestRun_StallKillsShBackgroundChild 复现审阅者实测的形态：`sh -c "sleep N & wait"` 的后代在
// 停滞终止后必须消失。
//
// 注意 **pid 命名空间**：MSYS/Git-Bash 的 `ps` 给的是 MSYS pid（≠ Windows pid），故本用例的探活与
// 清理都经 `sh` 完成（不与 `processAlive` 的 Windows pid 混用）。
// 环境无 `sh`（例如 Windows runner 的 PATH 里没有 Git-Bash）时 Skip 并说明理由：Job 语义已由
// TestRun_JobReapsOrphansOnExit 与 TestRun_StallKillsDescendants 覆盖。
func TestRun_StallKillsShBackgroundChild(t *testing.T) {
	t.Parallel()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("环境无 sh（MSYS/Git-Bash）：跳过该形态复现；Job 语义由 TestRun_JobReapsOrphansOnExit 覆盖")
	}
	const marker = "24680" // 唯一标记（休眠秒数），避免与其它 sleep 混淆
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "2s", "-poll", "200ms", "-grace", "300ms", "-log", logPath, "--"},
		sh, "-c", "sleep "+marker+" & wait")

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	t.Cleanup(func() { killShSleep(sh, marker) }) // 红跑时清理，别留孤儿
	if rc != stallExitCode {
		t.Fatalf("停滞时退出码应为 %d，实际 %d\nstdout:\n%s\nstderr:\n%s", stallExitCode, rc, out.String(), errBuf.String())
	}
	if got := shSleepCount(t, sh, marker); got != 0 {
		t.Errorf("终止后仍有 %d 个 `sleep %s` 后代存活 ⇒ Windows 未真正收割整棵树\nstdout:\n%s", got, marker, out.String())
	}
}

// shSleepCount 经 `sh` 统计「命令行末两字段恰为 sleep <marker>」的进程数（MSYS pid 空间）。
// 用字段比较而非行尾锚定：`ps -ef` 会对末列填充空格，`$` 锚定会假阴性（实测踩过）。
func shSleepCount(t *testing.T, sh, marker string) int {
	t.Helper()
	out, err := exec.Command(sh, "-c", "ps -ef").Output()
	if err != nil {
		t.Fatalf("运行 `sh -c ps -ef`: %v", err)
	}
	count := 0
	for ln := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(ln)
		if len(fields) >= 2 && fields[len(fields)-1] == marker && fields[len(fields)-2] == "sleep" {
			count++
		}
	}
	return count
}

// killShSleep 经 `sh` 强杀匹配的标记进程（best-effort 清理）。
func killShSleep(sh, marker string) {
	out, err := exec.Command(sh, "-c", "ps -ef").Output()
	if err != nil {
		return
	}
	for ln := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(ln)
		if len(fields) >= 2 && fields[len(fields)-1] == marker && fields[len(fields)-2] == "sleep" {
			_ = exec.Command(sh, "-c", "kill -9 "+fields[1]).Run()
		}
	}
}
