// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Command benchgate 是基准回归门禁：把**本次运行**的 benchmark 结果与入库基线
// （benchmarks/baseline/*.txt）做 benchstat 对比，退化超过阈值（默认 ±15%）即
// 以非零退出码失败。
//
// 为什么需要进程内工具而非直接跑 `benchstat` CLI：
//   - benchstat 的显著性判定需要「新老样本各有足够多的重复测量」才能给 Δ%，样本
//     不足时输出 `~`（噪声）——CI 与基线各 `-count=5` 时多数 benchmark 可判，但仍有
//     一部分（尤其低抖动/零分配）会落在「all equal / 样本不足」，CLI 没有「这类行
//     视为通过还是失败」的可编程语义；
//   - 我们需要的是**可脚本化的回归判据**：逐行解析 benchstat 表格的行 Δ%（含 ~ 噪声
//     行的处理规则），把「退化超阈值」变成 exit 1。这只能靠库调用（golang.org/x/perf/
//     benchstat 的 Collection/Table/Row）实现，shell 解析文本表无法可靠地做到。
//
// 判定口径（与 benchstat 语义一致）：
//   - 只比较「同一个 benchmark 名」在基线与当前两次运行间的均值；
//   - 显著性：p < alpha（默认 0.05）且 Δ% 超阈值才判回归；噪声（~）不判；
//   - 时间类指标（ns/op 等）「更大 = 更差」；速度类指标（MB/s 等）「更小 = 更差」；
//   - 改进（更快/更省）不判失败。
//
// 用法：
//
//	make bench-baseline    # 生成本机基线（benchmarks/baseline/linux.txt 等）
//	make bench-gate        # 跑基准并对比基线，退化 > 阈值 → exit 1
//	benchgate -threshold 0.05 -baseline <file> <current.txt>
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"golang.org/x/perf/benchstat" //nolint:staticcheck // 旧版库虽标 deprecated，但其 Collection/Table/Row 模型正适合本工具的逐行回归判定；新版 cmd/benchstat 是 CLI 且内聚在 internal 包，无法库调用
)

// 基准回归门禁：退化超过阈值即失败。
// 用法：benchgate [-threshold 0.15] <baseline.txt> <current.txt>
func main() {
	threshold := flag.Float64("threshold", 0.15, "回归判定阈值（退化超过该比例判失败，如 0.15 = 15%%）")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "用法: benchgate [-threshold 0.15] <baseline.txt> <current.txt>")
		os.Exit(2)
	}
	regressions, output, err := compareBenchFiles(flag.Arg(0), flag.Arg(1), *threshold)
	if err != nil {
		fmt.Fprintf(os.Stderr, "benchgate: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(output)
	if regressions > 0 {
		fmt.Fprintf(os.Stderr, "\n✗ 基准回归：%d 项退化超过 %.0f%%（判定口径见 tools/benchgate/main.go）\n",
			regressions, *threshold*100)
		os.Exit(1)
	}
}

// regressionResult 是一条已判定的回归记录。
type regressionResult struct {
	Benchmark string // benchmark 名（无 Benchmark 前缀）
	OldMean   float64
	NewMean   float64
	PctDelta  float64 // 百分比（正 = 更慢/更多；对速度指标负 = 更慢）
	Metric    string  // 指标单位（ns/op、MB/s、B/op、allocs/op）
}

// compareBenchFiles 用 benchstat 库对比基线与当前两次运行，返回回归数、人读输出与错误。
// 判定口径：
//   - 只统计「显著退化」（p < 0.05 且 Δ 超阈值）；
//   - 时间/内存类（ns/op、B/op、allocs/op）退化 = 新均值 > 旧均值；
//   - 速度类（MB/s）退化 = 新均值 < 旧均值；
//   - 改进不判失败；benchstat 判为噪声（~）的行不判失败。
func compareBenchFiles(baselinePath, currentPath string, threshold float64) (int, string, error) {
	for _, p := range []string{baselinePath, currentPath} {
		if _, err := os.Stat(p); err != nil {
			return 0, "", fmt.Errorf("基准文件不可读 %s: %w", p, err)
		}
	}
	c := &benchstat.Collection{
		Alpha:      0.05,
		AddGeoMean: false,
	}
	for _, entry := range []struct{ name, path string }{{"baseline", baselinePath}, {"current", currentPath}} {
		f, err := os.Open(entry.path)
		if err != nil {
			return 0, "", fmt.Errorf("打开基准文件 %s: %w", entry.path, err)
		}
		err = c.AddFile(entry.name, f)
		_ = f.Close()
		if err != nil {
			return 0, "", fmt.Errorf("解析基准文件 %s: %w", entry.path, err)
		}
	}

	var regressions []regressionResult
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== 基准回归对比（阈值 %.0f%%）===\n", threshold*100)
	fmt.Fprintf(&sb, "基线: %s\n当前: %s\n\n", baselinePath, currentPath)

	for _, table := range c.Tables() {
		for _, row := range table.Rows {
			if len(row.Metrics) != 2 {
				continue
			}
			old, cur := row.Metrics[0], row.Metrics[1]
			if old.Mean == 0 || cur.Mean == 0 {
				continue
			}
			// 只对「能判定显著差异」的行做回归判定：benchstat 把无法判别的
			// 行标为 "~"（噪声），此时 PctDelta 未计算（0），不判失败。
			if row.Delta == "~" || row.Delta == "" {
				continue
			}
			pct := ((cur.Mean / old.Mean) - 1.0) * 100.0
			// speed 指标（MB/s）小 = 差；其它指标（ns/op、B/op…）大 = 差。
			isSpeed := strings.Contains(table.Metric, "speed")
			isRegress := false
			if isSpeed {
				isRegress = -pct > threshold*100 // 速度掉了（Δ 为负）
			} else {
				isRegress = pct > threshold*100
			}
			fmt.Fprintf(&sb, "%-55s %10s  Δ %+6.2f%%  (%.4g → %.4g)\n",
				row.Benchmark, table.Metric, pct, old.Mean, cur.Mean)
			if isRegress {
				regressions = append(regressions, regressionResult{
					Benchmark: row.Benchmark,
					OldMean:   old.Mean,
					NewMean:   cur.Mean,
					PctDelta:  pct,
					Metric:    table.Metric,
				})
			}
		}
	}
	if len(regressions) == 0 {
		fmt.Fprintln(&sb, "无显著回归 ✓")
		return 0, sb.String(), nil
	}
	fmt.Fprintln(&sb, "\nREGRESSION:")
	for _, r := range regressions {
		fmt.Fprintf(&sb, "  ✗ %s %s Δ %+.2f%%（%.4g → %.4g）\n",
			r.Benchmark, r.Metric, r.PctDelta, r.OldMean, r.NewMean)
	}
	return len(regressions), sb.String(), nil
}

// mustOpen 打开文件，失败即 panic（文件存在性已在 compareBenchFiles 前置检查）。
// 已不再使用，保留注释说明历史（基准文件均显式 Open+Close，避免句柄泄漏阻塞 TempDir 清理）。
