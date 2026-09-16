// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// makefile_bench_watchdog_test.go 门禁：`make bench` 必须经**进程外**停滞看门狗
// `tools/benchwatch` 运行 benchmark，且两个静默窗口（startup / stall）必须存在、可解析、
// 且**早于**包级 `-timeout`（否则「卡死」永远等不到诊断）。
//
// 为什么必须有这个门禁（2026-09-16 实验更正，详见
// docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md §3.2）：
//   - `go test -timeout` **对 benchmark 不生效**（把 60s 睡眠放进 benchmark + `-timeout 5s` 仍 PASS；
//     同一睡眠放进 Test 才会 panic）⇒ 形态③（单个 op 卡死不再返回）在 CI 上只剩
//     `Terminate orphan process`、**没有任何 goroutine 栈**，job 被 6 分钟静默取消；
//   - 进程内看门狗也不保证有机会运行（卡死可能是 syscall 级不可中断 I/O）⇒ 只有独立进程可靠；
//   - 于是「谁把 bench 改回裸 `go test`（或去掉监视）」必须被门禁拦住，否则该形态会静默回退。
//
// 判据（四段）：
//  1. `bench` 的非注释 recipe 里必须出现 `./tools/benchwatch`（且带 `-startup $(BENCH_STARTUP_GRACE)`
//     与 `-limit $(BENCH_STALL_LIMIT)`）——用变量而非字面量，便于本地覆盖也让本门禁能读出实际值；
//  2. 两个变量都必须定义且可被 `time.ParseDuration` 解析；
//  3. 两者都必须**严格小于** `BENCH_TIMEOUT`（静默窗口必须比包级超时更早触发，否则形同虚设）；
//  4. `BENCH_STARTUP_GRACE >= BENCH_STALL_LIMIT`（首个字节前的静默上限应更宽：建包期无输出是正常的）。
//
// 另附自检：`bench` 必须解析到 recipe，且必须命中看门狗行——否则本门禁会在「解析口径变化」时
// 静默变成空判（初版 `makefile_bench_timeout_test.go` 就吃过这个亏）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBenchTargetRunsThroughWatchdog 见文件头注释。
func TestBenchTargetRunsThroughWatchdog(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	var bench *makeTarget
	for _, g := range parseMakefileTargets(t, root) {
		if g.name == "bench" {
			g := g
			bench = &g
			break
		}
	}
	if bench == nil {
		t.Fatalf("Makefile 缺少目标 bench（门禁自检失败）")
	}

	// ① 看门狗必须在**非注释**的 recipe 里被真正调用（`@#` 注释里提到不算）。
	//
	// 注意 recipe 可跨物理行（反斜杠续行）：构建看门狗的那一行只引用 `./tools/benchwatch`，
	// 而带两个窗口的调用在下一行（用 `$(BUILD_DIR)/bench/benchwatch`）⇒ 分别判定：
	//   - 全量非注释 recipe 里必须引用 `./tools/benchwatch`（工具路径可构建）；
	//   - 必须存在**同一行**同时含 `benchwatch` 与两个窗口变量的调用（防「只构建不调用」）。
	var body []string
	for _, r := range bench.recipes {
		if strings.HasPrefix(r, "@#") || strings.HasPrefix(r, "#") {
			continue
		}
		body = append(body, r)
	}
	joined := strings.Join(body, "\n")
	if !strings.Contains(joined, "./tools/benchwatch") {
		t.Fatalf("目标 bench 必须构建/引用 `./tools/benchwatch`（进程外看门狗）：`go test -timeout` 对 benchmark 不生效，"+
			"卡死时只会被 job 级静默取消（无诊断）\nrecipe:\n%s", joined)
	}
	var watchdogLine string
	for _, r := range body {
		if strings.Contains(r, "benchwatch") && strings.Contains(r, "-startup $(BENCH_STARTUP_GRACE)") {
			watchdogLine = r
			break
		}
	}
	if watchdogLine == "" {
		t.Fatalf("目标 bench 必须**在同一行**用看门狗带两个静默窗口运行 benchmark"+
			"（-startup $(BENCH_STARTUP_GRACE) 与 -limit $(BENCH_STALL_LIMIT)）\nrecipe:\n%s", joined)
	}
	for _, want := range []string{"-startup $(BENCH_STARTUP_GRACE)", "-limit $(BENCH_STALL_LIMIT)"} {
		if !strings.Contains(watchdogLine, want) {
			t.Errorf("看门狗调用必须带 %q（两个静默窗口缺一不可）\n实际: %s", want, watchdogLine)
		}
	}

	// ② 两个窗口必须可解析。
	stall := mustDuration(t, root, "BENCH_STALL_LIMIT")
	startup := mustDuration(t, root, "BENCH_STARTUP_GRACE")

	// ③ 都必须早于包级 -timeout。
	timeout := mustDuration(t, root, "BENCH_TIMEOUT")
	if stall >= timeout {
		t.Errorf("BENCH_STALL_LIMIT=%s 必须**严格小于** BENCH_TIMEOUT=%s：否则包级超时先触发，"+
			"静默窗口形同虚设（且拿不到看门狗诊断）", stall, timeout)
	}
	if startup >= timeout {
		t.Errorf("BENCH_STARTUP_GRACE=%s 必须**严格小于** BENCH_TIMEOUT=%s（首个字节前的静默上限也应能在包级超时前失败）",
			startup, timeout)
	}

	// ④ startup 不得小于 stall。
	if startup < stall {
		t.Errorf("BENCH_STARTUP_GRACE=%s 不得小于 BENCH_STALL_LIMIT=%s：首个字节前（建包/冷缓存）静默是正常的，"+
			"窗口应更宽，否则会误杀健康运行", startup, stall)
	}

	// ⑤ 工具本体必须存在且是 main 包（防「wiring 指向不存在的工具」）。
	mainPath := filepath.Join(root, "tools", "benchwatch", "main.go")
	src, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatalf("读取 %s: %v（wiring 必须指向真实存在的看门狗）", mainPath, err)
	}
	if !strings.Contains(string(src), "package main") {
		t.Errorf("%s 必须是 package main", mainPath)
	}
	t.Logf("看门狗窗口：startup=%s stall=%s（BENCH_TIMEOUT=%s）", startup, stall, timeout)
}

// mustDuration 读取并解析 Makefile 变量为 duration。
func mustDuration(t *testing.T, root, name string) time.Duration {
	t.Helper()
	raw := makefileVar(t, root, name)
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("Makefile 变量 %s=%q 不是合法 duration: %v", name, raw, err)
	}
	return d
}
