// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// test_resource_release_gate_test.go 是门禁 **R60**：测试夹具创建的**系统资源**
// （目录句柄 / debounce 落盘协程）必须显式释放。
//
// 背景（2026-11-27 两轮 CI 实测，均 `Test(windows)` 偶发）：
//   - `os.Root` 持有一个打开的目录 fd，且**没有 finalizer** ⇒ 不 Close 就持有到进程退出，
//     Windows 上 `t.TempDir()` 的 `RemoveAll` 报
//     `The process cannot access the file because it is being used by another process`；
//   - `checksum.ChecksumStore` 带 debounce 落盘协程（savemerger）⇒ 不 Close 会在测试结束后
//     仍写一次，报 `TempDir RemoveAll cleanup: ... The directory is not empty`。
//
// 判据：`*_test.go` 里每个 `OpenRoot(` / `NewChecksumStore(` 调用点，其后 N 行内必须出现
// `Close()` 或 `t.Cleanup(`（关闭归属可追溯）；确有例外登记到 `test_resource_exceptions.tsv`。
//
// 双侧下限护栏（防解析器失效假绿）：扫描文件数、匹配调用点数都不得低于下限。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	testResourceExceptionsTSV = "test_resource_exceptions.tsv"
	// testResourceWindow 是「关闭归属」的查找窗口（行）。
	testResourceWindow = 10
	// 下限护栏：低于此值说明扫描面/正则失效（会假绿）。
	testResourceMinFiles = 900
	testResourceMinSites = 145
)

// testResourcePattern 是「需要显式释放的资源构造函数」清单。
//
// 新增条目时同时确认：该构造函数确实持有进程级资源（fd / goroutine / listener），
// 且**没有** finalizer 兜底。
var testResourcePattern = regexp.MustCompile(
	`(?:storage\.)?OpenRoot\(|(?:storage\.)?OpenTenant\(|checksum\.NewChecksumStore\(|(?:^|[^.\w])NewChecksumStore\(`)

// openTestRootHelper 是本仓统一的「打开即登记关闭」助手名（命中即视为已登记）。
var openTestRootHelper = regexp.MustCompile(`openTestRoot\(|openRootForTest\(|testRootAt\(`)

// closeOwnership 判断窗口内是否出现关闭归属。
var closeOwnership = regexp.MustCompile(`\.Close\(\)|t\.Cleanup\(`)

// skipPathPrefixes 是扫描时跳过的目录（相对 module root）。
var skipPathPrefixes = []string{"build/", "node_modules/", ".git/", ".worktrees/", "vendor/"}

func TestR60_TestFixturesReleaseSystemResources(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	exceptions := readTestResourceExceptions(t)
	sites := 0
	files := 0
	var violations []string

	walkErr := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		// **相对 root 判定跳过**：worktree 绝对路径本身含 `/.worktrees/`，用绝对路径判会整仓跳过。
		if d.IsDir() {
			for _, p := range skipPathPrefixes {
				if strings.HasPrefix(rel+"/", p) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(rel, "_test.go") {
			return nil
		}
		// 门禁自身的匹配字符串不是真实调用（与 pkg/checksum 门禁同一处理）。
		if rel == "internal/archcheck/test_resource_release_gate_test.go" ||
			rel == "pkg/checksum/store_close_gate_test.go" {
			return nil // 门禁自身的匹配字符串不是真实调用
		}
		files++
		raw, ferr := os.ReadFile(path)
		if ferr != nil {
			return nil
		}
		lines := strings.Split(string(raw), "\n")
		for i, ln := range lines {
			trimmed := strings.TrimSpace(ln)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			if !testResourcePattern.MatchString(ln) {
				continue
			}
			// 助手函数内部自己登记关闭 / 定义处不计。
			if openTestRootHelper.MatchString(ln) || strings.HasPrefix(trimmed, "func ") {
				continue
			}
			sites++
			end := min(len(lines), i+1+testResourceWindow)
			window := strings.Join(lines[i:end], "\n")
			if closeOwnership.MatchString(window) {
				continue
			}
			key := rel + ":" + strconv.Itoa(i+1)
			if _, ok := exceptions[key]; ok {
				continue
			}
			violations = append(violations, key+"  "+trimmed)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("遍历仓库失败: %v", walkErr)
	}

	// 双侧下限护栏：扫描面被改窄 / 正则失效 ⇒ 立刻红（否则门禁假绿）。
	if files < testResourceMinFiles {
		t.Fatalf("R60 扫描到的测试文件数 %d < 下限 %d（扫描面被改窄？门禁会假绿）", files, testResourceMinFiles)
	}
	if sites < testResourceMinSites {
		t.Fatalf("R60 匹配到的资源构造调用点 %d < 下限 %d（正则失效？门禁会假绿）", sites, testResourceMinSites)
	}

	sort.Strings(violations)
	if len(violations) > 0 {
		t.Errorf("R60：测试夹具创建了系统资源但未登记关闭（Windows 上会让 t.TempDir() 清理失败；\n"+
			"   os.Root 无 finalizer、ChecksumStore 带 debounce 落盘协程）。\n"+
			"   修法：改用 openTestRoot(t, dir) 或紧跟 t.Cleanup(func(){ _ = X.Close() })；\n"+
			"   确有正当例外登记到 internal/archcheck/%s 并写理由：\n  %s",
			testResourceExceptionsTSV, strings.Join(violations, "\n  "))
	}
}

// readTestResourceExceptions 读例外清单（格式：`<rel>:<line>\t<理由>`；允许空文件）。
func readTestResourceExceptions(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	raw, err := os.ReadFile(testResourceExceptionsTSV)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("读取 %s: %v", testResourceExceptionsTSV, err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(strings.TrimPrefix(parts[1], ""))
	}
	return out
}
