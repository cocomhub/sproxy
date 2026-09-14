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
// 白名单形态为数据文件 internal/archcheck/serial_budgets.tsv（file 实测串行数），
// 由 helper（TestSerialRatchetHelper，ARCHCHECK_WRITE_BASELINE=1 触发）重生成，
// 避免手抄 200+ 行。棘轮判据：串行 Test 总数与逐文件计数**只减不增**。
// 增加/放宽任何预算行**必须**同时在 docs/testing/virtual-time-conversions.md
// 登记不可并发的就地理由。

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

// scanSerialTests 返回每个测试文件的顶层串行 Test 数（不并行且非 setenv 强制串）。
func scanSerialTests(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	_ = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		base := filepath.Base(path)
		if base == "node_modules" || base == "build" || base == "vendor" || base == "dist" {
			return fs.SkipDir
		}
		if strings.HasPrefix(base, ".") && base != "." {
			return fs.SkipDir
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := strings.ReplaceAll(filepath.ToSlash(path), "\\", "/")
		if rel == "internal/archcheck/test_parallel_gate_test.go" {
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
