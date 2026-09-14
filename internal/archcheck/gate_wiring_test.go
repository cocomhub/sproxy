// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// gate_wiring_test.go 是「**门禁自身可用性**」的守卫（R13）。
//
// 动机：门禁最危险的失效方式不是报红，而是**静默变绿**。2026-09-14 的实例——覆盖率门禁
// 用 `bc -l` 做数值比较，而 Windows/Git Bash 常无 bc：命令替换得到空串后 `(( ))` 变成
// 无操作数表达式（求值为 0 ⇒ false）⇒ `if` 不成立 ⇒ 直接打印 PASS，**任何覆盖率都能过**。
// 这类故障没人会报错，只能把口径钉进测试。
//
// 本文件只做**结构性断言**（不实际执行配方）：配方的真行为需要 awk/sh，而 Windows CI job
// （test-windows）的 PATH 上不保证有 awk；静态断言已足以拦住「重新引入 bc」「删掉空值守卫」
// 这类退化。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isASCII 判断 s 是否全部为 ASCII（Makefile 配方消息的 Windows 控制台兼容口径）。
func isASCII(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

// makefileTargetRecipe 返回 Makefile 中 target 的配方（紧跟 target 行、以 Tab 开头的连续行）。
func makefileTargetRecipe(t *testing.T, target string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(moduleRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("读 Makefile: %v", err)
	}
	var recipe []string
	inTarget := false
	for line := range strings.SplitSeq(string(data), "\n") {
		if !inTarget {
			// 只认「行首即 target:」的定义行，避免命中 .PHONY 之类的引用。
			if strings.HasPrefix(line, target+":") {
				inTarget = true
			}
			continue
		}
		if strings.HasPrefix(line, "\t") {
			recipe = append(recipe, line)
			continue
		}
		break // 配方结束（下一个 target 或注释）
	}
	if len(recipe) == 0 {
		t.Fatalf("Makefile 中未找到 target %q 的配方", target)
	}
	return strings.Join(recipe, "\n")
}

// TestCoverageGate_NoBCAndFailsClosed 钉住覆盖率门禁的两条口径：
//  1. **不得依赖 bc**（Windows/Git Bash 常缺 ⇒ 静默 PASS，见文件头注释）；
//  2. **必须 fail-closed**：取不到 total 时直接退出 1，而不是打印 PASS。
func TestCoverageGate_NoBCAndFailsClosed(t *testing.T) {
	t.Parallel()
	recipe := makefileTargetRecipe(t, "cover-check")

	if strings.Contains(recipe, "| bc") || strings.Contains(recipe, "bc -l") {
		t.Errorf("cover-check 不得依赖 bc（缺失时比较表达式变空 ⇒ 恒 PASS）：\n%s", recipe)
	}
	if !strings.Contains(recipe, "awk") {
		t.Errorf("cover-check 应改用 awk 做数值比较（POSIX 且随 git-bash 提供）：\n%s", recipe)
	}
	if !strings.Contains(recipe, "could not compute coverage") || !strings.Contains(recipe, "exit 1") {
		t.Errorf("cover-check 必须保留「取不到覆盖率即失败」的空值守卫：\n%s", recipe)
	}
	if !strings.Contains(recipe, "COVER_THRESHOLD") {
		t.Errorf("cover-check 必须按 COVER_THRESHOLD 阈值判定：\n%s", recipe)
	}
}

// TestDeadcodeGate_WiredIntoCI 钉住可达性死代码门禁存在且**真的被 CI 调用**：
// 只放 Makefile 不挂 CI 等于纸面规则；只挂 CI 而没有豁免清单则必然常红（测试替身恒被报
// 不可达），两者缺一不可。
func TestDeadcodeGate_WiredIntoCI(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	recipe := makefileTargetRecipe(t, "deadcode-check")
	if !strings.Contains(recipe, "deadcode") {
		t.Errorf("deadcode-check 配方未调用 deadcode：\n%s", recipe)
	}
	if !strings.Contains(recipe, ".deadcodeignore") {
		t.Errorf("deadcode-check 必须按 .deadcodeignore 过滤（否则测试替身导致门禁常红）：\n%s", recipe)
	}
	if !strings.Contains(recipe, "exit 1") {
		t.Errorf("deadcode-check 发现未登记符号时必须失败：\n%s", recipe)
	}
	// 消息必须 ASCII：Windows 控制台（CP936）下中文会乱码（实测 make 输出“鍙戠幇...”），
	// 使失败信息不可读。
	for line := range strings.SplitSeq(recipe, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "echo") && !isASCII(line) {
			t.Errorf("deadcode-check 的 echo 必须为 ASCII（Windows 控制台乱码）：%s", line)
		}
	}

	ignore, err := os.ReadFile(filepath.Join(root, ".deadcodeignore"))
	if err != nil {
		t.Fatalf("读 .deadcodeignore: %v", err)
	}
	if len(strings.TrimSpace(string(ignore))) == 0 {
		t.Error(".deadcodeignore 不得为空（空清单说明没有可解释的豁免，应直接删掉本门禁）")
	}
	if strings.Contains(string(ignore), "#") {
		t.Errorf(".deadcodeignore 是 grep -f 的模式清单，注释行会被当成模式，需把理由写在 Makefile：\n%s", string(ignore))
	}

	ci, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatalf("读 ci.yml: %v", err)
	}
	if !strings.Contains(string(ci), "make deadcode-check") {
		t.Error("CI 未调用 make deadcode-check（门禁未接线 ⇒ 永不生效）")
	}
	if !strings.Contains(string(ci), "make cover-check") {
		t.Error("CI 未调用 make cover-check（覆盖率门禁未接线）")
	}
}
