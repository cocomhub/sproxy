// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// genreport generates a unified report dashboard for benchmark/coverage/timing.
package main

import (
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
)

func main() {
	outDir := flag.String("out", "build/report", "output directory")
	flag.Parse()

	tmpl := template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Sproxy 质量报告</title>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 40px; background: #f5f5f5; color: #333; }
  h1 { font-size: 2em; margin-bottom: 10px; }
  .subtitle { color: #666; margin-bottom: 30px; }
  .cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(300px, 1fr)); gap: 24px; }
  .card { background: white; border-radius: 12px; padding: 24px; box-shadow: 0 2px 8px rgba(0,0,0,0.08); transition: transform 0.2s; }
  .card:hover { transform: translateY(-4px); box-shadow: 0 4px 16px rgba(0,0,0,0.12); }
  .card h2 { margin-top: 0; font-size: 1.3em; }
  .card p { color: #555; line-height: 1.6; }
  .card .icon { font-size: 2em; margin-bottom: 10px; }
  .card a { display: inline-block; margin-top: 12px; padding: 8px 16px; background: #4CAF50; color: white; text-decoration: none; border-radius: 6px; font-weight: 500; }
  .card a:hover { background: #388E3C; }
</style>
</head>
<body>
<h1>Sproxy 质量报告</h1>
<div class="subtitle">本地构建质量仪表盘 · 趋势对比</div>
<div class="cards">
  <div class="card">
    <div class="icon">🔬</div>
    <h2>Benchmark 趋势</h2>
    <p>查看各 benchmark 指标（ns/op、MB/s）的历史趋势折线图。每次 <code>make bench</code> 采集一次，保留最近 10 次结果。</p>
    <a href="../benchmark/web/index.html">查看</a>
  </div>
  <div class="card">
    <div class="icon">📊</div>
    <h2>覆盖率趋势</h2>
    <p>查看总体和每包覆盖率的历史趋势。每次 <code>make cover</code> 采集一次，保留最近 10 次结果。</p>
    <a href="../coverage/web/index.html">查看</a>
  </div>
  <div class="card">
    <div class="icon">⏱</div>
    <h2>测试耗时趋势</h2>
    <p>查看每包测试耗时和总耗时的历史趋势。每次 <code>make test</code> 采集一次，保留最近 10 次结果。</p>
    <a href="../timing/web/index.html">查看</a>
  </div>
  <div class="card">
    <div class="icon">📋</div>
    <h2>Benchstat 对比</h2>
    <p>运行 <code>make bench-compare</code> 对比最近两次 benchmark 结果，查看性能有无回归。</p>
    <a href="#" onclick="event.preventDefault(); alert('在终端运行: make bench-compare');">运行</a>
  </div>
</div>
</body>
</html>`))

	outPath := filepath.Join(*outDir, "index.html")
	if err := os.MkdirAll(*outDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "创建输出目录失败: %v\n", err)
		os.Exit(1)
	}
	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建输出文件失败: %v\n", err)
		os.Exit(1)
	}

	if err := tmpl.Execute(f, nil); err != nil {
		fmt.Fprintf(os.Stderr, "模板渲染失败: %v\n", err)
		f.Close()
		os.Exit(1)
	}
	fmt.Printf("已生成: %s\n", outPath)
}
