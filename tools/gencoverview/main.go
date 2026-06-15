// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// gencoverview generates a coverage trend visualization HTML page.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html/template"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type coverRecord struct {
	Branch   string
	Commit   string
	Date     string
	Packages []pkgCover
	Total    string
}

type pkgCover struct {
	Name string
	Pct  string
}

type coverEntry struct {
	Date   string
	Pct    float64
	Commit string
}

type coverNameGroup struct {
	Name    string
	Entries []coverEntry
}

func parseCoverFile(path string) (*coverRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rec := &coverRecord{}
	scanner := bufio.NewScanner(f)
	inData := false
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "branch: "):
			rec.Branch = strings.TrimPrefix(line, "branch: ")
		case strings.HasPrefix(line, "commit: "):
			rec.Commit = strings.TrimPrefix(line, "commit: ")
		case strings.HasPrefix(line, "date: "):
			rec.Date = strings.TrimPrefix(line, "date: ")
		case line == "---":
			inData = true
		case inData && strings.HasPrefix(line, "total:"):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				rec.Total = fields[1]
			}
		case inData && strings.Contains(line, "% of statements"):
			// Format: "ok   github.com/pkg/path\t53.5% of statements"
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				pkg := pkgCover{
					Name: strings.TrimSpace(parts[0]),
					Pct:  strings.TrimSpace(parts[1]),
				}
				rec.Packages = append(rec.Packages, pkg)
			}
		}
	}
	return rec, scanner.Err()
}

func main() {
	dataDir := flag.String("data", "build/coverage/data", "coverage data directory")
	outDir := flag.String("out", "build/coverage/web", "output directory")
	flag.Parse()

	entries, err := os.ReadDir(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取数据目录失败: %v\n", err)
		os.Exit(1)
	}

	var records []*coverRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		rec, err := parseCoverFile(filepath.Join(*dataDir, entry.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "解析 %s 失败: %v\n", entry.Name(), err)
			continue
		}
		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Date < records[j].Date
	})

	// Build total trend
	var totalEntries []coverEntry
	for _, rec := range records {
		if rec.Total == "" {
			continue
		}
		pct := 0.0
		fmt.Sscanf(rec.Total, "%f%%", &pct)
		totalEntries = append(totalEntries, coverEntry{
			Date: rec.Date, Pct: pct, Commit: rec.Commit,
		})
	}

	// Build per-package trends
	pkgMap := make(map[string][]coverEntry)
	for _, rec := range records {
		for _, p := range rec.Packages {
			pct := 0.0
			fmt.Sscanf(p.Pct, "%f%%", &pct)
			pkgMap[p.Name] = append(pkgMap[p.Name], coverEntry{
				Date: rec.Date, Pct: pct, Commit: rec.Commit,
			})
		}
	}

	// Sort package entries by date
	var pkgGroups []coverNameGroup
	for name, entries2 := range pkgMap {
		sort.Slice(entries2, func(i, j int) bool {
			return entries2[i].Date < entries2[j].Date
		})
		pkgGroups = append(pkgGroups, coverNameGroup{Name: name, Entries: entries2})
	}
	sort.Slice(pkgGroups, func(i, j int) bool {
		return pkgGroups[i].Name < pkgGroups[j].Name
	})

	// Also add total as first group
	totalGroup := coverNameGroup{Name: "total", Entries: totalEntries}
	allGroups := append([]coverNameGroup{totalGroup}, pkgGroups...)

	tmpl := template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>覆盖率趋势 - Sproxy</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4" integrity="sha384-8v4AxqxYPnBGFqOAyq/ExqNd8aOZnLH1LmP3t3RLPGE7W8v7t3pNsBiJR2/aaC8" crossorigin="anonymous"></script>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 20px; background: #f5f5f5; }
  h1 { color: #333; }
  .chart-container { background: white; border-radius: 8px; padding: 20px; margin-bottom: 20px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
  canvas { max-height: 350px; }
  .total { border-left: 4px solid #4CAF50; }
</style>
</head>
<body>
<h1>覆盖率趋势</h1>
<p>{{len .}} 个指标</p>
{{range .}}
<div class="chart-container{{if eq .Name "total"}} total{{end}}">
  <h3>{{.Name}}</h3>
  <canvas id="chart-{{.Name}}"></canvas>
</div>
{{end}}
<script>
const chartData = {{.}};
chartData.forEach((group) => {
  const labels = group.Entries.map(e => e.Date.substring(0, 16));
  const pctData = group.Entries.map(e => e.Pct);
  const ctx = document.getElementById('chart-' + group.Name).getContext('2d');
  new Chart(ctx, {
    type: 'line',
    data: {
      labels: labels,
      datasets: [{
        label: 'coverage %',
        data: pctData,
        borderColor: group.Name === 'total' ? '#4CAF50' : '#2196F3',
        backgroundColor: 'rgba(33, 150, 243, 0.1)',
        tension: 0.3,
        spanGaps: true
      }]
    },
    options: {
      responsive: true,
      plugins: {
        legend: { display: false },
        tooltip: { callbacks: { afterLabel: function(ctx) { const e = group.Entries[ctx.dataIndex]; return 'commit: ' + e.Commit; } } }
      },
      scales: { y: { min: 0, max: 100, title: { display: true, text: '%' } } }
    }
  });
});
</script>
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
	defer f.Close()

	if err := tmpl.Execute(f, allGroups); err != nil {
		fmt.Fprintf(os.Stderr, "模板渲染失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成: %s\n", outPath)
}
