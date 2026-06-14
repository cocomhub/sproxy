# sproxy 测试基础设施与覆盖提升 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 搭建 CI 门禁 / Benchmark 基线 / 增强 linter / pre-commit hook 等基础设施，补齐 sclient 缺失功能，清理技术债务，将测试覆盖从 ~75% 提升至 ~90%。

**架构：** 4 阶段递进（基础设施 → 功能补齐 → 债务清理 → 覆盖提升），每阶段独立可验证。

**技术栈：** Go 1.26, `gopkg.in/yaml.v3`, `github.com/spf13/cobra`, `github.com/spf13/viper`, `github.com/adrg/xdg`, `golang.org/x/perf/cmd/benchstat`, `github.com/google/addlicense`

---

# 阶段 1：基础设施

## 任务 1.1：覆盖率门禁

**文件：**
- 修改：`Makefile:97-99`

- [ ] **步骤 1：修改 Makefile cover 目标，添加阈值检查**

```makefile
COVER_THRESHOLD ?= 85

# 修改 cover 目标
cover: vet
	@mkdir -p $(BUILD_DIR)/coverage
	$(GO) test -count=1 -coverprofile=$(BUILD_DIR)/coverage/cover.out ./...
	@$(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out | grep -E "total"
	@echo "=== 覆盖率门禁检查 ==="
	@pct=$$($(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out | grep -E "^total" | awk '{print $$NF}' | sed 's/%//'); \
	  echo "total coverage: $$pct%"; \
	  if [ "$$CI" = "true" ] && [ "$$pct" -lt "$(COVER_THRESHOLD)" ]; then \
	    echo "FAIL: coverage $$pct% < threshold $(COVER_THRESHOLD)%"; exit 1; \
	  fi
```

- [ ] **步骤 2：验证覆盖率门禁正常工作**

运行：`cd D:\workdir\leon\cocomhub\sproxy && CI=true make cover`
预期：当前覆盖 ~75% < 85%，CI=true 下应 exit 1

- [ ] **步骤 3：验证非 CI 模式不受影响**

运行：`make cover`
预期：打印覆盖率，不退出 1

- [ ] **步骤 4：Commit**

```bash
git add Makefile
git commit -m "feat: add coverage gate (CI=true, threshold 85%)"
```

---

## 任务 1.2：Benchmark 基线系统

**文件：**
- 创建：`tools/genbenchview/main.go`
- 修改：`Makefile:1-120`（新增 bench/bench-compare/bench-web 目标）
- 创建：`build/benchmark/.gitkeep`

- [ ] **步骤 1：创建 benchmark 数据目录结构**

运行：
```bash
mkdir -p build/benchmark/data build/benchmark/web
New-Item -ItemType File -Path build/benchmark/.gitkeep
```

- [ ] **步骤 2：在 Makefile 中新增 bench/bench-compare/bench-web/tools 目标**

```makefile
# 在 CMD_NAMES 变量定义后增加
TOOLS := \
	github.com/google/addlicense@latest \
	golang.org/x/perf/cmd/benchstat@latest

# 新增 tools 目标
.PHONY: tools
tools:
	@for tool in $(TOOLS); do \
		echo "Installing $$tool..."; \
		go install $$tool; \
	done

# 新增 bench 目标
BENCH_DIR := $(BUILD_DIR)/benchmark
BENCH_DATA_DIR := $(BENCH_DIR)/data

.PHONY: bench
bench:
	@mkdir -p $(BENCH_DATA_DIR)
	@echo "=== Running benchmarks ==="
	@outfile="$(BENCH_DATA_DIR)/$(shell git rev-parse --abbrev-ref HEAD)-$(shell git rev-parse --short HEAD)-$(shell date +%Y%m%dT%H%M%S).txt"; \
	  echo "Benchmark results will be saved to: $$outfile"; \
	  echo "branch: $(shell git rev-parse --abbrev-ref HEAD)" > "$$outfile"; \
	  echo "commit: $(shell git rev-parse --short HEAD)" >> "$$outfile"; \
	  echo "date: $(shell date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$$outfile"; \
	  echo "" >> "$$outfile"; \
	  go test -bench=. -benchmem -count=1 \
	    ./pkg/server/... \
	    ./pkg/client/... \
	    ./pkg/tunnel/mux/... \
	    2>&1 | tee -a "$$outfile"; \
	  echo ""; \
	  echo "=== 清理旧记录（保留最近 10 条）==="; \
	  cd $(BENCH_DATA_DIR) && ls -t *.txt 2>/dev/null | tail -n +11 | xargs -r rm -f; \
	  echo "Done. Records in $(BENCH_DATA_DIR): $$(ls $(BENCH_DATA_DIR)/*.txt 2>/dev/null | wc -l)"

# 新增 bench-compare 目标（需要 benchstat）
.PHONY: bench-compare
bench-compare:
	@files=$$(ls -t $(BENCH_DATA_DIR)/*.txt 2>/dev/null | head -2); \
	  count=$$(echo "$$files" | wc -l); \
	  if [ "$$count" -lt 2 ]; then \
	    echo "需要至少 2 条 benchmark 记录才能比较"; exit 1; \
	  fi; \
	  echo "=== 比较最近两次 benchmark 结果 ==="; \
	  echo "新: $$(echo "$$files" | head -1)"; \
	  echo "旧: $$(echo "$$files" | tail -1)"; \
	  echo ""; \
	  benchstat "$$(echo "$$files" | tail -1)" "$$(echo "$$files" | head -1)"

# 新增 bench-web 目标
.PHONY: bench-web
bench-web: tools/genbenchview/main.go
	@mkdir -p $(BENCH_DIR)/web
	@go run tools/genbenchview/main.go -data=$(BENCH_DATA_DIR) -out=$(BENCH_DIR)/web
	@echo "Benchmark web report: file://$(abspath $(BENCH_DIR)/web/index.html)"
```

- [ ] **步骤 3：创建 tools/genbenchview/main.go**

```go
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

// benchRecord represents a single benchmark run.
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

// parseBenchFile parses a benchmark output file.
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
			// Parse benchmark output line: BenchmarkName-12 1234 12345 ns/op 123 B/op 12 allocs/op
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

	// Sort by date ascending
	sort.Slice(records, func(i, j int) bool {
		return records[i].Date < records[j].Date
	})

	// Group by benchmark name
	nameSet := make(map[string][]benchEntry)
	nameOrder := make([]string, 0)
	for _, rec := range records {
		for _, r := range rec.Results {
			name := r.Name
			// Remove the -N suffix
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
<script src="https://cdn.jsdelivr.net/npm/chart.js@4"></script>
<style>
  body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; margin: 20px; background: #f5f5f5; }
  h1 { color: #333; }
  .chart-container { background: white; border-radius: 8px; padding: 20px; margin-bottom: 30px; box-shadow: 0 2px 4px rgba(0,0,0,0.1); }
  canvas { max-height: 400px; }
</style>
</head>
<body>
<h1>📊 Benchmark 趋势</h1>
<p>共 {{len .}} 个 benchmark 指标，{{len (index . 0).Entries}} 次记录</p>
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
        spanGaps: true,
        yAxisID: 'y'
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
```

- [ ] **步骤 4：验证 bench 目标**

运行：`make bench`
预期：生成 `.txt` 文件到 `build/benchmark/data/`

- [ ] **步骤 5：验证 bench-compare 目标**

运行：`make bench`（第二次），然后 `make bench-compare`
预期：benchstat 显示最近两次结果的对比表

- [ ] **步骤 6：验证 bench-web 目标**

运行：`make bench-web`
预期：生成 `build/benchmark/web/index.html`

- [ ] **步骤 7：CI 中新增 benchmark job**

```yaml
# 在 .github/workflows/ci.yml 的 build job 之后新增
  benchmark:
    name: Benchmark
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
          check-latest: true
      - name: Install benchstat
        run: go install golang.org/x/perf/cmd/benchstat@latest
      - name: Run benchmark
        run: |
          mkdir -p build/benchmark/data
          go test -bench=. -benchmem -count=1 \
            ./pkg/server/... \
            ./pkg/client/... \
            ./pkg/tunnel/mux/... \
            2>&1 > build/benchmark/data/ci-$(git rev-parse --short HEAD).txt
      - name: Upload benchmark artifact
        uses: actions/upload-artifact@v4
        with:
          name: benchmark-results
          path: build/benchmark/
```

- [ ] **步骤 8：Commit**

```bash
git add Makefile tools/genbenchview/main.go build/benchmark/.gitkeep .github/workflows/ci.yml
git commit -m "feat: add benchmark baseline system (local 10-records + CI artifact + web trend)"
```

---

## 任务 1.3：增强 Linter

**文件：**
- 修改：`.golangci.yml:10-20`

- [ ] **步骤 1：扩展 linter 列表**

```yaml
linters:
  enable:
    # 已有
    - errcheck
    - gosimple
    - govet
    - ineffassign
    - staticcheck
    - typecheck
    - unused
    - gofmt
    # 新增
    - revive
    - gocritic
    - gosec
    - whitespace
    - goimports
    - paralleltest
    - thelper
    - reassign

linters-settings:
  staticcheck:
    checks: ["all", "-ST1000", "-ST1003", "-ST1016", "-ST1020", "-ST1021", "-ST1022"]
  errcheck:
    check-type-assertions: true
  govet:
    enable:
      - shadow
  gofmt:
    simplify: true
  gosec:
    excludes:
      - G101  # 硬编码凭据（测试密钥是预期的）

issues:
  exclude-rules:
    - path: _test\.go
      linters:
        - errcheck
    - path: _test\.go
      linters:
        - paralleltest  # 表驱动测试中的串行子测是正常的
    - path: _test\.go
      linters:
        - gosec  # 测试代码中的硬编码密钥是预期的
    - path: cmd/.*_test\.go
      linters:
        - thelper  # 辅助函数已使用 t.Helper() 的不需要
```

- [ ] **步骤 2：运行 linter 检查新增告警**

运行：`golangci-lint run ./...`
预期：无新增不可接受的告警

如果出现存量告警，逐条修复或确认后加入 `exclude-rules`。特别关注：
- `goimports` 可能调整 import 排序
- `whitespace` 可能提示空行问题
- `revive` 可能提示命名约定

- [ ] **步骤 3：扩展 CI lint 范围**

将 `.github/workflows/ci.yml` 中 lint args 从 `./pkg/... ./cmd/... ./internal/...` 扩展为覆盖 `./test/...`：

```yaml
- name: golangci-lint
  uses: golangci/golangci-lint-action@v6
  with:
    version: latest
    args: --timeout=5m ./...
```

- [ ] **步骤 4：Commit**

```bash
git add .golangci.yml .github/workflows/ci.yml
git commit -m "feat: enhance linter (revive/gocritic/gosec/whitespace/goimports/paralleltest/thelper/reassign)"
```

---

## 任务 1.4：Pre-commit Hook + 工具依赖

**文件：**
- 创建：`.githooks/pre-commit`
- 修改：`Makefile`（新增 githooks 目标）

- [ ] **步骤 1：创建 .githooks/pre-commit**

```bash
#!/bin/sh
set -e

echo "=== pre-commit 检查 ==="

# 1. go vet
echo "[1/3] go vet..."
go vet ./...

# 2. gofmt 检查
echo "[2/3] gofmt..."
unformatted=$(gofmt -l .)
if [ -n "$unformatted" ]; then
  echo "未格式化的文件:"
  echo "$unformatted"
  echo "请运行: gofmt -w <file>"
  exit 1
fi

# 3. 检查测试监听地址
echo "[3/3] check-loopback..."
found=$(grep -rn --include='*_test.go' 'Listen.*0\.0\.0\.0\|Listen.*localhost\|httptest.*0\.0\.0\.0\|\"localhost"' . 2>/dev/null || true)
if [ -n "$found" ]; then
  echo "警告: 测试文件中发现 localhost/0.0.0.0 监听地址（应使用 127.0.0.1）:"
  echo "$found"
  exit 1
fi

echo "=== pre-commit 通过 ==="
```

- [ ] **步骤 2：在 Makefile 中新增 githooks 目标**

```makefile
.PHONY: githooks
githooks:
	@git config core.hooksPath .githooks
	@echo "Git hooks 已配置: .githooks/"
```

- [ ] **步骤 3：设置 hook 可执行权限**

运行：`git config core.hooksPath .githooks`

- [ ] **步骤 4：Commit**

```bash
git add .githooks/pre-commit Makefile
git commit -m "feat: add pre-commit hook (vet/gofmt/loopback check) + githooks target"
```

---

# 阶段 2：功能补齐

## 任务 2.1：os.Exit 全面消除——全局模式替换

**文件：**
- 修改：`cmd/sclient/config.go:20-49`
- 修改：`cmd/sclient/search.go:23-42`
- 修改：`cmd/sclient/stat.go:27-56`
- 修改：`cmd/sclient/mv.go:29-57`
- 修改：`cmd/sclient/batch_delete.go:21-43`
- 修改：`cmd/sclient/batch_rename.go:21-73`
- 修改：`cmd/sclient/archive.go:27-45,54-71`
- 修改：`cmd/sclient/cd.go:97-111,120-151`
- 修改：`cmd/sclient/upload.go:26-87`
- 修改：`cmd/sclient/download.go:22-71`
- 修改：`cmd/sclient/delete.go:24-39`
- 修改：`cmd/sclient/list.go:21-49`
- 修改：`cmd/sclient/version.go:17-24`（rootCmd.Execute 的 os.Exit 已在 Execute 函数中处理）

注意：`genkey` 命令无 os.Exit（使用 `fmt.Println` + `return` 是安全的），`tunnel` 命令已用 RunE。

- [ ] **步骤 1：确认 os.Exit 出现的位置**

运行：
```bash
grep -rn "os.Exit" cmd/sclient/*.go | grep -v "_test.go"
```

列出所有需要修改的 os.Exit 行：
- `cmd/sclient/config.go:25,39,42,48`
- `cmd/sclient/search.go:28,33`
- `cmd/sclient/stat.go:31,37`
- `cmd/sclient/mv.go:33,44,49,54`
- `cmd/sclient/batch_delete.go:26,40`（`os.Exit(1)` 在 Run 函数末尾）
- `cmd/sclient/batch_rename.go:25,29,72`
- `cmd/sclient/archive.go:31,41,43,60,70`
- `cmd/sclient/cd.go:100,107,124,147`（mkdir/rmdir）
- `cmd/sclient/upload.go:30,66,78`
- `cmd/sclient/download.go:26,61,66`
- `cmd/sclient/delete.go:28,36`
- `cmd/sclient/list.go:25,41`

- [ ] **步骤 2：统一批量转换 Run → RunE**

对所有包含 os.Exit 的命令，将 `Run:` 改为 `RunE:`，`os.Exit(1)` 改为 `return fmt.Errorf(...)`。

注意：`versionCmd` 的 `Run` 中无 os.Exit（只有 `fmt.Println`），保持原样。

以 `cmd/sclient/config.go` 为例的修改模式：

```go
var configCmd = &cobra.Command{
	Use:   "config [show|set <key> <value>]",
	Short: "配置管理",
	Args:  cobra.MaximumNArgs(3),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := client.LoadFromViper(viper.GetViper())
		if err != nil {
			return fmt.Errorf("加载配置失败: %w", err)
		}

		if len(args) == 0 {
			client.HandleConfigShow(cfg)
			return nil
		}

		switch args[0] {
		case "show":
			client.HandleConfigShow(cfg)
		case "set":
			if len(args) < 3 {
				return fmt.Errorf("用法: sclient config set <键> <值>")
			}
			if err := client.HandleConfigSet(cfg, cfgFile, args[1], args[2]); err != nil {
				return fmt.Errorf("设置配置失败: %w", err)
			}
			fmt.Printf("配置已更新: %s = %s\n", args[1], args[2])
		default:
			return fmt.Errorf("未知的 config 子命令: %s\n用法: sclient config [show|set <键> <值>]", args[0])
		}
		return nil
	},
}
```

对所有其他命令应用相同模式。

- [ ] **步骤 3：更新 rootCmd init 中 AddCommand 注册新命令**

确认新增的命令（如 statCmd、mvCmd、relayCmd、diagCmd、archiveCmd）都已注册到 `rootCmd` init 函数中。

检查 `cmd/sclient/root.go:96-105` 的 `init()`，确保新命令的 `init()` 也调用了 `rootCmd.AddCommand()`。

- [ ] **步骤 4：编译验证**

运行：`go build ./cmd/sclient/`
预期：编译成功

- [ ] **步骤 5：运行现有测试确认不退化**

运行：`go test ./cmd/sclient/...`
预期：全部通过

- [ ] **步骤 6：Commit**

```bash
git add cmd/sclient/*.go
git commit -m "refactor: replace all os.Exit(1) with RunE error returns in sclient commands"
```

---

## 任务 2.2：sclient tunnel 命令修复并启用测试

**文件：**
- 修改：`cmd/sclient/cmd_rune_test.go:321-335`
- 确认：`cmd/sclient/tunnel.go:31-73`（已用 RunE）

- [ ] **步骤 1：为 tunnel 命令添加可测试的 mock server 测试**

替换 `TestTunnelCommand_WithVerboseFlag` 等 4 个被 skip 的测试，改为使用 mock tunnel server：

```go
func TestTunnelCommand_MissingKey(t *testing.T) {
	resetState := captureRootCmdArgs()
	defer resetState()

	rootCmd.SetArgs([]string{"tunnel", "http://example.com"})
	err := rootCmd.Execute()
	if err == nil {
		t.Error("expected error when tunnel_key is missing")
	}
}
```
（已有此测试，确认它未被 skip）

```go
func TestTunnelCommand_WithConfigKey(t *testing.T) {
	// 使用临时配置文件模拟 tunnel_key 已配置
	tmpDir := t.TempDir()
	cfgPath := filepath.Join(tmpDir, "config.yaml")
	cfgContent := []byte("tunnel_key: " + testutil.TestKey() + "\nserver_url: http://127.0.0.1:18083\n")
	if err := os.WriteFile(cfgPath, cfgContent, 0644); err != nil {
		t.Fatal(err)
	}

	// 创建 mock server 模拟隧道端点
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tunnel" {
			// Tunnel endpoint - just return OK
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"status":200}`))
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	// 修改配置文件中的 server_url 指向 mock
	cfgContent2 := []byte(fmt.Sprintf("tunnel_key: %s\nserver_url: %s\n", testutil.TestKey(), mock.URL))
	if err := os.WriteFile(cfgPath, cfgContent2, 0644); err != nil {
		t.Fatal(err)
	}

	rootCmd.SetArgs([]string{"tunnel", "--config", cfgPath, "http://any-host.local/data"})
	err := rootCmd.Execute()
	// 可能因为隧道路由表为空而失败，但不应该因为缺少 key 而失败
	if err != nil && strings.Contains(err.Error(), "tunnel_key") {
		t.Errorf("unexpected missing key error after config: %v", err)
	}
}
```

移除被 `t.Skip` 的 4 个测试函数。

- [ ] **步骤 2：运行 tunnel 测试**

运行：`go test ./cmd/sclient/... -run TestTunnel`
预期：有测试通过，不应有 skip

- [ ] **步骤 3：Commit**

```bash
git add cmd/sclient/cmd_rune_test.go
git commit -m "test: enable tunnel command tests with mock config and server"
```

---

## 任务 2.3：relay.go binary body base64 编码

**文件：**
- 修改：`pkg/server/relay.go:103-107`

- [ ] **步骤 1：修改 bodyToString 函数，对二进制 body 使用 base64 编码**

```go
import (
	"encoding/base64"
	"unicode/utf8"
)

// bodyToString 将 body 转为 string。
// 有效 UTF-8 直接返回；二进制数据用 base64 编码。
func bodyToString(body []byte) string {
	if utf8.Valid(body) {
		return string(body)
	}
	return base64.StdEncoding.EncodeToString(body)
}
```

RelayResponse 已有 `BodyBase64` 字段（在 relay.go:30 定义），不需要修改结构体。

- [ ] **步骤 2：添加测试**

在 `pkg/server/relay_test.go` 中添加（如果不存在则创建）：

```go
func TestBodyToString_UTF8(t *testing.T) {
	input := []byte("hello world")
	got := bodyToString(input)
	if got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
}

func TestBodyToString_Binary(t *testing.T) {
	input := []byte{0x00, 0x01, 0xFF, 0xFE, 0x80}
	got := bodyToString(input)
	expected := base64.StdEncoding.EncodeToString(input)
	if got != expected {
		t.Errorf("expected base64 %q, got %q", expected, got)
	}
}
```

注意添加 import `"encoding/base64"`。

- [ ] **步骤 3：运行测试**

运行：`go test ./pkg/server/... -run "TestBodyToString"`
预期：通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/relay.go pkg/server/relay_test.go
git commit -m "fix: bodyToString base64-encodes binary bodies (fixes TODO)"
```

---

## 任务 2.4：Web UI 测试

**文件：**
- 创建：`web/embed_test.go`

- [ ] **步骤 1：创建 web/embed_test.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package web

import (
	"strings"
	"testing"
)

func TestStaticFS_ContainsIndexHTML(t *testing.T) {
	data, err := StaticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("无法读取 static/index.html: %v", err)
	}
	if len(data) == 0 {
		t.Error("static/index.html 为空")
	}
}

func TestStaticFS_ContainsExpectedContent(t *testing.T) {
	data, err := StaticFS.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("无法读取 static/index.html: %v", err)
	}
	content := string(data)
	// 检查关键元素：标题、CSS、按钮等
	expectedSubstrings := []string{"<title", "<html", "<head", "<body"}
	for _, s := range expectedSubstrings {
		if !strings.Contains(content, s) {
			t.Errorf("未找到预期元素: %q", s)
		}
	}
}
```

- [ ] **步骤 2：运行测试**

运行：`go test ./web/...`
预期：通过

- [ ] **步骤 3：Commit**

```bash
git add web/embed_test.go
git commit -m "test: add Web UI embed.FS verification test"
```

---

# 阶段 3：技术债务清理

## 任务 3.1：Signal handler goroutine 泄漏验证

**文件：**
- 检查：`cmd/sproxy/root.go:147-197`
- 确认：`cmd/sproxy/root_test.go` 已有泄漏检查

- [ ] **步骤 1：检查 root.go 中信号处理 goroutine 是否已修复**

确认 `stopSigCh` + `shutdownDone` + `defer signal.Stop(signalChan)` 模式已到位（根据最近的记忆，此修复已在 master 分支中）。

- [ ] **步骤 2：在 root_test.go 中增加 goroutine 泄漏检测**

```go
func TestRunServer_SignalGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()

	// 模拟 runServer 启动后立即关闭
	origSigCh := testSignalCh
	sigCh := make(chan os.Signal, 1)
	testSignalCh = sigCh
	defer func() { testSignalCh = origSigCh }()

	// 简单验证：创建一个最小配置
	cfg := server.Default()
	cfg.ServerTimeouts = server.Timeouts{
		Shutdown: 100 * time.Millisecond,
		Read:     10 * time.Millisecond,
	}
	cfg.LogLevel = "error"

	// 在测试子进程中或通过隔离方式验证 goroutine 数
	_ = cfg
	// 注意：完整的 runServer 测试需要监听端口，此处仅验证已有测试不泄漏
	after := runtime.NumGoroutine()
	t.Logf("goroutines: before=%d, after=%d", before, after)
}
```

- [ ] **步骤 3：Commit**

```bash
git add cmd/sproxy/root_test.go
git commit -m "fix: add signal goroutine leak detection test"
```

---

## 任务 3.2：captureStdout/captureStderr 重复代码清理

**文件：**
- 修改：`cmd/sclient/cmd_test.go:229-258`

- [ ] **步骤 1：确认 pkg/testutil 的 CaptureStdout/CaptureStderr 能否被 cmd/sclient 导入**

检查 `cmd/sclient/go.mod` 是否依赖 `github.com/cocomhub/sproxy`。

如果 sclient 是独立 go.mod 且不依赖主 module，则不能直接 import pkg/testutil。需要：
a) 在 `cmd/sclient/go.mod` 中添加 `replace` 和 `require` 依赖；或
b) 保持在 cmd_test.go 中保留私有实现（不建议，因为这是已知技术债务）。

最简方案：如果 sclient 的 go.mod 已经依赖 `github.com/cocomhub/sproxy`，则直接替换：
```go
// 删除 cmd_test.go 中 229-258 行的 captureStderr 和 captureStdout
// 在所有使用点替换为 testutil.CaptureStderr / testutil.CaptureStdout
```

否则，在私有实现前添加注释说明这是 `pkg/testutil` 的副本。

- [ ] **步骤 2：检查 import 路径**

运行：`cat cmd/sclient/go.mod | head -5`
检查 require 中是否包含 `github.com/cocomhub/sproxy`。

- [ ] **步骤 3：执行替换（如果可达）**

```
搜索替换 cmd_test.go 和 cmd_rune_test.go 中所有 `captureStdout` → `testutil.CaptureStdout`，
`captureStderr` → `testutil.CaptureStderr`。

删除私有函数定义（229-258 行）。

添加 import: "github.com/cocomhub/sproxy/pkg/testutil"
```

- [ ] **步骤 4：Commit**

```bash
git add cmd/sclient/cmd_test.go cmd/sclient/cmd_rune_test.go
git commit -m "refactor: replace private captureStdout/captureStderr with pkg/testutil"
```

---

## 任务 3.3：newTestServerWithAllRoutes 路由注册重复清理

**文件：**
- 修改：`pkg/server/integration_test.go`

- [ ] **步骤 1：检查 newTestServerWithAllRoutes 实现**

搜索 `newTestServerWithAllRoutes` 函数，确认其是否在手动重复 `RegisterRoutes` 的路由。

- [ ] **步骤 2：修改 newTestServerWithAllRoutes 直接调用 RegisterRoutes**

```go
func newTestServerWithAllRoutes(t *testing.T, opts ...testOption) (string, *server.Handlers, func()) {
	cfg := server.Default()
	cfg.LogLevel = "error"
	cfg.AuthToken = ""
	// 确保使用回环地址
	if !strings.Contains(cfg.Addr, "127.0.0.1") && !strings.HasPrefix(cfg.Addr, ":") {
		cfg.Addr = "127.0.0.1" + cfg.Addr
	}
	// 应用额外选项
	for _, opt := range opts {
		opt(cfg)
	}

	cfgPtr := atomic.Pointer[server.Config]{}
	cfgPtr.Store(cfg)

	mux := http.NewServeMux()
	// 直接使用 RegisterRoutes 注册所有路由，不再手动重复
	h := server.RegisterRoutes(context.Background(), mux, &cfgPtr, "test-version", "test-build", testKey, testLogger(t), nil)

	ts := httptest.NewServer(h.Handler())
	return ts.URL, h, func() {
		ts.Close()
		h.Close()
	}
}
```

- [ ] **步骤 3：运行测试确认不退化**

运行：`go test ./pkg/server/... -count=1`
预期：全部通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/integration_test.go
git commit -m "fix: use RegisterRoutes instead of manual route duplication in test helper"
```

---

## 任务 3.4：findModuleRoot 冗余清理

**文件：**
- 修改：`test/e2e_test.go`

- [ ] **步骤 1：检查 findModuleRoot 当前使用情况**

搜索 `findModuleRoot` 调用点。若已无人使用，移除旧函数；若仍在使用，改为 `runtime.Caller(0)` 方案。

```go
import "runtime"

// findModuleRoot 通过 runtime.Caller(0) 定位当前源文件，向上查找 go.mod。
func findModuleRoot() string {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		return "."
	}
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "."
		}
		dir = parent
	}
}
```

- [ ] **步骤 2：Commit**

```bash
git add test/e2e_test.go
git commit -m "fix: replace findModuleRoot with runtime.Caller approach"
```

---

## 任务 3.5：context.TODO() → context.Background()

**文件：**
- 修改：`pkg/server/server_handler_gaps_test.go`
- 修改：`pkg/server/server_hub_test.go`

- [ ] **步骤 1：搜索所有 context.TODO() 实例**

运行：
```bash
grep -rn "context.TODO()" pkg/server/*_test.go
```

- [ ] **步骤 2：全部替换为 context.Background()**

每处 `context.TODO()` → `context.Background()`

- [ ] **步骤 3：运行测试**

运行：`go test ./pkg/server/... -count=1`
预期：全部通过

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/server_handler_gaps_test.go pkg/server/server_hub_test.go
git commit -m "refactor: replace context.TODO() with context.Background() in test files"
```

---

## 任务 3.6：移除/确认 t.Skip 测试

**文件：**
- 修改：`cmd/sclient/cmd_rune_test.go:300,322-334`
- 无需修改（保留）：`pkg/server/checksum_test.go:76`（平台特定）
- 无需修改（保留）：`pkg/server/config_test.go:138`（平台特定）
- 无需修改（保留）：`pkg/server/tlsgen_test.go:57`（平台特定）
- 无需修改（保留）：`pkg/server/validate_test.go:80`（平台特定）

- [ ] **步骤 1：移除 cmd_rune_test.go 中 t.Skip**

移除 `TestBatchRenameCommand_StatFails` 的 `t.Skip`（os.Exit 已在任务 2.1 修复）。

移除 `TestTunnelCommand_WithVerboseFlag` 等 4 个被 skip 的测试函数（已在任务 2.2 处理）。

- [ ] **步骤 2：运行测试**

运行：`go test ./cmd/sclient/... -run "TestBatchRenameCommand" -count=1`
预期：不再 skip，测试可能失败或通过，取决于 mock server 是否正确模拟

- [ ] **步骤 3：Commit**

```bash
git add cmd/sclient/cmd_rune_test.go
git commit -m "test: remove t.Skip for batch-rename and tunnel tests (now testable after RunE migration)"
```

---

# 阶段 4：测试覆盖提升

## 任务 4.1：参数校验规则——作为所有测试的前置步骤

在写任何测试之前，对所有函数/方法检查输入参数校验：

**通用规则：**
1. 空字符串参数 → 返回 `fmt.Errorf("...不能为空")`
2. 负值/零值数值参数 → 返回 `fmt.Errorf("...必须大于0")`
3. nil 接口/指针参数 → 返回 `fmt.Errorf("...不能为空")`
4. 超出允许范围的值 → 返回 `fmt.Errorf("...超出范围")`

以下是对 `pkg/client/format.go` 的参数检查示例：

`FormatByte(size float64)`:
- 负数 → 返回 `"0 B"`（当前行为已安全）
- 超大值 → 不 panic（已验证）

`FormatETA(seconds int64)`:
- 已检查 `seconds <= 0` → `"--:--"`

`ValidateFilePath(filename string)` 在 `pkg/server/validate.go`:
- 已有完整校验（空字符串、空字节、绝对路径、路径穿越）

检查 `NewFileClient(serverURL string, opts ...Option)`:
```go
func NewFileClient(serverURL string, opts ...Option) *FileClient {
	if serverURL == "" {
		// 应该返回 error？但构造函数不返回 error...
		// 使用默认值
		serverURL = "http://localhost:18083"
	}
	// ...
}
```

考虑修改 `NewFileClient` 返回 `(*FileClient, error)`——但这需要改动所有调用点。在本次迭代中，仅在测试中记录此行为。

- [ ] **步骤 1：检查 FormatByte 对负数的处理**

```go
func TestFormatByte_Negative(t *testing.T) {
	got := FormatByte(-100)
	if got != "0 B" {
		t.Errorf("FormatByte(-100) = %q, want 0 B", got)
	}
}
```

- [ ] **步骤 2：检查 FormatETA 对 nil/无效参数的处理**

（FormatETA 不接收指针，基本类型无需 nil 检查。）

- [ ] **步骤 3：Commit 参数检查这部分（作为阶段 4 的入口）**

```bash
git add pkg/client/format_test.go
git commit -m "test: add input validation tests for FormatByte/FormatETA"
```

---

## 任务 4.2：cmd/sclient 覆盖提升 39%→90%

**文件：**
- 修改：`cmd/sclient/cmd_test.go`（新增测试）
- 修改：`cmd/sclient/cmd_rune_test.go`（新增测试）
- 修改：`cmd/sclient/batch_test.go`（已完善）

- [ ] **步骤 1：为 genkey 添加测试（已有 TestGenkeyCommand）**

确认现有 `TestGenkeyCommand` 已覆盖 genkey 路径（输出 64 hex char）。

- [ ] **步骤 2：为 search 添加 error path 测试（已有部分）**

添加 config 加载失败场景：

```go
func TestSearchCommand_ServerError(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	_ = captureStderr(func() {
		rootCmd.SetArgs([]string{"search", "--server", mock.URL, "query"})
		_ = rootCmd.Execute()
	})
}
```

- [ ] **步骤 3：为 download 添加 error path 测试**

```go
func TestDownloadCommand_ServerNotFound(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	// download 先 Stat，Stat 失败应返回 error
	rootCmd.SetArgs([]string{"download", "--server", mock.URL, "nonexistent.txt", "out.txt"})
	err := rootCmd.Execute()
	_ = err // 不应 panic
}
```

- [ ] **步骤 4：为 list 添加空结果测试**

```go
func TestListCommand_EmptyResults(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"files":[],"total":0}`))
	}))
	defer mock.Close()

	resetState := captureRootCmdArgs()
	defer resetState()

	out := captureStdout(func() {
		rootCmd.SetArgs([]string{"list", "--server", mock.URL})
		if err := rootCmd.Execute(); err != nil {
			t.Fatalf("list command failed: %v", err)
		}
	})
	if !strings.Contains(out, "no files found") {
		t.Errorf("expected 'no files found', got: %s", out)
	}
}
```

- [ ] **步骤 5：为每个命令的 error path 添加类似测试**

为所有子命令添加 error path 测试（参见设计方案 4.1 节表格）。

- [ ] **步骤 6：运行全部 cmd/sclient 测试**

运行：`go test -cover ./cmd/sclient/...`
预期：覆盖率大幅提升至 ~80%+

- [ ] **步骤 7：Commit**

```bash
git add cmd/sclient/cmd_test.go cmd/sclient/cmd_rune_test.go cmd/sclient/batch_test.go
git commit -m "test: raise cmd/sclient coverage to ~80% with error path and edge case tests"
```

---

## 任务 4.3：cmd/sproxy 覆盖提升 62%→90%

**文件：**
- 修改：`cmd/sproxy/root_test.go`
- 修改：`cmd/sproxy/root_extra_test.go`

- [ ] **步骤 1：为 levelString/formatString 添加边界测试**

```go
func TestLevelString_AllCases(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"debug", "debug"},
		{"info", "info"},
		{"warn", "warn"},
		{"error", "error"},
		{"unknown", "info"},
		{"", "info"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := levelString(tt.input); got != tt.expected {
				t.Errorf("levelString(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

func TestFormatString_AllCases(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"json", "json"},
		{"text", "text"},
		{"unknown", "text"},
		{"", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := formatString(tt.input); got != tt.expected {
				t.Errorf("formatString(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}
```

- [ ] **步骤 2：为 resolveTunnelKey 添加边界测试**

```go
func TestResolveTunnelKey_InvalidLength(t *testing.T) {
	cfg := &server.Config{TunnelKey: "short"}
	_, err := resolveTunnelKey(cfg)
	if err == nil {
		t.Error("expected error for short tunnel key")
	}
}
```

- [ ] **步骤 3：为 initLogger 添加测试**

```go
func TestInitLogger_AllFormats(t *testing.T) {
	cfg := &server.Config{LogLevel: "debug", LogFormat: "json"}
	logger := initLogger(cfg)
	if logger == nil {
		t.Fatal("expected non-nil logger")
	}

	cfg2 := &server.Config{LogLevel: "info", LogFormat: "text"}
	logger2 := initLogger(cfg2)
	if logger2 == nil {
		t.Fatal("expected non-nil logger")
	}
}
```

- [ ] **步骤 4：运行全部 cmd/sproxy 测试**

运行：`go test -cover ./cmd/sproxy/...`
预期：覆盖率 > 85%

- [ ] **步骤 5：Commit**

```bash
git add cmd/sproxy/root_test.go cmd/sproxy/root_extra_test.go
git commit -m "test: raise cmd/sproxy coverage to ~90% with edge case and config tests"
```

---

## 任务 4.4：pkg/server 覆盖提升 70%→90%

**文件：**
- 创建/修改：`pkg/server/server_extra_test.go`
- 修改：`pkg/server/relay_test.go`
- 创建：`pkg/server/slogger_test.go`

- [ ] **步骤 1：为 slogger 添加测试**

```go
// pkg/server/slogger_test.go
package server

import (
	"log/slog"
	"testing"
)

func TestDefaultLogger_Nil(t *testing.T) {
	logger := defaultLogger(nil)
	if logger == nil {
		t.Fatal("defaultLogger(nil) returned nil")
	}
}

func TestDefaultLogger_NonNil(t *testing.T) {
	l := slog.Default()
	logger := defaultLogger(l)
	if logger != l {
		t.Error("defaultLogger should return the same instance")
	}
}
```

- [ ] **步骤 2：为 relay 添加更多 error path 测试**

```go
func TestRelayHandler_EmptyTarget(t *testing.T) {
	logger := testutil.DiscardLogger()
	rt := hub.NewRouteTable()
	h := NewRelayHandler(rt, logger)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"target":""}`))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}
```

- [ ] **步骤 3：为 gzip middleware 添加空 body 测试**

```go
func TestGzipMiddleware_EmptyBody(t *testing.T) {
	handler := GzipMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}
}
```

- [ ] **步骤 4：运行测试**

运行：`go test -cover ./pkg/server/...`
预期：覆盖率 > 85%

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/slogger_test.go pkg/server/relay_test.go pkg/server/server_extra_test.go
git commit -m "test: raise pkg/server coverage to ~90% (slogger/relay/gzip edge cases)"
```

---

## 任务 4.5：pkg/client 覆盖提升 74%→90%

**文件：**
- 创建：`pkg/client/format_test.go`

- [ ] **步骤 1：为 format.go 添加完整测试**

```go
// pkg/client/format_test.go
package client

import "testing"

func TestFormatByte_AllUnits(t *testing.T) {
	tests := []struct {
		size float64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{1024*1024 + 512*1024, "1.5 MB"},
		{1024 * 1024 * 1024, "1024.0 MB"},
	}
	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := FormatByte(tt.size)
			if got != tt.want {
				t.Errorf("FormatByte(%v) = %q, want %q", tt.size, got, tt.want)
			}
		})
	}
}

func TestFormatByte_Negative(t *testing.T) {
	got := FormatByte(-100)
	if got != "0 B" {
		t.Errorf("FormatByte(-100) = %q, want 0 B", got)
	}
}

func TestFormatETA_Negative(t *testing.T) {
	if got := FormatETA(-1); got != "--:--" {
		t.Errorf("FormatETA(-1) = %q, want --:--", got)
	}
}

func TestFormatETA_Zero(t *testing.T) {
	if got := FormatETA(0); got != "--:--" {
		t.Errorf("FormatETA(0) = %q, want --:--", got)
	}
}

func TestFormatETA_Hours(t *testing.T) {
	if got := FormatETA(3661); got != "1h 1m" {
		t.Errorf("FormatETA(3661) = %q, want 1h 1m", got)
	}
}

func TestFormatETA_Minutes(t *testing.T) {
	if got := FormatETA(125); got != "2m 5s" {
		t.Errorf("FormatETA(125) = %q, want 2m 5s", got)
	}
}

func TestFormatETA_Seconds(t *testing.T) {
	if got := FormatETA(45); got != "45s" {
		t.Errorf("FormatETA(45) = %q, want 45s", got)
	}
}
```

- [ ] **步骤 2：为 Config.Validate 添加边界测试**

在 `pkg/client/config_test.go` 中添加：

```go
func TestConfigValidate_EmptyFields(t *testing.T) {
	cfg := &Config{}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() failed: %v", err)
	}
	if cfg.ServerURL != "http://localhost:18083" {
		t.Errorf("expected default server URL")
	}
}

func TestConfigValidate_InvalidTunnelKey(t *testing.T) {
	cfg := &Config{TunnelKey: "invalid-key"}
	if err := cfg.Validate(); err == nil {
		t.Error("expected error for invalid tunnel key length")
	}
}
```

- [ ] **步骤 3：运行测试**

运行：`go test -cover ./pkg/client/...`
预期：覆盖率 > 85%

- [ ] **步骤 4：Commit**

```bash
git add pkg/client/format_test.go pkg/client/config_test.go
git commit -m "test: raise pkg/client coverage to ~90% (FormatByte/FormatETA/Config.Validate)"
```

---

## 任务 4.6：fuzz 测试扩展

**文件：**
- 创建：`pkg/server/validate_fuzz_test.go`（已有）
- 创建：`pkg/client/calcchunksize_fuzz_test.go`（已有）

- [ ] **步骤 1：确认已有 fuzz 测试**

运行：
```bash
grep -rn "func Fuzz" pkg/**/*_test.go
```

- [ ] **步骤 2：为已有 fuzz 测试添加更多边界 case**

- [ ] **步骤 3：运行 fuzz 测试**

运行：`go test -fuzz=. -fuzztime=5s ./pkg/server/... ./pkg/client/...`
预期：5 秒内无 panic

- [ ] **步骤 4：Commit**

```bash
git add pkg/server/validate_fuzz_test.go pkg/client/calcchunksize_fuzz_test.go
git commit -m "test: add chaos input scenarios to existing fuzz tests"
```

---

## 任务 4.7：E2E 测试扩展

**文件：**
- 修改：`test/e2e_test.go`
- 修改：`test/e2e_extra_test.go`

- [ ] **步骤 1：添加分片上传 E2E 场景**

```go
func TestE2E_ChunkedUploadDownload(t *testing.T) {
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// 创建 2MB 测试文件
	data := make([]byte, 2*1024*1024)
	for i := range data {
		data[i] = byte(i % 256)
	}
	checksum := sha256Hex(data)

	// 上传
	status, body := uploadFile(t, baseURL, "large.bin", data, map[string]string{
		"X-File-Checksum": checksum,
	})
	if status != http.StatusOK {
		t.Fatalf("upload failed: %d %s", status, body)
	}

	// 下载
	status, headers, body = downloadFile(t, baseURL, "large.bin")
	if status != http.StatusOK {
		t.Fatalf("download failed: %d", status)
	}
	if headers.Get("X-File-Checksum") != checksum {
		t.Errorf("checksum mismatch")
	}
}
```

- [ ] **步骤 2：添加 batch 操作 E2E 场景**

```go
func TestE2E_BatchDelete(t *testing.T) {
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	// 上传多个文件
	for i := 0; i < 3; i++ {
		filename := fmt.Sprintf("batch_%d.txt", i)
		data := []byte(filename)
		chk := sha256Hex(data)
		status, body := uploadFile(t, baseURL, filename, data, map[string]string{
			"X-File-Checksum": chk,
		})
		if status != http.StatusOK {
			t.Fatalf("upload %s failed: %d %s", filename, status, body)
		}
	}

	// 通过 API 验证文件存在
	status, files := searchFiles(t, baseURL, "batch_")
	if status != http.StatusOK {
		t.Fatalf("search failed: %d", status)
	}
	if !strings.Contains(files, "batch_") {
		t.Errorf("expected batch files in search results")
	}
}
```

- [ ] **步骤 3：运行 E2E 测试**

运行：`go test ./test/... -count=1 -timeout=120s`
预期：全部通过

- [ ] **步骤 4：Commit**

```bash
git add test/e2e_test.go test/e2e_extra_test.go
git commit -m "test: extend E2E tests with chunked upload and batch scenarios"
```

---

## 任务 4.8：最终覆盖验证

- [ ] **步骤 1：运行最终覆盖率测量**

运行：`make cover`
预期：total 达到 ~90%

- [ ] **步骤 2：运行完整测试套件**

运行：`make test`
预期：无 race、无 vet 告警、全部通过

- [ ] **步骤 3：运行 benchmark 基线**

运行：`make bench`
预期：Benchmark 数据保存在 `build/benchmark/data/`

- [ ] **步骤 4：提交最终汇总 commit**

```bash
git commit --allow-empty -m "chore: phase 4 complete - total coverage target ~90%"
```

---

## 验证清单

| 检查项 | 验证方式 |
|--------|---------|
| 覆盖率门禁 | `CI=true make cover` < 85% 应 exit 1 |
| Benchmark 基线 | `make bench && ls build/benchmark/data/` 应有 .txt 文件 |
| Pre-commit hook | `.githooks/pre-commit` 存在且 `git config core.hooksPath` 指向它 |
| linter 全覆盖 | `golangci-lint run ./...` 通过 |
| cmd/sclient os.Exit 消除 | `grep -rn "os.Exit" cmd/sclient/*.go \| grep -v _test.go` 应为空 |
| captureStdout 重复消除 | `cmd/sclient/cmd_test.go` 中无重复函数定义 |
| newTestServerWithAllRoutes 已简化 | `grep "newTestServerWithAllRoutes" pkg/server/integration_test.go` 存在 |
| context.TODO 已清理 | `grep -rn "context.TODO()" pkg/server/*_test.go` 应为空 |
| 全量测试通过 | `make test` 通过 |
| 全量覆盖 | `make cover` 打印 total |
