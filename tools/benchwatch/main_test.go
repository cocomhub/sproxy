// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// helperModeFlag 是**测试专用**标志：被监视的 helper 子进程靠它选择行为。
//
// 为什么不走环境变量：`run()` 不暴露子进程环境（生产无需该能力），而 `t.Setenv` 会让用例
// 无法并行（本仓 R18 门禁要求顶层用例默认并行）。用测试二进制自己的标志则既显式又并行安全
// （testing 允许测试包注册自定义标志，只要在 flag.Parse 之前注册——本文件 init 即满足）。
var helperModeFlag = flag.String("benchwatch-helper", "", "测试专用：helper 子进程行为（stall/live/late/exit7/stallchild）")

func init() {
	// 触发标志注册（赋值发生在包变量初始化阶段，早于 testing 的 flag.Parse）。
	_ = helperModeFlag
}

// TestHelperProcess 不是断言型用例：它作为**被监视子进程**由 run() 启动（标准 helper-process 模式）。
// 模式由 `-benchwatch-helper` 选择：
//
//	stall      —— 打印一行后**永久阻塞**（模拟「单个 op 卡死不再返回」；刻意不含任何 sleep）
//	live       —— 按固定间隔持续打印，达到时长后退出 0（模拟「正常但慢」的 benchmark）
//	late       —— 先**静默**一段时间再持续打印（模拟「建包/冷缓存期间无输出」），随后正常退出
//	exit7      —— 立即以退出码 7 结束（验证退出码传播）
//	stallchild —— 先派生一个 stall 子进程并打印其 pid，然后自己也永久阻塞（验证「整棵树被终止」）
//
// 除 late 外，各模式都在 `t.Parallel()` **之前**打印 banner：让「首个字节」尽早出现，从而进入
// 稳态窗口（否则会被 startup 窗口掩盖，用例失去判别力）。
//
// t.Parallel()：本用例在 helper 二进制里是唯一被选中运行的用例（-test.run 精确匹配），
// 并行组会立即释放，因此「永久阻塞」不会卡住任何东西；而在常规 `go test` 中它直接 Skip。
func TestHelperProcess(t *testing.T) {
	mode := *helperModeFlag
	if mode == "" {
		t.Parallel()
		t.Skip("仅作为 benchwatch 的被监视子进程运行（-benchwatch-helper=<mode>）")
	}
	if mode != "late" {
		fmt.Printf("helper: pid=%d mode=%s\n", os.Getpid(), mode)
	}
	t.Parallel()
	switch mode {
	case "stall":
		fmt.Println("helper: 开始停滞（永久阻塞，无 sleep）")
		select {}
	case "live":
		emitTicks("live", 50*time.Millisecond, 3*time.Second)
		fmt.Println("helper: live 结束")
		os.Exit(0)
	case "late":
		// 静默阶段：不产出任何输出（用 ticker 计时，不用 sleep）。
		silent := time.NewTicker(2 * time.Second)
		<-silent.C
		silent.Stop()
		emitTicks("late", 50*time.Millisecond, 2*time.Second)
		fmt.Println("helper: late 结束")
		os.Exit(0)
	case "exit7":
		fmt.Println("helper: 即将以 7 退出")
		os.Exit(7)
	case "envcheck":
		// 验证「命令开头的 KEY=VALUE 前缀被注入子进程环境」（Makefile 的 $(GO) 就靠它）。
		if os.Getenv("BENCHWATCH_ENV_PROBE") != "ok" {
			fmt.Printf("helper: BENCHWATCH_ENV_PROBE=%q（期望 ok）\n", os.Getenv("BENCHWATCH_ENV_PROBE"))
			os.Exit(5)
		}
		fmt.Println("helper: envcheck ok")
		os.Exit(0)
	case "stallchild":
		// 后代的 stdout/stderr **不继承本进程**（置 nil ⇒ 接空设备）：否则它会一直持住看门狗的
		// stdout 管道，使 `cmd.Wait()` 永远等不到 EOF（真实场景里残留 `*.test` 确实会这样，
		// 看门狗会因此报一次「停滞」并收割整棵树——属有意的 fail-closed 行为；此处为隔离
		// 「后代是否被收割」这一单一变量而刻意断开）。
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "-test.timeout=30s",
			"-benchwatch-helper=stall")
		if err := child.Start(); err != nil {
			fmt.Printf("helper: 派生 stall 子进程失败: %v\n", err)
			os.Exit(9)
		}
		fmt.Printf("helper: grandchild-pid=%d\n", child.Process.Pid)
		select {}
	case "orphan":
		// 派生一个永久阻塞的后代后**立即退出 0**：验证「被监视命令退出后遗留的后代」会被收割
		// （Windows 靠 Job 的 KILL_ON_JOB_CLOSE；Unix 无此语义，故该用例只在 Windows 跑）。
		// 同上：后代不继承 stdout/stderr，避免它持住管道使 `cmd.Wait()` 永不返回。
		child := exec.Command(os.Args[0], "-test.run=TestHelperProcess", "-test.timeout=30s",
			"-benchwatch-helper=stall")
		if err := child.Start(); err != nil {
			fmt.Printf("helper: 派生 orphan 子进程失败: %v\n", err)
			os.Exit(9)
		}
		fmt.Printf("helper: grandchild-pid=%d\n", child.Process.Pid)
		os.Exit(0) // 不等待后代：故意留下孤儿
	}
	os.Exit(0)
}

// emitTicks 按 interval 持续打印直到 duration 用尽（用 ticker，不用 sleep）。
func emitTicks(label string, interval, duration time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		<-ticker.C
		fmt.Printf("helper: %s tick %d\n", label, time.Now().UnixNano())
	}
}

// helperArgs 返回「以本测试二进制作为被监视命令」的 argv，模式经 -benchwatch-helper 传入。
func helperArgs(mode string) []string {
	return []string{os.Args[0], "-test.run=TestHelperProcess", "-test.timeout=30s", "-benchwatch-helper=" + mode}
}

// runGuarded 在守护下调用 run()：看门狗若失效（例如变异后不再检测停滞），run() 会一直等待
// 卡死的子进程 ⇒ 用守护计时器把「测试挂住」变成「快速失败」，而不是让整个测试包超时。
// 注意：这里的 time.After 是**失败保护**而非同步手段（正常路径由 run() 自身返回驱动）。
func runGuarded(t *testing.T, args []string, stdout, stderr io.Writer, guard time.Duration) int {
	t.Helper()
	done := make(chan int, 1)
	go func() { done <- run(args, stdout, stderr) }()
	select {
	case rc := <-done:
		return rc
	case <-time.After(guard):
		t.Fatalf("run() 在 %s 内未返回：看门狗没有检测到停滞（或未终止卡死的子进程）\nargs=%v", guard, args)
		return -1
	}
}

// waitUntilGone 轮询等待 pid 消失（有界），用于断言「进程确实被终止」；
// 轮询用 testutil.WaitForBool（本仓禁裸 sleep 轮询）。
func waitUntilGone(pid int, timeout time.Duration) bool {
	return testutil.WaitForBool(timeout, func() bool { return !processAlive(pid) })
}

func TestRun_StalledCommandIsReportedAndKilled(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "300ms", "-poll", "50ms", "-grace", "20ms", "-tail", "5", "-log", logPath, "--"},
		helperArgs("stall")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != stallExitCode {
		t.Fatalf("停滞时退出码应为 %d（本工具定义），实际 %d\nstdout:\n%s\nstderr:\n%s", stallExitCode, rc, out.String(), errBuf.String())
	}
	got := out.String()
	for _, want := range []string{"卡死", "输出停滞", "300ms", "已开始输出后"} {
		if !strings.Contains(got, want) {
			t.Errorf("停滞诊断缺少 %q\nstdout:\n%s", want, got)
		}
	}
	// 日志尾部必须出现在诊断里（卡在哪一步的关键线索）。
	//
	// 注意断言必须**只看尾部段**：被监视进程的输出本来就会被实时转发到 stdout，若只查整个
	// stdout 里有没有那行，则「不打印尾部」的变异测不出来（实测过该变异仍绿）。
	tailIdx := strings.Index(got, "日志尾部")
	if tailIdx < 0 {
		t.Fatalf("停滞诊断缺少日志尾部件\nstdout:\n%s", got)
	}
	if !strings.Contains(got[tailIdx:], "helper:") {
		t.Errorf("日志尾部件必须包含被监视进程的最后输出\nstdout:\n%s", got)
	}
	// 被监视进程必须真的被终止（不只是打印了消息）。
	pid := helperPID(t, out.String())
	if !waitUntilGone(pid, 10*time.Second) {
		t.Errorf("被监视进程 pid=%d 在停滞判定后仍存活 ⇒ 没有真正终止", pid)
	}
}

// TestRun_SilentStartupIsNotKilled 钉住**双窗口**语义：首个字节之前用 -startup（编译/冷缓存期
// 无输出是正常的），已开始输出后才用 -limit。
//
// 判别力前提：静默期（2s）必须**长于 limit（1.5s）**且短于 startup（5s）——否则「去掉 startup
// 窗口」这个变异测不出来（实测过：静默期短于 limit 时该变异仍绿）。
func TestRun_SilentStartupIsNotKilled(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "1500ms", "-poll", "50ms", "-grace", "20ms", "-log", logPath, "--"},
		helperArgs("late")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != 0 {
		t.Fatalf("「先静默 2s（长于 limit、短于 startup）再持续输出」的命令不得被判卡死（期望退出码 0，实际 %d）\nstdout:\n%s\nstderr:\n%s",
			rc, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "卡死") {
		t.Errorf("静默启动被误判为卡死（假阳性）—— startup 窗口失效\nstdout:\n%s", out.String())
	}
}

func TestRun_LiveCommandIsNotKilled(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "2s", "-poll", "50ms", "-grace", "20ms", "-log", logPath, "--"},
		helperArgs("live")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != 0 {
		t.Fatalf("持续有输出的命令应正常结束（退出码 0），实际 %d\nstdout:\n%s\nstderr:\n%s", rc, out.String(), errBuf.String())
	}
	if strings.Contains(out.String(), "卡死") {
		t.Errorf("持续有输出的命令被误判为卡死（假阳性）\nstdout:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "helper: live 结束") {
		t.Errorf("被监视命令的输出必须被转发到 stdout\nstdout:\n%s", out.String())
	}
	// 日志文件必须被写入（CI 依赖它做 artifact 与失败诊断）。
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志文件: %v", err)
	}
	if !strings.Contains(string(data), "helper: live tick") {
		t.Errorf("日志文件未捕获被监视命令的输出\n日志:\n%s", string(data))
	}
}

func TestRun_PropagatesChildExitCode(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "5s", "-poll", "50ms", "-log", logPath, "--"}, helperArgs("exit7")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != 7 {
		t.Fatalf("必须传播被监视命令的退出码（期望 7，实际 %d）—— make bench 的失败判定依赖它\nstdout:\n%s", rc, out.String())
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("读取日志文件: %v", err)
	}
	if !strings.Contains(string(data), "即将以 7 退出") {
		t.Errorf("日志文件未捕获退出前的输出\n日志:\n%s", string(data))
	}
}

// TestRun_StallKillsDescendants 覆盖「整棵进程树终止」：`go test` 会再派生 `<pkg>.test`，
// 只杀直接子进程会留下孤儿（CI 形态③日志里的 `Terminate orphan process: pid (…) (server.test)`
// 就是这么来的）。
//
// 两个平台各自用自己的机制：Unix 用独立进程组（`kill -pgid`）；Windows 用 Job Object
// （`TerminateJobObject`）——后者是 2026-09-16 修复的：`taskkill /T /F` 先杀掉中间进程后，
// 孤儿会脱离其树而逃逸（开发机是 Windows，卡死会残留 `*.test`/`go` 进程占文件与端口）。
func TestRun_StallKillsDescendants(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "300ms", "-poll", "50ms", "-grace", "20ms", "-log", logPath, "--"},
		helperArgs("stallchild")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != stallExitCode {
		t.Fatalf("停滞时退出码应为 %d，实际 %d\nstdout:\n%s\nstderr:\n%s", stallExitCode, rc, out.String(), errBuf.String())
	}
	grandchild := grandchildPID(t, out.String())
	// 不断言「判定前它曾存活」：Windows 上 taskkill 的树枚举有竞态，**可能已经把它带走了**
	// （这正是本片要修的不可靠之处），事后无法区分「从未存活」与「已被杀」。
	// 判别力由 `grandchild-pid=` 的存在（上面 grandchildPID 会在缺失时 Fatalf）+ 本断言承担。
	t.Cleanup(func() { killByPID(grandchild) }) // 若修复失效（红跑），别把孤儿留在开发机上
	testutil.WaitFor(t, 10*time.Second, func() bool { return !processAlive(grandchild) },
		"停滞判定后必须终止被监视命令的**后代**（否则留下孤儿进程，CI 上只能靠 runner 清理）")
}

// grandchildPID 从被监视进程的输出里取出它自报的后代 pid。
func grandchildPID(t *testing.T, output string) int {
	t.Helper()
	for ln := range strings.SplitSeq(output, "\n") {
		_, after, ok := strings.Cut(ln, "grandchild-pid=")
		if !ok {
			continue
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return pid
	}
	t.Fatalf("未能从输出里解析后代 pid:\n%s", output)
	return 0
}

// TestRun_PassesEnvAssignmentsToChild 钉住「命令开头的 `KEY=VALUE` 前缀会注入子进程环境」。
//
// 为何必须有：本仓 Makefile 的 `$(GO)` 展开为 `GOOS=… GOARCH=… go`，而 `exec.Command` 不会像
// shell 那样解释它——若看门狗不拆前缀，`make bench` 会以「exec: "GOOS=windows": executable file
// not found」直接失败（2026-09-16 实测到的真实 wiring bug）。
func TestRun_PassesEnvAssignmentsToChild(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "output.txt")
	var out, errBuf bytes.Buffer
	args := append([]string{"-startup", "5s", "-limit", "5s", "-poll", "50ms", "-log", logPath, "--", "BENCHWATCH_ENV_PROBE=ok"},
		helperArgs("envcheck")...)

	rc := runGuarded(t, args, &out, &errBuf, 30*time.Second)
	if rc != 0 {
		t.Fatalf("`KEY=VALUE` 前缀必须注入子进程环境（期望退出码 0，实际 %d）\nstdout:\n%s\nstderr:\n%s",
			rc, out.String(), errBuf.String())
	}
}

func TestRun_RejectsMissingCommandOrLog(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"缺少被监视命令", []string{"-log", filepath.Join(tmp, "o.txt")}, "缺少被监视命令"},
		{"缺少 -log", []string{"--", os.Args[0]}, "-log 必须指定"},
		{"poll 大于 limit", []string{"-limit", "1s", "-poll", "2s", "-log", filepath.Join(tmp, "o.txt"), "--", os.Args[0]}, "不得大于"},
		{"startup 小于 limit", []string{"-startup", "1s", "-limit", "2s", "-log", filepath.Join(tmp, "o.txt"), "--", os.Args[0]}, "不得小于"},
	}
	for _, tc := range cases {
		var out, errBuf bytes.Buffer
		rc := run(tc.args, &out, &errBuf)
		if rc != usageExitCode {
			t.Errorf("%s: 期望退出码 %d，实际 %d", tc.name, usageExitCode, rc)
		}
		if !strings.Contains(errBuf.String(), tc.want) {
			t.Errorf("%s: stderr 应含 %q，实际:\n%s", tc.name, tc.want, errBuf.String())
		}
	}
}

// helperPID 从被监视进程的输出里取出它自报的 pid（helper 每个模式都会打印）。
func helperPID(t *testing.T, output string) int {
	t.Helper()
	for ln := range strings.SplitSeq(output, "\n") {
		_, after, ok := strings.Cut(ln, "helper: pid=")
		if !ok {
			continue
		}
		fields := strings.Fields(after)
		if len(fields) == 0 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		return pid
	}
	t.Fatalf("未能从输出里解析 helper pid:\n%s", output)
	return 0
}
