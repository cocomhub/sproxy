// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// perf.go 是**隐藏**的性能排查命令族（`sclient perf ...`）。
//
// 为什么做成命令而不是散落的临时脚本（用户要求 2026-11-12：「设计隐藏子命令保留
// perf/metric 功能便于以后排查」）：
//   - 2026-11-12 排查「加密卷 OOM」时，抓服务端 pprof 要先手写一个 Go 程序（走 FileClient
//     的签名能力）才能过 `/debug/pprof` 的认证，脚本散落在 build/ 下容易丢失；
//   - 服务端内存问题的第一现场信息（heap/goroutine profile、heap 指标）应当**一条命令
//     拿到**，且与既有客户端配置（server_url/凭据/隧道）自动复用。
//
// 设计约束：
//   - **Hidden: true** —— 属运维/排查面，不进普通用户的帮助列表，也不占 CLI 兼容承诺；
//   - 复用 `clientfactory.Factory.NewClient`（签名/隧道/自签 TLS 全由既有链路处理），
//     **不新造请求实现**；
//   - pprof 端点受认证保护（服务端 `debug_pprof_enabled`，默认 false）：未开启时服务端 404，
//     命令原样透出状态码并**提示检查该开关**，便于判断「是没开还是别的错」。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// perfProfiles 是允许抓取的 pprof profile 白名单（避免任意路径注入）。
var perfProfiles = map[string]bool{
	"heap":         true, // 内存（inuse_space/alloc_space）：排查内存放大首选
	"goroutine":    true, // 协程栈：排查泄漏/阻塞
	"allocs":       true, // 累计分配：找出「谁在反复分配」
	"block":        true, // 阻塞剖析
	"mutex":        true, // 锁竞争
	"threadcreate": true,
	"cpu":          true, // CPU（pprof 会附带 seconds 参数）
}

// perfProfileNames 供错误提示用（map 迭代无序，分开维护一份稳定清单）。
const perfProfileNames = "heap/goroutine/allocs/block/mutex/threadcreate/cpu"

// heapMetricKeys 是 /metrics 里与内存排查直接相关的指标（默认高亮，其余原样透出）。
var heapMetricKeys = []string{
	"sproxy_heap_alloc_bytes", // 当前堆分配（Go 视角）
	"sproxy_heap_objects",     // 当前堆对象数
	"sproxy_gc_cycles",        // GC 次数
}

// NewCmdPerf 创建隐藏的 perf 命令族。
func NewCmdPerf(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "perf",
		Short:         "内部性能排查（隐藏命令）",
		// Hidden：本命令属运维/排查面，**不进** 普通用户的 `sclient --help` 列表，
		// 也不占 CLI 兼容承诺（用户要求 2026-11-12：设计隐藏子命令保留 perf/metric 能力）。
		Hidden: true,
		Long: `服务端性能排查工具（隐藏命令，不面向普通用户）。

	依赖服务端已开启 debug_pprof_enabled（默认关闭）；未开启时返回 404。

	使用示例:
	  sclient perf profile heap --out heap.prof    # 抓内存 profile（inuse_space 首选）
	  sclient perf profile goroutine               # 抓协程栈到 stdout
	  sclient perf metrics                         # 抓 /metrics 并高亮 heap 指标`,
	}
	cmd.AddCommand(newCmdPerfProfile(factory, ios))
	cmd.AddCommand(newCmdPerfMetrics(factory, ios))
	return cmd
}

func newCmdPerfProfile(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "profile <heap|goroutine|allocs|block|mutex|cpu>",
		Short:         "抓取服务端 pprof profile",
		Long: `抓取服务端 /debug/pprof/<name>。

	分析（在服务端同版本的 Go 环境下）:
	  go tool pprof -top -inuse_space heap.prof             # 常驻内存 top（排查放大）
	  go tool pprof -top -alloc_space heap.prof             # 累计分配 top
	  go tool pprof -peek '<符号>' -inuse_space heap.prof   # 看调用链`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPerfProfile(cmd, factory, ios, out, args[0])
		},
	}
	// 注意：root 已占用 `-o` shorthand，此处只用长名，避免 flagset 重定义 panic（实测）。
	cmd.Flags().StringVar(&out, "out", "", "输出文件（默认写 stdout；二进制 profile 建议重定向到文件）")
	return cmd
}

func newCmdPerfMetrics(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		SilenceUsage:  true,
		SilenceErrors: true,
		Use:           "metrics",
		Short:         "抓取服务端 /metrics（默认高亮 heap 指标）",
		Long: `抓取服务端 /metrics。

	默认只打印与内存排查相关的指标（sproxy_heap_alloc_bytes / sproxy_heap_objects /
	sproxy_gc_cycles）；--all 打印完整指标。`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPerfMetrics(cmd, factory, ios, all)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "打印完整 /metrics（默认只高亮内存相关）")
	return cmd
}

// runPerfProfile 执行一次 profile 抓取（从 RunE 抽出，降认知复杂度）。
func runPerfProfile(cmd *cobra.Command, factory clientfactory.Factory, ios cli.IOStreams, out, rawName string) error {
	name := strings.ToLower(strings.TrimSpace(rawName))
	if !perfProfiles[name] {
		return fmt.Errorf("不支持的 profile %q（可选：%s）", name, perfProfileNames)
	}
	fc, err := factory.NewClient(cmd)
	if err != nil {
		return err
	}
	resp, derr := fc.RequestRaw(cmd.Context(), http.MethodGet, "/debug/pprof/"+name, nil, nil)
	if derr != nil {
		return derr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %d: %s（服务端是否已开启 debug_pprof_enabled？）",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return writePerfProfile(ios, resp.Body, out)
}

// writePerfProfile 把 profile 字节写到 out 指定的文件（空 = stdout），并给出分析提示。
func writePerfProfile(ios cli.IOStreams, body io.Reader, out string) error {
	w := ios.Out
	if out != "" {
		f, cerr := os.Create(out)
		if cerr != nil {
			return fmt.Errorf("创建输出文件: %w", cerr)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	n, cerr := io.Copy(w, body)
	if cerr != nil {
		return fmt.Errorf("写入 profile: %w", cerr)
	}
	if out != "" {
		fmt.Fprintf(ios.Out, "已写入 %s（%d 字节）\n", out, n)
		fmt.Fprintf(ios.Out, "分析: go tool pprof -top -inuse_space %s\n", out)
	}
	return nil
}

// runPerfMetrics 抓 /metrics 并按 all 决定是否只高亮内存指标（从 RunE 抽出）。
func runPerfMetrics(cmd *cobra.Command, factory clientfactory.Factory, ios cli.IOStreams, all bool) error {
	fc, err := factory.NewClient(cmd)
	if err != nil {
		return err
	}
	resp, derr := fc.RequestRaw(cmd.Context(), http.MethodGet, "/metrics", nil, nil)
	if derr != nil {
		return derr
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, cerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if cerr != nil {
		return fmt.Errorf("读取 metrics: %w", cerr)
	}
	if all {
		fmt.Fprint(ios.Out, string(body))
		return nil
	}
	fmt.Fprintln(ios.Out, "# 内存相关（--all 可看全部指标）")
	if !printHeapMetricLines(ios.Out, string(body)) {
		fmt.Fprintln(ios.Out, "# （未找到 heap 指标 —— 服务端版本可能较旧）")
	}
	return nil
}

// printHeapMetricLines 打印与内存排查相关的指标行，返回是否打印了至少一行。
func printHeapMetricLines(w io.Writer, body string) bool {
	shown := false
	for l := range strings.SplitSeq(body, "\n") {
		if isHeapMetricLine(l) {
			fmt.Fprintln(w, l)
			shown = true
		}
	}
	return shown
}

// isHeapMetricLine 判据：以 heapMetricKeys 之一 + 空格或 `{` 开头（Prometheus 行格式）。
func isHeapMetricLine(l string) bool {
	for _, k := range heapMetricKeys {
		if strings.HasPrefix(l, k+" ") || strings.HasPrefix(l, k+"{") {
			return true
		}
	}
	return false
}
