// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"strings"
	"testing"
)

// repo_walk_test.go 是「全仓源码遍历类门禁」**共用的目录排除口径**（单一事实源）。
//
// 为什么单列一处：R18 并发注册（test_parallel_gate_test.go）、R14 睡眠棘轮
// （test_sleep_ratchet_test.go）、R11 死代码（dead_symbols_test.go）、子 module 边界
// （submodule_test.go）都要遍历整棵源码树，却各自内联了一份排除清单，写法互不相同
// （有的只列 node_modules/build/vendor，有的另列 .claude/.git，有的漏了 dist）——
// 结果是「同一个仓库、不同门禁看到的文件集不同」：排查「门禁为什么没抓到」时极易误判，
// 新增目录时也容易只改一处。
//
// 口径 = 各门禁原有清单的**并集**：
//   - 任意隐藏目录（`.` 开头：.git/.github/.learnings/.claude/.trae/...）——不是产品源码，
//     且可能含大体积拷贝；
//   - node_modules / build / vendor / dist——依赖与构建产物。
//
// 调用约定：**只对目录**用它（`d.IsDir()` 分支内）。fs.SkipDir 作用在**文件**条目上时
// 语义是「跳过该文件所在目录的剩余条目」，会截断整次遍历——R18 曾因此把全仓统计成 0
// （扫到仓库根的 .codecov.yml 就停了）。
func repoScanSkipDir(base string) bool {
	if strings.HasPrefix(base, ".") {
		return true
	}
	switch base {
	case "node_modules", "build", "vendor", "dist":
		return true
	}
	return false
}

// TestRepoScanSkipDir_PinnedSets 钉住排除口径**本身**（R13 风格防静默退化）。
//
// 动机：本仓今天没有 `dist/`、`node_modules/`、`vendor/` 目录，因此把清单里任一项删掉、
// 或删掉隐藏目录判断，四道门禁**依旧全绿**——口径静默退化的唯一表现就是「没人报错」。
// 这与 R18 的识别面（`TestSerialGateRegexRecognizesTestForms`）是同一类失效：门禁不再
// 看得见它本该拦的东西，而它自己不会红。
//
// 断言的是**函数级契约**（不依赖仓库当前目录结构），所以两侧都取固定样本：
// 排除侧含本仓当前并不存在的 `dist`/`node_modules`/`vendor`（这正是必须钉住的原因），
// 扫描侧取源码子树名（含 `archcheck`，它是 `internal/` 下一层、不该被名字规则误伤）。
func TestRepoScanSkipDir_PinnedSets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		base     string
		excluded bool
	}{
		{".git", true},
		{".github", true},
		{".superpowers", true},
		{"node_modules", true},
		{"build", true},
		{"vendor", true},
		{"dist", true},
		{"pkg", false},
		{"web", false},
		{"cmd", false},
		{"internal", false},
		{"test", false},
		{"tools", false},
		{"docs", false},
		{"archcheck", false},
	} {
		if got := repoScanSkipDir(tc.base); got != tc.excluded {
			t.Errorf("repoScanSkipDir(%q) = %v，want %v；该口径是四道全仓遍历门禁的**并集**，"+
				"任何增删都是显式决策（排除项一多就漏扫源码，一少就扫到依赖/构建产物）",
				tc.base, got, tc.excluded)
		}
	}
}
