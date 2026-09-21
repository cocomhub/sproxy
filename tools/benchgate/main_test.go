// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// benchLines 构造 count 个同一 benchmark 的测量行（ns/op 单位）。
// nsPerOp 为基准值，逐行加 0~3% 的抖动模拟真实测量（完全相同的值会让
// benchstat 的 UTest 报 all equal 而无法判显著，测不到回归判定）。
func benchLines(benchName string, nsPerOp float64, count int) []string {
	lines := make([]string, 0, count)
	for i := range count {
		jitter := 1.0 + float64(i%3)*0.01 // 0%, 1%, 2%
		ns := nsPerOp * jitter
		// go test 输出格式：BenchmarkXxx-N  <iters>  <ns/op>  <B/op>  <allocs/op>
		lines = append(lines, fmt.Sprintf("%s-20        %d        %.2f ns/op       0 B/op       0 allocs/op",
			benchName, 1000000, ns))
	}
	return lines
}

// writeBenchFile 把 benchmark 行写入临时文件并返回路径。
func writeBenchFile(t *testing.T, name string, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	content := "goos: linux\ngoarch: amd64\npkg: testpkg\n" + strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写基准文件 %s: %v", path, err)
	}
	return path
}

// runGate 运行门禁核心（不 fork 子进程），返回 (回归数, 输出文本, 错误)。
func runGate(base, current string, threshold float64) (int, string, error) {
	return compareBenchFiles(base, current, threshold)
}

// TestGate_NoChange_Passes 基线 == 当前（带少量抖动，无系统性差异）→ 0 回归。
func TestGate_NoChange_Passes(t *testing.T) {
	t.Parallel()
	base := writeBenchFile(t, "base.txt", benchLines("BenchmarkEcho", 1000, 5))
	cur := writeBenchFile(t, "current.txt", benchLines("BenchmarkEcho", 1010, 5))
	n, out, err := runGate(base, cur, 0.15)
	if err != nil {
		t.Fatalf("compareBenchFiles: %v", err)
	}
	if n != 0 {
		t.Fatalf("无差异应 0 回归，得 %d；输出:\n%s", n, out)
	}
}

// TestGate_Regress_OverThreshold_Fails 当前比基线慢 >15% → 至少 1 回归。
func TestGate_Regress_OverThreshold_Fails(t *testing.T) {
	t.Parallel()
	base := writeBenchFile(t, "base.txt", benchLines("BenchmarkEcho", 1000, 5))
	cur := writeBenchFile(t, "current.txt", benchLines("BenchmarkEcho", 1300, 5)) // +30%
	n, out, err := runGate(base, cur, 0.15)
	if err != nil {
		t.Fatalf("compareBenchFiles: %v", err)
	}
	if n == 0 {
		t.Fatalf("+30%% 退化应触发回归，输出:\n%s", out)
	}
	if !strings.Contains(out, "REGRESSION") {
		t.Fatalf("输出应含 REGRESSION 标记:\n%s", out)
	}
	if !strings.Contains(out, "BenchmarkEcho") && !strings.Contains(out, "Echo-20") {
		t.Fatalf("输出应点名退化 benchmark（benchstat 去 Benchmark 前缀后为 Echo-20）：\n%s", out)
	}
}

// TestGate_Improve_DoesNotFail 当前更快（-20%）→ 不是回归，0 失败。
func TestGate_Improve_DoesNotFail(t *testing.T) {
	t.Parallel()
	base := writeBenchFile(t, "base.txt", benchLines("BenchmarkEcho", 1000, 5))
	cur := writeBenchFile(t, "current.txt", benchLines("BenchmarkEcho", 800, 5)) // -20%
	n, out, err := runGate(base, cur, 0.15)
	if err != nil {
		t.Fatalf("compareBenchFiles: %v", err)
	}
	if n != 0 {
		t.Fatalf("改进不应判回归，得 %d；输出:\n%s", n, out)
	}
}

// TestGate_Threshold_Configurable 阈值 5% 时 +10% 退化红；阈值 20% 时同数据不红。
func TestGate_Threshold_Configurable(t *testing.T) {
	t.Parallel()
	base := writeBenchFile(t, "base.txt", benchLines("BenchmarkEcho", 1000, 5))
	cur := writeBenchFile(t, "current.txt", benchLines("BenchmarkEcho", 1100, 5)) // +10%

	n, _, err := runGate(base, cur, 0.05)
	if err != nil {
		t.Fatalf("compareBenchFiles(5%%): %v", err)
	}
	if n == 0 {
		t.Fatalf("阈值 5%% 时 +10%% 退化应触发回归")
	}

	n2, _, err := runGate(base, cur, 0.20)
	if err != nil {
		t.Fatalf("compareBenchFiles(20%%): %v", err)
	}
	if n2 != 0 {
		t.Fatalf("阈值 20%% 时 +10%% 退化不应触发回归，得 %d", n2)
	}
}

// TestGate_SpeedMetric_RegressionFails 速度指标（MB/s）下降同样判回归。
// 速度指标是「越大越好」：MB/s 从 100 掉到 70（-30%）应触发回归。
func TestGate_SpeedMetric_RegressionFails(t *testing.T) {
	t.Parallel()
	// 速度行格式：BenchmarkXxx-20  <iters>  <MB/s>
	mk := func(name string, mbs float64, count int) []string {
		lines := make([]string, 0, count)
		for i := range count {
			jitter := 1.0 + float64(i%3)*0.01
			lines = append(lines, fmt.Sprintf("%s-20        %d        %.2f MB/s",
				name, 1000, mbs*jitter))
		}
		return lines
	}
	base := writeBenchFile(t, "base.txt", mk("BenchmarkThroughput", 100, 5))
	cur := writeBenchFile(t, "current.txt", mk("BenchmarkThroughput", 70, 5)) // -30% speed
	n, out, err := runGate(base, cur, 0.15)
	if err != nil {
		t.Fatalf("compareBenchFiles: %v", err)
	}
	if n == 0 {
		t.Fatalf("速度下降 30%% 应触发回归，输出:\n%s", out)
	}
}

// TestGate_MissingFile_Error 基线文件缺失 → 报错（不是静默通过）。
func TestGate_MissingFile_Error(t *testing.T) {
	t.Parallel()
	base := writeBenchFile(t, "base.txt", benchLines("BenchmarkEcho", 1000, 5))
	_, _, err := runGate(base, filepath.Join(t.TempDir(), "missing.txt"), 0.15)
	if err == nil {
		t.Fatal("基线/当前文件缺失应报错，实际静默通过")
	}
}
