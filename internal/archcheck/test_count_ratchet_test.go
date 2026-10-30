// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// test_count_ratchet_test.go 是「测试函数总数只增不减」的棘轮（R50）。
//
// 背景（2026-10-30 实测事故）：PR #927 搬运 `scheduler_test.go` 时，一个测试的函数体被
// 整体删除、只剩注释，全仓测试数 −1，无任何门禁发现。R49 只能发现「注释悬空」，若删除时
// 连注释一起删则无法察觉；本棘轮从**总数**维度兜底：任何 PR 让测试函数总数下降即红。
//
// 口径：`*_test.go` 中 `func TestXxx(` 定义总数（不含 Benchmark/Fuzz）。
// 有意删除测试时：同步下调 `test_count_baseline.tsv` 并在 PR 说明理由。

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var testFuncCountRe = regexp.MustCompile(`(?m)^func Test[A-Za-z0-9_]*\(`)

// TestCountRatchet 门禁：测试函数总数不得低于基线。
func TestCountRatchet(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	current := 0
	for _, dir := range []string{"pkg", "cmd", "internal", "tools", "test"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			current += len(testFuncCountRe.FindAll(data, -1))
			return nil
		})
	}
	baseline := readTestCountBaseline(t, filepath.Join(root, "internal", "archcheck", "test_count_baseline.tsv"))
	if current < baseline {
		t.Fatalf("测试函数总数下降：当前 %d < 基线 %d（疑似搬运/重构时丢失测试；确属有意删除请同步下调 test_count_baseline.tsv 并说明理由）", current, baseline)
	}
}

// readTestCountBaseline 读取 total 行（缺文件 = 失败，避免门禁空转）。
func readTestCountBaseline(t *testing.T, path string) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("读取基线失败: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) == 2 && fields[0] == "total" {
			n, perr := strconv.Atoi(fields[1])
			if perr != nil {
				t.Fatalf("基线格式错误: %v", perr)
			}
			return n
		}
	}
	t.Fatal("基线缺少 total 行")
	return 0
}
