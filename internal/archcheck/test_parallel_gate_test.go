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
// （TestSerialRatchetHelper，ARCHCHECK_WRITE_BASELINE=1 触发）重生成，避免手抄 246 行
// （246 行 / 1743 例是 **2026-09-15 存量快照**）。
//
// 目录排除口径与其他全仓遍历门禁**同源**（repoScanSkipDir，见 repo_walk_test.go）。
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
	return regexp.MustCompile(`(?m)^func (Test\w+)\(\w+ \*testing\.T\)[ \t]*\{`)
}()

// TestSerialGateRegexRecognizesTestForms 钉住扫描器的**识别面**：正则一旦写窄，
// 合法的测试写法会被静默漏检（不进棘轮、永远不报），而门禁本身依旧全绿——这是
// 本门禁最危险的失效形态（与「扫描根退回 `.`」同类）。
//
// 2026-09-15 实况：原正则 `^func (Test\w+)\(t \*testing\.T\) \{` 要求参数名恰好是 `t`
// 且 `)` 后紧跟「空格 + `{`」，因此 `func TestFoo(tt *testing.T) {` 与
// `func TestFoo(t *testing.T){` 都会被漏检；当前树上恰无这种写法（放宽前后计数一致
// 246/1743），属**未来防护**——一旦有人这样写，本用例先把问题暴露出来。
func TestSerialGateRegexRecognizesTestForms(t *testing.T) {
	t.Parallel()

	accepted := []string{
		"func TestFoo(t *testing.T) {",
		"func TestFoo(tt *testing.T) {", // 参数名不必是 t
		"func TestFoo(_ *testing.T) {",  // 下划线同样合法
		"func TestFoo(t *testing.T){",   // `{` 前无空格
		"func TestFoo(t *testing.T)\t{", // `{` 前是制表符
	}
	for _, line := range accepted {
		if !testSerialFuncRe.MatchString(line) {
			t.Errorf("应识别为顶层 Test 却未识别（会静默不进棘轮）：%s", line)
		}
	}

	rejected := []string{
		"func TestFoo(tb testing.TB) {",           // 非 *testing.T：Go 不认这种测试签名
		"func TestFoo(t *testing.T, extra int) {", // 参数不止一个：不是合法测试签名
		"func (s *Suite) TestFoo(t *testing.T) {", // 方法（带接收者）不是顶层测试
		"func helper(t *testing.T) {",             // 非 Test 前缀
		"// func TestFoo(t *testing.T) {",         // 注释行
	}
	for _, line := range rejected {
		if testSerialFuncRe.MatchString(line) {
			t.Errorf("不该识别为顶层 Test 却识别了（会误增棘轮计数）：%s", line)
		}
	}
}

// serialGateSelfPath 是本门禁自身（仓库相对路径）：它含扫描器与棘轮的串行用例，
// 不排除会把门禁自己算进预算（自我循环）。
const serialGateSelfPath = "internal/archcheck/test_parallel_gate_test.go"

// scanTestFiles 遍历全仓并返回两份统计：
//
//	serial         每个 _test.go 文件的顶层串行 Test 数（棘轮判据；只含 ≥1 例的文件）；
//	filesBySubtree 每棵顶层子树（"pkg/" 等）扫到的 _test.go **文件总数**（覆盖探针判据）。
//
// 为什么覆盖探针用「测试文件总数」而不是「仍有串行用例的文件数」：后者的键集会随棘轮收敛
// （某文件被整体并行化后即从键集消失）而**单向下滑**，方向与门禁期望相反 ⇒ 合法收敛会被
// 误诊为「扫描面漏掉子树」；文件总数只反映「门禁还看不看得见这棵子树」，与扫描面同向且变化慢。
//
// 扫描根必须是 moduleRoot(t) 而**不是** "."：go test 以**包目录**为 cwd，用 "." 只会
// 扫到 internal/archcheck 自身（**修复前实测** 9 个文件 / 14 个串行 Test；2026-09-16 实跑复核仍为
// 9/14——门禁自身文件此时不再被自排除，但它的 TestSerialRatchet 因错误文案里带 `t.Parallel()`
// 字面量被子串判据豁免、TestSerialRatchetHelper 计入，净 +1），全仓新增的串行用例根本不进门禁
// （R18 曾长期处于这种「看着全绿、实则只守自己」的状态）。
// serial 的键为**仓库相对**路径（与 serial_budgets.tsv 一致）：WalkDir 返回的路径带扫描根前缀，
// 必须先 Rel 剥掉再算键，否则基线的键会变成机器绝对路径。
func scanTestFiles(t *testing.T) (serial map[string]int, filesBySubtree map[string]int) {
	t.Helper()
	root := moduleRoot(t)
	serial = map[string]int{}
	filesBySubtree = map[string]int{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			// 只能对**目录**返回 fs.SkipDir：fs.SkipDir 作用在非目录条目上时，语义是
			// 「跳过它所在目录的**剩余条目**」——扫到仓库根的 .codecov.yml 就会把整次
			// 遍历截断（实测：全仓只扫到 3 个条目、串行 Test 统计为 0）。
			// 排除口径与其他全仓遍历门禁共用同一个事实源（repoScanSkipDir）。
			if path != root && repoScanSkipDir(base) {
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
		// 覆盖探针的度量：**扫到的**测试文件数（含门禁自身文件——它就是这棵树里的一个测试
		// 文件，含它才能真实反映「门禁看得见哪些子树」）。只按顶层路径段归并。
		if i := strings.IndexByte(rel, '/'); i > 0 {
			filesBySubtree[rel[:i+1]]++
		}
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
			serial[rel]++
		}
		return nil
	})
	return serial, filesBySubtree
}

// scanSerialTests 是棘轮与基线生成器用的薄封装（只要串行计数）。
func scanSerialTests(t *testing.T) map[string]int {
	t.Helper()
	serial, _ := scanTestFiles(t)
	return serial
}

// 覆盖探针下限（R13 风格防「静默变绿」）：扫描面一旦被改窄，棘轮就恒绿——例如退回
// `filepath.WalkDir(".")`（go test 以**包目录**为 cwd ⇒ 只扫到 internal/archcheck 自身，
// 修复前实测 9 个文件 / 14 个串行 Test），门禁看着全绿实则对全仓失效。
//
// 阈值取自 2026-09-15 全仓实测（246 个文件 / 1743 个串行 Test），留足余量：
// 文件数下限 200（≈81%）、串行总数下限 1000（≈57%）。
// 只做**下限**断言，不设上限——清理串行用例把数字压到下限以下是正当收敛，届时
// 同步下调阈值即可（这本就是棘轮的期望方向）。
//
// 合法收缩（而非扫描面被改窄）怎么办：若确因**大规模删除/合并测试**导致低于下限，
// 请同步下调本组常量与 serialScanSubtreeFloors，并在
// docs/testing/virtual-time-conversions.md 登记理由；**不要**靠放宽排除口径或跳过某棵
// 子树把探针哄绿——那正是它要拦的形态。
const (
	serialScanMinFiles = 200
	serialScanMinTotal = 1000
)

// serialScanSubtreeFloor 是一棵子树的下限行（见 serialScanSubtreeFloors）。
type serialScanSubtreeFloor struct {
	minFiles      int
	snapshotFiles int
}

// serialScanSubtreeFloors 是**逐子树**的「扫到的测试文件数」下限。
//
// 为什么要有它：只有全局下限时，把**整棵子树**排除（例如在 pkg/ 上返回 fs.SkipDir）仍可能
// 让全局数字留在下限之上 ⇒ 门禁对那棵子树静默失效，而全局探针看不出来。
//
// 度量与推导（2026-09-16 全仓实测，共 443 个测试文件）：下限取实测值的**约 60%**，取整
// 便于人工核对。该度量是「扫到的文件数」，**不会**因测试被并行化而下降（那是棘轮的收敛方向），
// 所以余量可以给得比旧口径更紧而不易假红；真正的合法下降只来自删除/合并测试文件。
var serialScanSubtreeFloors = map[string]serialScanSubtreeFloor{
	"pkg/":      {minFiles: 200, snapshotFiles: 334},
	"cmd/":      {minFiles: 35, snapshotFiles: 58},
	"internal/": {minFiles: 14, snapshotFiles: 24},
	"test/":     {minFiles: 10, snapshotFiles: 17},
	"web/":      {minFiles: 6, snapshotFiles: 10},
}

// formatSubtreeCounts 渲染逐子树实测快照（map 迭代无序，日志要能逐行 diff，故先排序）。
func formatSubtreeCounts(counts map[string]int, floors map[string]serialScanSubtreeFloor) string {
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if f, ok := floors[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%d(下限%d/快照%d)", k, counts[k], f.minFiles, f.snapshotFiles))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%d", k, counts[k]))
	}
	return strings.Join(parts, " ")
}

// TestSerialGateScanCoverage 断言 R18 的扫描面确实覆盖全仓（见上面阈值推导）。
func TestSerialGateScanCoverage(t *testing.T) {
	t.Parallel()
	serial, filesBySubtree := scanTestFiles(t)
	total := 0
	for _, c := range serial {
		total += c
	}
	for f := range serial {
		if strings.HasPrefix(f, "build/") || strings.HasPrefix(f, ".git/") || strings.Contains(f, "node_modules/") ||
			strings.HasPrefix(f, "dist/") || strings.HasPrefix(f, "vendor/") {
			t.Fatalf("扫描面混入非源文件 %s：应排除 build/.git/node_modules/vendor/dist 与隐藏目录", f)
		}
	}
	// 诊断（-v 可见）：贴出实测快照，便于核对阈值与定位「哪棵子树变少了」。
	t.Logf("R18 扫描面实测：串行文件 %d 个 / 串行 %d 例；逐子树测试文件数 %s",
		len(serial), total, formatSubtreeCounts(filesBySubtree, serialScanSubtreeFloors))

	// 逐子树下限：防止「整棵子树被排除而全局数字仍达标」的局部静默失效。
	// 度量是**扫到的测试文件数**（不是「仍有串行用例的文件数」：后者随棘轮收敛单向下滑，
	// 合法收敛会被误诊为扫描面漏掉子树——详见 scanTestFiles 的说明）。
	for prefix, floor := range serialScanSubtreeFloors {
		if got := filesBySubtree[prefix]; got < floor.minFiles {
			t.Fatalf("扫描面疑似漏掉子树 %s：只扫到 %d 个测试文件（下限 %d，2026-09-16 实测 %d）。\n"+
				"整棵子树被排除（例如在该子树内返回 fs.SkipDir）时全局下限仍可能达标 ⇒ 门禁对那棵子树静默失效。\n"+
				"若确因**合法收缩**（大规模删除/合并测试文件）导致低于下限，请同步下调 serialScanSubtreeFloors，"+
				"并在 docs/testing/virtual-time-conversions.md 登记理由；**不要**靠放宽排除口径把探针哄绿。",
				prefix, got, floor.minFiles, floor.snapshotFiles)
		}
	}
	if len(serial) < serialScanMinFiles || total < serialScanMinTotal {
		t.Fatalf("R18 扫描面疑似被改窄：只扫到 %d 个文件 / %d 个串行 Test（下限 %d / %d）。\n"+
			"扫描根必须用 moduleRoot(t)：go test 以**包目录**为 cwd，`filepath.WalkDir(\".\")` 只会扫到 "+
			"internal/archcheck 自身（修复前实测 9/14）⇒ 棘轮恒绿、全仓新增串行用例无人拦。\n"+
			"若确因**大规模删除/合并测试**（而非扫描面被改窄）导致低于下限，请同步下调 "+
			"serialScanMinFiles/serialScanMinTotal 与 serialScanSubtreeFloors，并在 "+
			"docs/testing/virtual-time-conversions.md 登记理由。",
			len(serial), total, serialScanMinFiles, serialScanMinTotal)
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
