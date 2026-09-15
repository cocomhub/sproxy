// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// makefile_target_dup_test.go 门禁：Makefile 中同一目标不得被定义两次。
//
// 动机（实测事故）：`bench-old` 曾被定义两遍——先是一条 `bench-old: bench-local` 别名，
// 之后又整段复制了 `bench-local` 的配方（只差 `-count=1`）。GNU make 对重复目标**不报错**，
// 只用后一份覆盖前一份（并打印 `Makefile:NNN: warning: overriding recipe for target`，
// 轻易被 build 日志吞掉）⇒ 读者以为 `bench-old` 是别名，实际跑的是那份复制的旧配方（含
// `… | tee` 吞退出码的老写法）。注意：GNU make 只对「**两条规则都带配方**」的重复才覆盖并告警；
// 只带先决条件、无配方的重复规则是**静默合并**的——本门禁一刀切禁止重复定义（比 make 更严），
// 因为「哪天有人给另一处补上配方」正是这类事故的入口。
package archcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMakefileNoDuplicateTargets 断言 Makefile 里每个目标名只被定义一次。
//
// 判据：非 recipe（不以 Tab 开头）、非注释、非变量赋值（`=` 出现在首个 `:` 之前）、
// 非特殊目标（以 `.` 开头的 `.PHONY` 等可合法重复）的行才视为目标定义行；规则行 `:` 左侧的
// 每个名字都算被定义（支持 `test-ci test-cover:` 这类多目标规则）。
func TestMakefileNoDuplicateTargets(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("读取 Makefile: %v", err)
	}

	seen := map[string]int{}
	var order []string
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ") ||
			strings.HasPrefix(line, "#") || strings.HasPrefix(line, ".") {
			continue
		}
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		if eq := strings.Index(line, "="); eq >= 0 && (eq < colon || eq == colon+1) {
			continue // 变量赋值（= / := / ?= / +=）
		}
		for name := range strings.FieldsSeq(line[:colon]) {
			if seen[name] == 0 {
				order = append(order, name)
			}
			seen[name]++
		}
	}

	// 正探针：解析面必须非平凡（防「一个目标都没解析到 ⇒ 断言空转」）。
	if len(seen) < 40 {
		t.Fatalf("只解析到 %d 个 Makefile 目标（判据失效，需更新本测试）", len(seen))
	}

	var dups []string
	for _, name := range order {
		if seen[name] > 1 {
			dups = append(dups, name)
		}
	}
	if len(dups) > 0 {
		t.Fatalf("Makefile 存在重复定义的目标：%s。GNU make 不报错，只用**后一份**覆盖前一份"+
			"（只在 stderr 留一行 overriding recipe warning）⇒ 前者形同虚设。请删除多余定义。",
			strings.Join(dups, ", "))
	}
}
