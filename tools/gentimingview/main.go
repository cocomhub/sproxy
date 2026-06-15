// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// gentimingview generates a test timing trend visualization HTML page.
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

type timingRecord struct {
	Branch  string
	Commit  string
	Date    string
	Results []timingResult
	TotalS  float64
}

type timingResult struct {
	Package string
	TimeS   float64
}

type timingEntry struct {
	Date   string
	TimeS  float64
	Commit string
}

type timingGroup struct {
	Name    string
	Entries []timingEntry
}

func parseTimingFile(path string) (*timingRecord, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	rec := &timingRecord{}
	scanner := bufio.NewScanner(f)
	inData := false
	// Track total line for multi-line timing output (e.g., test-packages format)
	// "ok   github.com/xxx  10.234s"
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
		case inData:
			// Parse lines like: "ok   github.com/xxx  10.234s"
			// Also: "ok   github.com/xxx  10.234s  coverage: 53.5% of statements"
			parts := strings.Fields(line)
			if len(parts) >= 3 && parts[0] == "ok" {
				last := parts[len(parts)-1]
				if strings.HasSuffix(last, "s") && !strings.Contains(last, "%") {
					ts := 0.0
					fmt.Sscanf(last, "%fs", &ts)
					if ts > 0 {
						rec.Results = append(rec.Results, timingResult{
							Package: parts[1],
							TimeS:   ts,
						})
					}
				}
			}
		}
	}
	return rec, scanner.Err()
}

func main() {
	dataDir := flag.String("data", "build/timing/data", "timing data directory")
	outDir := flag.String("out", "build/timing/web", "output directory")
	flag.Parse()

	entries, err := os.ReadDir(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "读取数据目录失败: %v\n", err)
		os.Exit(1)
	}

	var records []*timingRecord
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".txt") {
			continue
		}
		rec, err := parseTimingFile(filepath.Join(*dataDir, entry.Name()))
		if err != nil {
			fmt.Fprintf(os.Stderr, "解析 %s 失败: %v\n", entry.Name(), err)
			continue
		}
		records = append(records, rec)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Date < records[j].Date
	})

	// Compute total time per record (sum of package timings)
	var totalEntries []timingEntry
	for _, rec := range records {
		totalS := rec.TotalS
		if totalS == 0 {
			for _, r := range rec.Results {
				totalS += r.TimeS
			}
		}
		totalEntries = append(totalEntries, timingEntry{
			Date: rec.Date, TimeS: totalS, Commit: rec.Commit,
		})
	}

	// Per-package trends
	pkgMap := make(map[string][]timingEntry)
	for _, rec := range records {
		for _, r := range rec.Results {
			pkgMap[r.Package] = append(pkgMap[r.Package], timingEntry{
				Date: rec.Date, TimeS: r.TimeS, Commit: rec.Commit,
			})
		}
	}

	var groups []timingGroup
	for name, ents := range pkgMap {
		sort.Slice(ents, func(i, j int) bool {
			return ents[i].Date < ents[j].Date
		})
		groups = append(groups, timingGroup{Name: name, Entries: ents})
	}
	sort.Slice(groups, func(i, j int) bool {
		return groups[i].Name < groups[j].Name
	})

	totalGroup := timingGroup{Name: "total", Entries: totalEntries}
	allGroups := append([]timingGroup{totalGroup}, groups...)

	tmpl := template.Must(template.New("page").Parse(`<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>测试耗时趋势 - Sproxy</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4" integrity="sha384-8v4AxqxYPnBGFqOAyq/ExqNd8aOZnLH1LmP3t3RLPGE7W8v7t3pNsBiJR2/aaC8" crossorigin="anonymous"></script>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 20px; background: #f5f5f5; }
  h1 { color: #333; }
  .chart-container { background: white; border-radius: 8px; padding: 20px; margin-bottom: 20px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
  canvas { max-height: 350px; }
  .total { border-left: 4px solid #FF5722; }
</style>
</head>
<body>
<h1>测试耗时趋势</h1>
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
  const timeData = group.Entries.map(e => e.TimeS);
  const ctx = document.getElementById('chart-' + group.Name).getContext('2d');
  new Chart(ctx, {
    type: 'line',
    data: {
      labels: labels,
      datasets: [{
        label: 'seconds',
        data: timeData,
        borderColor: group.Name === 'total' ? '#FF5722' : '#9C27B0',
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
      scales: { y: { beginAtZero: true, title: { display: true, text: 'seconds' } } }
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
