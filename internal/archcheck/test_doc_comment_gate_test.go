// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// test_doc_comment_gate_test.go 是「测试文档注释必须有对应函数」的门禁（R49）。
//
// 背景（2026-10-30 实测事故）：PR #927 搬运 `scheduler_test.go` 时，
// `TestConfigValidate_MaintenanceWindow` 的函数体被整体删掉、只留下文档注释，导致该用例
// 静默消失（测试计数 -1 未被任何门禁发现）。本门禁把「注释存在但函数不存在」变成红灯。
//
// 口径：`*_test.go` 中形如 `// TestXxx ...` / `// BenchmarkXxx ...` / `// FuzzXxx ...` 的
// 行，其后的首个非空非注释行必须是 `func <同名>(`。多行文档注释允许。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var testDocRe = regexp.MustCompile(`^// ((?:Test|Benchmark|Fuzz)[A-Z0-9_][A-Za-z0-9_]*)`)

// collectTestFuncNames 收集全仓测试函数名（判据：注释指向的测试是否真的存在）。
func collectTestFuncNames(t *testing.T, root string) map[string]bool {
	t.Helper()
	names := map[string]bool{}
	defRe := regexp.MustCompile(`(?m)^func (?:\*\([^)]*\) )?((?:Test|Benchmark|Fuzz)[A-Z0-9_]\w*)\(`)
	for _, dir := range []string{"pkg", "cmd", "internal", "tools", "test"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			for _, m := range defRe.FindAllStringSubmatch(string(data), -1) {
				names[m[1]] = true
			}
			return nil
		})
	}
	return names
}

// TestDanglingTestDocComment 门禁：测试文档注释不得悬空（函数被删但注释残留）。
func TestDanglingTestDocComment(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	defined := collectTestFuncNames(t, root)
	var offenders []string
	for _, dir := range []string{filepath.Join("pkg", "server")} {
		base := filepath.Join(root, dir)
		_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil
			}
			lines := strings.Split(string(data), "\n")
			for i, ln := range lines {
				m := testDocRe.FindStringSubmatch(ln)
				if m == nil {
					continue
				}
				name := m[1]
				j := i + 1
				for j < len(lines) {
					s := strings.TrimSpace(lines[j])
					if s == "" || strings.HasPrefix(s, "//") {
						j++
						continue
					}
					break
				}
				followedBySelf := j < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[j]), "func "+name+"(")
				if !followedBySelf && !defined[name] {
					rel, _ := filepath.Rel(root, path)
					offenders = append(offenders, filepath.ToSlash(rel)+": 注释 "+name+" 指向不存在的测试函数")
				}
			}
			return nil
		})
	}
	if len(offenders) > 0 {
		t.Fatalf("测试文档注释悬空（函数体疑似被删）：\n%s", strings.Join(offenders, "\n"))
	}
}
