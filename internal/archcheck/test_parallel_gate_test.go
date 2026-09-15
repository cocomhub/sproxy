// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// 门禁 R18「测试并发注册」——用户明示 2026-09-16：
//
//	所有顶层 Test 默认必须 t.Parallel()；无法并发者必须显式登记（白名单棘轮
//	只减不增）。新增测试直接满足设计，禁止「先串行后补规范化」。
//
// 扫描面是**全仓**（扫描根 moduleRoot(t)，见 scanSerialTests）；基线形态为数据文件
// internal/archcheck/serial_budgets.tsv（`仓库相对路径\t实测串行数`），由 helper
// （TestSerialRatchetHelper，ARCHCHECK_WRITE_BASELINE=1 触发）重生成，避免手抄 246 行。
//
// 棘轮判据是**两层**：① 逐文件计数只减不增；② 全仓总数只减不增。后者是主判据——
// 新增文件（未登记 ⇒ 预算 0）与新增用例都只在总数上体现。
//
// 新增顶层 Test **必须**直接满足设计（默认 t.Parallel()），而不是「先串行后补规范化」；
// 确实不能并发的，在**函数体内**加 `// sproxy:serial: <理由>` 并同步在
// docs/testing/virtual-time-conversions.md 登记理由（既有 1743 处是 2026-09-15 的存量
// 快照，按类别登记口径，不逐一具名；提高任何预算行仍需先登记再重生成基线）。

// serial_budgets.tsv 与门禁同目录（包目录内），避免不同 go test cwd 下的解析差异。
var serialBudgetPath = func() string {
	dir := os.Getenv("SREPO_DIR")
	if dir == "" {
		return "serial_budgets.tsv"
	}
	return filepath.Join(dir, "internal", "archcheck", "serial_budgets.tsv")
}()

var testSerialFuncRe = func() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\) \{`)
}()

// serialGateSelfPath 是本门禁自身（仓库相对路径）：它含扫描器与棘轮的串行用例，
// 不排除会把门禁自己算进预算（自我循环）。
const serialGateSelfPath = "internal/archcheck/test_parallel_gate_test.go"

// scanSerialTests 返回每个测试文件的顶层串行 Test 数（不并行且非 setenv 强制串）。
//
// 扫描根必须是 moduleRoot(t) 而**不是** "."：go test 以**包目录**为 cwd，用 "." 只会
// 扫到 internal/archcheck 自身（实测 9 个文件 / 14 个串行 Test），全仓新增的串行用例
// 根本不进门禁（R18 曾长期处于这种「看着全绿、实则只守自己」的状态）。
// 键为**仓库相对**路径（与 serial_budgets.tsv 一致）：WalkDir 返回的路径带扫描根前缀，
// 必须先 Rel 剥掉再算键，否则基线的键会变成机器绝对路径。
func scanSerialTests(t *testing.T) map[string]int {
	t.Helper()
	root := moduleRoot(t)
	counts := map[string]int{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			// 只能对**目录**返回 fs.SkipDir：fs.SkipDir 作用在非目录条目上时，语义是
			// 「跳过它所在目录的**剩余条目**」——扫到仓库根的 .codecov.yml 就会把整次
			// 遍历截断（实测：全仓只扫到 3 个条目、串行 Test 统计为 0）。
			if path != root && (base == "node_modules" || base == "build" || base == "vendor" || base == "dist" || strings.HasPrefix(base, ".")) {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(base, ".") {
			return nil
		}
		if !strings.HasSuffix(base, "_test.go") {
			return nil
		}
		relPath, relErr := filepath.Rel(root, path)
		if relErr != nil {
			t.Fatalf("相对化 %s（root=%s）失败: %v", path, root, relErr)
		}
		// ToSlash：Windows 下统一为 "/"，否则基线的键跨平台不可比（与 serial_budgets.tsv
		// 里既有条目的写法一致）。
		rel := filepath.ToSlash(relPath)
		if rel == serialGateSelfPath {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			t.Fatalf("read %s: %v", rel, rerr)
		}
		for _, loc := range testSerialFuncRe.FindAllStringIndex(string(data), -1) {
			fend := strings.IndexByte(string(data[loc[0]:]), '{')
			if fend < 0 {
				continue
			}
			fend += (loc[0] + 1)
			depth, i := 1, fend
			for i < len(data) && depth > 0 {
				c := data[i]
				switch c {
				case '{':
					depth++
				case '}':
					depth--
				}
				i++
			}
			body := string(data[fend:i])
			if strings.Contains(body, "t.Parallel()") {
				continue
			}
			if strings.Contains(body, "t.Setenv") ||
				strings.Contains(body, "os.Chdir") ||
				strings.Contains(body, "t.Chdir") {
				continue
			}
			// 显式登记的不可并发用例（函数体内含标记注释）自动豁免；标记格式：
			// `// sproxy:serial: 原因摘要`。
			if strings.Contains(body, "sproxy:serial:") {
				continue
			}
			counts[rel]++
		}
		return nil
	})
	return counts
}

// 覆盖探针下限（R13 风格防「静默变绿」）：扫描面一旦被改窄，棘轮就恒绿——例如退回
// `filepath.WalkDir(".")`（go test 以**包目录**为 cwd ⇒ 只扫到 internal/archcheck 自身，
// 实测 9 个文件 / 14 个串行 Test），门禁看着全绿实则对全仓失效。
//
// 阈值取自 2026-09-15 全仓实测（246 个文件 / 1743 个串行 Test），留足余量：
// 文件数下限 200（≈81%）、串行总数下限 1000（≈57%）。
// 只做**下限**断言，不设上限——清理串行用例把数字压到下限以下是正当收敛，届时
// 同步下调阈值即可（这本就是棘轮的期望方向）。
const (
	serialScanMinFiles = 200
	serialScanMinTotal = 1000
)

// TestSerialGateScanCoverage 断言 R18 的扫描面确实覆盖全仓（见上面阈值推导）。
func TestSerialGateScanCoverage(t *testing.T) {
	t.Parallel()
	counts := scanSerialTests(t)
	total := 0
	for _, c := range counts {
		total += c
	}
	for f := range counts {
		if strings.HasPrefix(f, "build/") || strings.HasPrefix(f, ".git/") || strings.Contains(f, "node_modules/") {
			t.Fatalf("扫描面混入非源文件 %s：应排除 build/.git/node_modules/vendor/dist 与隐藏目录", f)
		}
	}
	if len(counts) < serialScanMinFiles || total < serialScanMinTotal {
		t.Fatalf("R18 扫描面疑似被改窄：只扫到 %d 个文件 / %d 个串行 Test（下限 %d / %d）。\n"+
			"扫描根必须用 moduleRoot(t)：go test 以**包目录**为 cwd，`filepath.WalkDir(\".\")` 只会扫到 "+
			"internal/archcheck 自身（实测 9/14）⇒ 棘轮恒绿、全仓新增串行用例无人拦。",
			len(counts), total, serialScanMinFiles, serialScanMinTotal)
	}
}

// TestSerialRatchet 白名单棘轮：串行 Test 总数与逐文件计数只减不增。
func TestSerialRatchet(t *testing.T) {
	counts := scanSerialTests(t)
	data, err := os.ReadFile(serialBudgetPath)
	if err != nil {
		t.Fatalf("读取 %s 失败（先跑 ARCHCHECK_WRITE_BASELINE=1 的 helper 生成基线）：%v", serialBudgetPath, err)
	}
	budget := readSerialBudget(t, string(data))
	base := 0
	for _, b := range budget {
		base += b
	}
	for f, b := range budget {
		c, ok := counts[f]
		if !ok {
			continue
		}
		if c > b {
			t.Fatalf("文件 %s 的串行 Test 数 %d 超过登记值 %d——新增测试必须默认 t.Parallel()，或在 docs/testing/virtual-time-conversions.md 登记不可并发理由后更新 %s", f, c, b, serialBudgetPath)
		}
	}
	sum := 0
	for _, c := range counts {
		sum += c
	}
	if sum > base {
		t.Fatalf("串行 Test 总数 %d 超基线 %d（新增文件/用例须先登记 %s）", sum, base, serialBudgetPath)
	}
}

func readSerialBudget(t *testing.T, text string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 2 {
			t.Fatalf("serial_budgets.tsv 存在非法行: %q", line)
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil {
			t.Fatalf("serial_budgets.tsv 格式错: %s", line)
		}
		out[parts[0]] = n
	}
	return out
}

// TestSerialRatchet_helper 是白名单基线的生成器（默认 no-op；显式触发）：
//
//	ARCHCHECK_WRITE_BASELINE=1 go test -run TestSerialRatchetHelper ./internal/archcheck/
//
// 重新生成 serial_budgets.tsv 并打印总量（棘轮起点由当前扫描自动确认，禁止上行；
// 上行须人工评估并在台账登记理由）。
func TestSerialRatchetHelper(t *testing.T) {
	if os.Getenv("ARCHCHECK_WRITE_BASELINE") != "1" {
		t.Skip("设置 ARCHCHECK_WRITE_BASELINE=1 才会重写 serial_budgets.tsv 基线")
	}
	counts := scanSerialTests(t)
	total := 0
	var lines []string
	for f, c := range counts {
		lines = append(lines, fmt.Sprintf("%s\t%d\n", f, c))
		total += c
	}
	sort.Strings(lines)
	if err := os.WriteFile(serialBudgetPath, []byte(strings.Join(lines, "")), 0o644); err != nil {
		t.Fatalf("写 %s: %v", serialBudgetPath, err)
	}
	t.Logf("刷新 %s；串行 Test 总数=%d（基线登记）", serialBudgetPath, total)
}
