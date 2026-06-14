// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// genbenchview generates a benchmark trend visualization HTML page.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type benchRecord struct {
	Branch  string
	Commit  string
	Date    string
	Results []benchResult
}

type benchResult struct {
	Name   string
	NsOp   string
	Allocs string
	MBs    string
}

func parseBenchFile(path string) (*benchRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rec := &benchRecord{Date: time.Now().Format(time.RFC3339)}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "branch: "):
			rec.Branch = strings.TrimPrefix(line, "branch: ")
		case strings.HasPrefix(line, "commit: "):
			rec.Commit = strings.TrimPrefix(line, "commit: ")
		case strings.HasPrefix(line, "date: "):
			rec.Date = strings.TrimPrefix(line, "date: ")
		default:
			if strings.HasPrefix(line, "Benchmark") {
				fields := strings.Fields(line)
				if len(fields) >= 3 {
					r := benchResult{Name: fields[0]}
					for i, f := range fields {
						if strings.HasSuffix(f, "ns/op") && i > 0 {
							r.NsOp = fields[i-1]
						}
						if strings.HasSuffix(f, "B/op") && i > 0 {
							r.Allocs = fields[i-1]
						}
						if strings.HasSuffix(f, "MB/s") && i > 0 {
							r.MBs = fields[i-1]
						}
					}
					rec.Results = append(rec.Results, r)
				}
			}
		}
	}
	return rec, scanner.Err()
}

type benchNameGroup struct {
	Name    string
	Entries []benchEntry
}

type benchEntry struct {
	Date   string
	NsOp   string
	MBs    string
	Commit string
}

func main() {
	dataDir := flag.String("data", "build/benchmark/data", "benchmark data directory")
	outDir := flag.String("out", "build/benchmark/web", "output directory")
	flag.Parse()

	entries, err := os.ReadDir(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取数据目录失败: %v\n", err)
		os.Exit(1)
	}

	var records []*benchRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		rec, err := parseBenchFile(filepath.Join(*dataDir, entry.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "解析 %s 失败: %v\n", entry.Name(), err)
			continue
		}
		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Date < records[j].Date
	})

	nameSet := make(map[string][]benchEntry)
	nameOrder := make([]string, 0)
	for _, rec := range records {
		for _, r := range rec.Results {
			name := r.Name
			re := regexp.MustCompile(`-\d+$`)
			name = re.ReplaceAllString(name, "")
			entry := benchEntry{
				Date:   rec.Date,
				NsOp:   r.NsOp,
				MBs:    r.MBs,
				Commit: rec.Commit,
			}
			if _, ok := nameSet[name]; !ok {
				nameOrder = append(nameOrder, name)
			}
			nameSet[name] = append(nameSet[name], entry)
		}
	}

	var groups []benchNameGroup
	for _, name := range nameOrder {
		groups = append(groups, benchNameGroup{Name: name, Entries: nameSet[name]})
	}

	tmpl := template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Benchmark 趋势图 - Sproxy</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4" integrity="sha384-8v4AxqxYPnBGFqOAyq/ExqNd8aOZnLH1LmP3t3RLPGE7W8v7t3pNsBiJR2/aaC8" crossorigin="anonymous"></script>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 20px; background: #f5f5f5; }
  h1 { color: #333; }
  .chart-container { background: white; border-radius: 8px; padding: 20px; margin-bottom: 30px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
  canvas { max-height: 400px; }
</style>
</head>
<body>
<h1>📊 Benchmark 趋势</h1>
<p>共 {{len .}} 个 benchmark 指标</p>
{{range .}}
<div class="chart-container">
  <h3>{{.Name}}</h3>
  <canvas id="chart-{{.Name}}"></canvas>
</div>
{{end}}
<script>
const chartData = {{.}};
chartData.forEach((group, idx) => {
  const labels = group.Entries.map(e => e.Date.substring(0, 16));
  const nsOpData = group.Entries.map(e => parseFloat(e.NsOp) || null);
  const ctx = document.getElementById('chart-' + group.Name).getContext('2d');
  new Chart(ctx, {
    type: 'line',
    data: {
      labels: labels,
      datasets: [{
        label: 'ns/op',
        data: nsOpData,
        borderColor: '#4CAF50',
        backgroundColor: 'rgba(76, 175, 80, 0.1)',
        tension: 0.3,
        spanGaps: true
      }]
    },
    options: {
      responsive: true,
      plugins: { legend: { display: false }, tooltip: { callbacks: { afterLabel: function(ctx) { const e = group.Entries[ctx.dataIndex]; return 'commit: ' + e.Commit; } } } },
      scales: { y: { beginAtZero: false, title: { display: true, text: 'ns/op' } } }
    }
  });
});
</script>
</body>
</html>`))

	outPath := filepath.Join(*outDir, "index.html")
	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "创建输出文件失败: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	if err := tmpl.Execute(f, groups); err != nil {
		fmt.Fprintf(os.Stderr, "模板渲染失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成: %s\n", outPath)
}
