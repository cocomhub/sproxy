// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command benchwatch 是 `make bench` 的**进程外**停滞看门狗。
//
// 它运行一个子命令，把子命令的 stdout/stderr **同时**写进日志文件与自身输出（保持流式日志），
// 并在「日志零增长超过上限」时判定停滞（形态③：单个 op 卡死不再返回），随即打印醒目诊断
// （含日志尾部）、终止子命令**及其后代**，并以退出码 66 结束。
//
// 为什么必须是**进程外**：`go test -timeout` 对 benchmark **不生效**——
// 把 60s 睡眠放进 benchmark 并加 `-timeout 5s`，用例仍会 PASS；同一睡眠放进 Test 才会
// `panic: test timed out`（A/B/C 实验见 docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md §3.2）。
// 而卡死可能是 syscall 级不可中断 I/O，进程内的看门狗（goroutine + 定时器）不保证有机会运行，
// 只有独立进程能可靠观测并报告——这正是 CI 里形态③只剩 `Terminate orphan process`、没有任何栈的原因。
//
// 两个静默窗口：首个字节之前用 -startup（编译/冷缓存期无输出是正常的），
// 已有输出之后用 -limit（benchmark 结果行每隔几秒刷出，长时间零增长只能是卡死）。
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	// stallExitCode 是本工具定义的退出码：停滞看门狗触发。
	// 与 go test 的退出码（1=FAIL / 2=构建失败或 panic）区分开，便于 CI 判据与日志检索。
	stallExitCode = 66
	// usageExitCode 是参数/环境错误（未给出被监视命令、日志文件打不开等）。
	usageExitCode = 2

	defaultStartup = 180 * time.Second
	defaultLimit   = 120 * time.Second
	defaultPoll    = time.Second
	defaultGrace   = 5 * time.Second
	defaultTail    = 40
	// tailReadLimit 限制「读日志尾部」时最多回读的字节数（bench 日志可达数十 MB）。
	tailReadLimit = 1 << 20

	stallBanner = "=============================================================================="
)

type options struct {
	startup   time.Duration
	limit     time.Duration
	poll      time.Duration
	grace     time.Duration
	tailLines int
	logPath   string
	command   []string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 解析参数、运行被监视命令并返回退出码（main 与测试共用同一入口）。
func run(args []string, stdout, stderr io.Writer) int {
	// 用互斥包装（见 lockedWriter）：exec.Cmd 的内部拷贝 goroutine 与停滞诊断 reportStall
	// 会并发写调用方的 writer ⇒ 测试传的 bytes.Buffer 非线程安全，`-race` 在 CI 抓到。
	stdout, stderr = lockWriters(stdout, stderr)
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		fmt.Fprintf(stderr, "benchwatch: %v\n", err)
		return usageExitCode
	}
	logFile, err := os.Create(opts.logPath)
	if err != nil {
		fmt.Fprintf(stderr, "benchwatch: 无法创建日志文件 %s: %v\n", opts.logPath, err)
		return usageExitCode
	}
	defer func() { _ = logFile.Close() }()

	cmdArgs := splitEnvAssignments(opts.command)
	envPrefix := opts.command[:len(opts.command)-len(cmdArgs)]
	cmd := exec.Command(cmdArgs[0], cmdArgs[1:]...)
	if len(envPrefix) > 0 {
		// 把 `KEY=VALUE` 前缀注入子进程环境（Makefile 的 `$(GO)` 就是这么用的）。
		cmd.Env = append(os.Environ(), envPrefix...)
	}
	cmd.Stdout = io.MultiWriter(logFile, stdout)
	cmd.Stderr = io.MultiWriter(logFile, stderr)

	// 平台进程树控制器：Unix 用独立进程组（Setpgid + kill(-pgid)）；Windows 用 Job Object
	// （KILL_ON_JOB_CLOSE + TerminateJobObject，taskkill /T /F 仅作兜底）。
	// beforeStart 必须在 Start 之前（Setpgid 需在启动前设置）；afterStart 在 Start 后立即接管。
	treeCtrl := newTreeController()
	treeCtrl.beforeStart(cmd)
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "benchwatch: 启动被监视命令失败: %v\n", err)
		return usageExitCode
	}
	if note, err := treeCtrl.afterStart(cmd); err != nil {
		// Job 接管失败：Windows 上意味着「后代无法被一次性收割」⇒ 显式记录，不再静默降级。
		fmt.Fprintf(stderr, "benchwatch: %s\n", note)
	} else if note != "" {
		fmt.Fprintf(stderr, "benchwatch: %s\n", note)
	}

	stalled, sawOutput, waitErr := waitForCommand(cmd, logFile, opts)
	if !stalled {
		// 命令正常退出：仍在 Job 内运行的后代（孤儿）由 release 的 KILL_ON_JOB_CLOSE 收割。
		treeCtrl.release()
		return exitCodeFromWait(waitErr, stderr)
	}

	// 形态③：日志零增长超过上限 ⇒ 判定卡死。先把诊断打出来（含日志尾部），再终止进程树。
	reportStall(stdout, opts, sawOutput)
	used, note, killErr := treeCtrl.terminate(cmd, opts.grace)
	if killErr != nil {
		fmt.Fprintf(stderr, "benchwatch: 终止被监视命令失败: %v（%s）\n", killErr, note)
	} else {
		if used != "" {
			fmt.Fprintf(stderr, "benchwatch: 已终止进程树：%s\n", used)
		}
		if note != "" {
			fmt.Fprintf(stderr, "benchwatch: %s\n", note)
		}
	}
	treeCtrl.release()
	return stallExitCode
}

// waitForCommand 等待被监视命令结束，并在「日志文件零增长超过上限」时判定停滞。
//
// 判定依据是**输出是否还在增长**，而不是「耗时是否超限」：正常的 benchmark 整趟可跑 3–5 分钟，
// 单包也可达数十秒，任何「总时长上限」都会误杀；而「零增长」只可能出现在卡死（进程已退出由 done 分支处理）。
//
// 两个窗口（缺一不可）：
//   - **首个字节之前**用 opts.startup：`go test ./...` 建包/冷缓存时**本来就无输出**，用短窗口会误杀健康运行；
//   - **已有输出之后**用 opts.limit：benchmark 结果行每隔几秒就会刷出，长时间零增长只能是卡死。
func waitForCommand(cmd *exec.Cmd, logFile *os.File, opts options) (stalled, sawOutput bool, waitErr error) {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(opts.poll)
	defer ticker.Stop()
	lastSize := int64(-1) // -1 保证首次轮询视为「有进展」，让命令拿到完整的初始窗口
	lastGrowth := time.Now()
	for {
		select {
		case err := <-done:
			return false, sawOutput, err
		case <-ticker.C:
			size := fileSize(logFile, lastSize)
			if size != lastSize {
				lastSize = size
				lastGrowth = time.Now()
				if size > 0 {
					sawOutput = true
				}
				continue
			}
			if time.Since(lastGrowth) >= opts.windowFor(sawOutput) {
				return true, sawOutput, nil
			}
		}
	}
}

// windowFor 返回当前阶段的静默上限：首个字节之前用 startup（编译期静默正常），之后用 limit。
func (o options) windowFor(sawOutput bool) time.Duration {
	if sawOutput {
		return o.limit
	}
	return o.startup
}

// fileSize 返回日志当前字节数；取不到时退回 fallback（宁可漏报一次停滞，也不误报）。
func fileSize(f *os.File, fallback int64) int64 {
	st, err := f.Stat()
	if err != nil {
		return fallback
	}
	return st.Size()
}

// reportStall 打印停滞诊断：为何不是 go test 超时、以及日志尾部（卡在哪一步的关键线索）。
func reportStall(w io.Writer, opts options, sawOutput bool) {
	phase := "已开始输出后"
	window := opts.limit
	if !sawOutput {
		phase = "**首个字节之前**"
		window = opts.startup
	}
	fmt.Fprintf(w, "\n%s\n", stallBanner)
	fmt.Fprintf(w, "benchwatch: **输出停滞**（%s零增长 %s，-poll=%s）⇒ 判定 benchmark **卡死**（形态③：单个 op 不再返回）\n", phase, window, opts.poll)
	fmt.Fprintf(w, "  被监视命令: %s\n", strings.Join(opts.command, " "))
	fmt.Fprintf(w, "  日志文件: %s（已保留：CI 作为 artifact 上传，失败步骤也会打印其尾部）\n", opts.logPath)
	fmt.Fprintf(w, "  为何不是 go test 超时: `go test -timeout` 对 benchmark **不生效**（把 60s 睡眠放进\n")
	fmt.Fprintf(w, "    benchmark + `-timeout 5s` 仍会 PASS；同一睡眠放进 Test 才会 panic。A/B/C 实验见\n")
	fmt.Fprintf(w, "    docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md §3.2）\n")
	fmt.Fprintf(w, "    ⇒ 卡死只能由**进程外**看门狗发现；本工具将终止该命令及其后代并以退出码 %d 结束。\n", stallExitCode)
	fmt.Fprintf(w, "  日志尾部（最后 %d 行）:\n%s\n", opts.tailLines, tailLines(opts.logPath, opts.tailLines))
	fmt.Fprintf(w, "%s\n\n", stallBanner)
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	fs := flag.NewFlagSet("benchwatch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	startup := fs.Duration("startup", defaultStartup, "**首个字节之前**的静默上限：编译/冷缓存期间静默是正常的（go test ./... 建包时无输出）")
	limit := fs.Duration("limit", defaultLimit, "已开始输出后的停滞窗口：日志在此窗口内零增长即判定卡死")
	poll := fs.Duration("poll", defaultPoll, "检查日志增长的轮询间隔")
	grace := fs.Duration("grace", defaultGrace, "终止子进程的宽限期（先温和终止，宽限后再强制）")
	tail := fs.Int("tail", defaultTail, "停滞时打印日志尾部的行数")
	logPath := fs.String("log", "", "日志文件路径（被监视命令的输出同时写入此处与 stdout）")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "用法: benchwatch -limit 120s -startup 180s -log <path> -- <command> [args...]\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	opts := options{
		startup:   *startup,
		limit:     *limit,
		poll:      *poll,
		grace:     *grace,
		tailLines: *tail,
		logPath:   *logPath,
		command:   fs.Args(),
	}
	if len(opts.command) == 0 {
		return options{}, errors.New("缺少被监视命令：用法 benchwatch -limit 120s -log <path> -- <command> [args...]")
	}
	if len(splitEnvAssignments(opts.command)) == 0 {
		return options{}, errors.New("被监视命令只有环境赋值前缀、没有真正的命令：用法 benchwatch ... -- <command> [args...]")
	}
	if opts.logPath == "" {
		return options{}, errors.New("-log 必须指定（停滞诊断与 CI artifact 都依赖它）")
	}
	if opts.limit <= 0 || opts.poll <= 0 || opts.startup <= 0 {
		return options{}, fmt.Errorf("startup/limit/poll 必须为正：startup=%s limit=%s poll=%s", opts.startup, opts.limit, opts.poll)
	}
	if opts.poll > opts.limit {
		return options{}, fmt.Errorf("poll(%s) 不得大于 limit(%s)，否则无法在窗口内及时判定", opts.poll, opts.limit)
	}
	if opts.startup < opts.limit {
		return options{}, fmt.Errorf("startup(%s) 不得小于 limit(%s)：首个字节前的静默上限应更宽（编译期静默是正常的）", opts.startup, opts.limit)
	}
	if opts.tailLines < 0 {
		opts.tailLines = 0
	}
	return opts, nil
}

// splitEnvAssignments 把命令开头的 `KEY=VALUE` 前缀（shell 风格的环境赋值）剔除，
// 返回真正的命令。
//
// 为何需要：本仓 Makefile 的 `$(GO)` 展开为 `GOOS=… GOARCH=… go`（跨平台编译前缀），
// 而 `exec.Command` **不会**像 shell 那样解释它——不拆就会把 `GOOS=…` 当可执行文件。
func splitEnvAssignments(args []string) []string {
	i := 0
	for i < len(args) && isEnvAssignment(args[i]) {
		i++
	}
	return args[i:]
}

// isEnvAssignment 判断参数是否是 `KEY=VALUE` 形式的环境赋值（KEY 为合法标识符）。
func isEnvAssignment(arg string) bool {
	eq := strings.IndexByte(arg, '=')
	if eq <= 0 {
		return false
	}
	for j, r := range arg[:eq] {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if j == 0 {
				return false // 标识符不得以数字开头（如 `1=2` 不是赋值）
			}
		default:
			return false
		}
	}
	return true
}

// exitCodeFromWait 把 cmd.Wait() 的错误映射为退出码：正常 0；退出错误取子进程退出码；
// 其它错误（启动/IO）报 1 并打印原因。
func exitCodeFromWait(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		if code := ee.ExitCode(); code >= 0 {
			return code
		}
		// 被信号终止（例如 CI 取消）：统一报 1，避免与「停滞」的 66 混淆。
		fmt.Fprintf(stderr, "benchwatch: 被监视命令被信号终止: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "benchwatch: 等待被监视命令失败: %v\n", err)
	return 1
}

// tailLines 读取日志文件的最后 n 行（最多回读 tailReadLimit 字节，避免大日志撑爆内存）。
func tailLines(path string, n int) string {
	if n <= 0 {
		return "(未启用日志尾部)"
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(读取日志失败: %v)", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(读取日志失败: %v)", err)
	}
	if start := st.Size() - tailReadLimit; start > 0 {
		if _, seekErr := f.Seek(start, io.SeekStart); seekErr != nil {
			return fmt.Sprintf("(读取日志失败: %v)", seekErr)
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("(读取日志失败: %v)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// lockedWriter 把任意 io.Writer 包成互斥写者。
//
// 为什么需要：run() 里 exec.Cmd 用**内部 goroutine** 把子进程 stdout/stderr 拷到我们给的 writer
// （io.MultiWriter(logFile, stdout)），而同一 run() 在停滞时又直接 reportStall 写 stdout ⇒ 两个
// goroutine 并发写调用方的 writer。生产（os.Stdout/*os.File）并发写安全，但测试传的 bytes.Buffer
// 非线程安全 ⇒ `-race` 在 CI（多核）抓到、本地（单核节奏）没抓到。包一层互斥后，生产与测试
// 行为一致且无竞态；对 *os.File 的额外锁开销可忽略。
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// lockWriters 返回 run() 内部使用的互斥包装；nil 原样返回（调用方不可能传 nil，防御性）。
func lockWriters(stdout, stderr io.Writer) (io.Writer, io.Writer) {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	return &lockedWriter{w: stdout}, &lockedWriter{w: stderr}
}
