// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// makefile_bench_timeout_test.go 门禁：benchmark 入口必须设置**包级** `-timeout`，且必须
// **早于** CI Benchmark job 的 `timeout-minutes` 触发。
//
// 动机（2026-09-16 实证的**第三种**超时形态：单个 op 卡死不再返回，而不是「慢但会返回」）：
// op 级停滞守卫 `benchStallErr` 只在 op **返回后**测量耗时，所以对「永不返回」没有测量点；
// 而 `go test` 的 `-timeout` 默认是 **10 分钟**，长于
// `.github/workflows/benchmark-manual.yml` 里 Benchmark job 的 `timeout-minutes: 15` ⇒ **job 级取消先发生**，
// 日志里只剩 `Terminate orphan process: pid (…) (server.test)`，**没有任何 goroutine 栈**，
// 无法定位卡在哪一层（§1 早已记录这个痛点）。
//
// **重要更正（同日晚些的实验）**：`-timeout` **对 benchmark 不生效**——把 60s 睡眠放进 benchmark
// 并加 `-timeout 5s`，用例仍会 PASS；同一睡眠放进 Test 才会 `panic: test timed out`（A/B/C 实验见
// docs/archive/benchmark-ci-timeout-disk-io.md §3.2 的更正小节）。
// ⇒ 本 flag **不是**形态③的对策（它给不出栈，也拦不住卡死），保留它只是给这两个目标里的
// **非 benchmark 测试**留一个预防性兜底（`-run=^$` 下目前无测试）；形态③的真正机制是
// **进程外看门狗** tools/benchwatch，由 makefile_bench_watchdog_test.go 门禁。
//
// 取证：run `34994978562` / job `104468842282`（Benchmark，状态 CANCELLED）——全部 `ns/op` 行的
// 时间戳只跨 `16:27:03 → 16:27:42`（39 s），job 却在 `16:32:52` 才被掐断（+5 分钟），掐断时
// `server.test` 仍存活；日志中无任何 FAIL/panic 行（即停滞守卫未触发）。
//
// 判据（三段，缺一不可）：
//  1. `bench` 与 `bench-local` 的 recipe 必须出现 `-timeout $(BENCH_TIMEOUT)`——用变量而非字面量，
//     既便于本地临时放宽，也让本门禁能读出实际值；
//  2. `BENCH_TIMEOUT` 必须被定义且可被 `time.ParseDuration` 解析，且落在合理区间内（防解析抓错行）；
//  3. `BENCH_TIMEOUT` 必须**严格小于** CI Benchmark job 的 `timeout-minutes`——否则包内超时
//     仍会输给 job 级取消（对测试类超时是真实风险；对 benchmark 卡死由看门狗负责）。
//
// 另附：`bench` 必须保留「把 go test 退出码写进文件再 exit」的传播写法（管道退出码取自 `tee`
// 会吞掉失败，见 §4.2 的既有事故）。
//
// 三形态判据与取证：docs/archive/benchmark-ci-timeout-disk-io.md §3.2
// （含「-timeout 对 benchmark 不生效」的更正小节）

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// benchJobTimeoutMinutes 解析 Benchmark workflow 里 `benchmark` job 的 timeout-minutes。
// 2026-09-22 起 Benchmark 从 ci.yml 拆出为**手动触发**的独立 workflow（benchmark-manual.yml）：
// PR 检查不再包含 Benchmark，但包级 `-timeout` 仍须早于该 job 的 timeout-minutes 触发
// （手动跑基准时同样需要看门狗先于 job 取消拿诊断）。
// 判据：先定位行首两空格的 `benchmark:`（job 名），再向下最多 40 行找 `timeout-minutes: N`。
func benchJobTimeoutMinutes(t *testing.T, root string) int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "benchmark-manual.yml"))
	if err != nil {
		t.Fatalf("读取 benchmark-manual.yml: %v", err)
	}
	lines := strings.Split(string(data), "\n")
	reTimeout := regexp.MustCompile(`^\s*timeout-minutes:\s*(\d+)\s*$`)
	start := -1
	for i, ln := range lines {
		if ln == "  benchmark:" {
			start = i
			break
		}
	}
	if start < 0 {
		t.Fatalf("benchmark-manual.yml 中未找到 Benchmark job（期望行首两空格的 `benchmark:`）——门禁自检失败")
	}
	for i := start; i < len(lines) && i < start+40; i++ {
		if m := reTimeout.FindStringSubmatch(lines[i]); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("解析 timeout-minutes 失败: %v", err)
			}
			return n
		}
	}
	t.Fatalf("Benchmark job 未声明 timeout-minutes——门禁依赖它做「包级超时须更早触发」的判据")
	return 0
}

// makefileVar 解析 Makefile 中的简单变量赋值（支持 `:=` 与 `?=`），返回去掉行内注释后的值。
func makefileVar(t *testing.T, root, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(name) + `\s*[:?]=\s*([^#\n]+)`)
	m := re.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("Makefile 中未找到变量 %s（门禁要求显式定义，便于读取与本地覆盖）", name)
	}
	return strings.TrimSpace(m[1])
}

// TestBenchTargetsHavePackageTimeout 见文件头注释。
func TestBenchTargetsHavePackageTimeout(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	byName := map[string]makeTarget{}
	for _, g := range parseMakefileTargets(t, root) {
		byName[g.name] = g
	}

	// ① 两个 benchmark 入口都要带 -timeout $(BENCH_TIMEOUT)
	//
	// 只认**非注释**的 recipe 行，且 flag 必须落在真正调用 `$(GO) test` 的那一行上：
	// recipe 里常有 `@#` 解释性注释，若把它们也算进去，门禁会在「注释里提到该 flag」时
	// 假绿（本门禁初版正是被自己的变异验证抓到：删掉真实 flag 后仍 PASS）。
	for _, name := range []string{"bench", "bench-local"} {
		g, ok := byName[name]
		if !ok {
			t.Fatalf("Makefile 缺少目标 %s", name)
		}
		var cmd, all []string
		for _, r := range g.recipes {
			if strings.HasPrefix(r, "@#") || strings.HasPrefix(r, "#") {
				continue
			}
			all = append(all, r)
			if strings.Contains(r, "$(GO) test") {
				cmd = append(cmd, r)
			}
		}
		if len(cmd) == 0 {
			t.Fatalf("目标 %s 未解析到 `$(GO) test` 命令行（解析口径可能变化，门禁自检失败）：\n%s", name, strings.Join(all, "\n"))
		}
		if !strings.Contains(strings.Join(cmd, "\n"), "-timeout $(BENCH_TIMEOUT)") {
			t.Errorf("目标 %s 的 `$(GO) test` 命令行必须带 `-timeout $(BENCH_TIMEOUT)`：对非 benchmark 测试的预防性兜底，且防回退到默认 10m\n%s", name, strings.Join(cmd, "\n"))
		}
		if name == "bench" {
			joined := strings.Join(all, "\n")
			if !strings.Contains(joined, ".go_test_rc") || !strings.Contains(joined, "exit $$rc") {
				t.Errorf("目标 bench 必须保留「go test 退出码写文件后 exit $$rc」的传播写法（管道退出码取自 tee 会吞失败）：\n%s", joined)
			}
		}
	}

	// ② BENCH_TIMEOUT 必须可解析且落在合理区间（正向探针，防解析抓错行）
	raw := makefileVar(t, root, "BENCH_TIMEOUT")
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("BENCH_TIMEOUT=%q 不是合法 duration: %v", raw, err)
	}
	if d < 30*time.Second || d > 5*time.Minute {
		t.Errorf("BENCH_TIMEOUT=%s 超出合理区间 [30s, 5m]：过小会在慢 runner 上误报，过大则失去「先于 job 取消」的意义", d)
	}

	// ③ 必须严格小于 job 的 timeout-minutes
	jobMin := benchJobTimeoutMinutes(t, root)
	if jobMin < 1 || jobMin > 60 {
		t.Fatalf("Benchmark job 的 timeout-minutes=%d 超出合理区间（门禁自检失败）", jobMin)
	}
	if d >= time.Duration(jobMin)*time.Minute {
		t.Errorf("BENCH_TIMEOUT=%s 必须**严格小于** Benchmark job 的 timeout-minutes=%dm：否则包内超时输给 job 级取消（对测试类超时是真实风险；对 benchmark 卡死则由 tools/benchwatch 负责，见 makefile_bench_watchdog_test.go）", d, jobMin)
	}
	t.Logf("benchmark 包级超时=%s，Benchmark job timeout-minutes=%dm（余量 %s）", d, jobMin, time.Duration(jobMin)*time.Minute-d)
}
