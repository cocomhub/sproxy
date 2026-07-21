# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

PROJECT_NAME := sproxy

# ═══════════════════════════════════════════════════════════════════════════════
# STANDARD VARIABLES — 所有项目一致
# ═══════════════════════════════════════════════════════════════════════════════
BUILD_DIR       ?= build
BIN_DIR         ?= $(BUILD_DIR)/bin
RAW_GO          ?= go
GOOS            ?= $(shell $(RAW_GO) env GOOS)
GOARCH          ?= $(shell $(RAW_GO) env GOARCH)
HOST_GOARCH     ?= $(shell $(RAW_GO) env GOHOSTARCH)
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif
GO              := GOOS=$(GOOS) GOARCH=$(GOARCH) $(RAW_GO)
GORACE          := -race
GOTEST_COUNT    ?= -count=1
GOTEST_TIMEOUT  ?= -timeout=5m
NOTEST_IGNORE   := .notestignore
SUB_MODULE_DIRS := $(shell find . -name 'go.mod' \
  -not -path './$(BUILD_DIR)/*' \
  -not -path './.claude/*' \
  -not -path './vendor/*' \
  -not -path './web/e2e/*' \
  -exec dirname {} \; | sort -u | grep -v '^\.$$')

# ═══════════════════════════════════════════════════════════════════════════════
# CUSTOM VARIABLES — 本项目按需配置
# ═══════════════════════════════════════════════════════════════════════════════
VERSION         ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_AT        ?= $(shell date +"%Y-%m-%dT%H:%M:%SZ")
COVER_THRESHOLD ?= 70
SONAR_PROJECT_KEY ?= cocomhub_sproxy
SKIP_VERSION    ?= true
VERSION_DIR     ?= internal/version/build
GOTAGS          ?=
GOBUILD_EXTRA   ?= -v
GO_LDFLAGS      := 
CONFIG_FILE     ?= $(BUILD_DIR)/config.yaml
CMD_NAMES       := sproxy sclient
BIN_NAME        := $(BIN_DIR)/$(PROJECT_NAME)-$(GOOS)-$(GOARCH)$(EXE)

# ═══════════════════════════════════════════════════════════════════════════════
# OTHER VARIABLES — 原有变量，保留不动
# ═══════════════════════════════════════════════════════════════════════════════
GOFMT := gofmt
ALL_SRC := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./... 2>/dev/null)
BENCH_DATA_DIR := $(BUILD_DIR)/benchmark/data
BENCH_WEB_DIR := $(BUILD_DIR)/benchmark/web
COVER_DATA_DIR := $(BUILD_DIR)/coverage/data
COVER_WEB_DIR := $(BUILD_DIR)/coverage/web
TIMING_DATA_DIR := $(BUILD_DIR)/timing/data
TIMING_WEB_DIR := $(BUILD_DIR)/timing/web
REPORT_DIR := $(BUILD_DIR)/report
TOOLS := \
    github.com/google/addlicense@latest \
    golang.org/x/perf/cmd/benchstat@latest

.DEFAULT_GOAL := help

# ═══════════════════════════════════════════════════════════════════════════════
# STANDARD TARGETS — 所有项目一致
# ═══════════════════════════════════════════════════════════════════════════════

.PHONY: prepare
prepare:
	@mkdir -p $(BUILD_DIR) $(BIN_DIR)
ifneq ($(SKIP_VERSION), true)
	@mkdir -p $(VERSION_DIR)
	@if ! git diff --quiet HEAD 2>/dev/null; then \
		git diff HEAD > $(VERSION_DIR)/dirty_info.txt 2>/dev/null; \
		echo "[prepare] dirty_info.txt updated ($(VERSION_DIR)/dirty_info.txt)"; \
	else \
		rm -f $(VERSION_DIR)/dirty_info.txt; \
	fi
endif

.PHONY: build
build: fmt
	@mkdir -p $(BIN_DIR)
	@$(foreach name,$(CMD_NAMES),echo "Building $(name)"; $(GO) build $(GOBUILD_EXTRA) $(GO_LDFLAGS) -o $(BIN_DIR)/$(name)$(EXE) ./cmd/$(name);)

.PHONY: build-ci
build-ci: prepare
	@mkdir -p $(BIN_DIR)
	@$(foreach name,$(CMD_NAMES),echo "Building $(name)"; $(GO) build $(GOBUILD_EXTRA) $(GO_LDFLAGS) -o $(BIN_DIR)/$(name)$(EXE) ./cmd/$(name);)

.PHONY: test
test: prepare
	$(GO) test $(GORACE) $(GOTEST_COUNT) $(GOTEST_TIMEOUT) $(GOTAGS) ./...

.PHONY: test-ci test-cover
test-ci test-cover: prepare
	$(GO) test $(GORACE) $(GOTEST_COUNT) $(GOTEST_TIMEOUT) $(GOTAGS) -coverprofile=$(BUILD_DIR)/cover.out ./...

.PHONY: notest
notest:
	@scripts/check-test-files.sh

.PHONY: cover-check
cover-check: test-cover
	@total=$$(go tool cover -func=$(BUILD_DIR)/cover.out | tail -1 | awk '{print $$NF}' | sed 's/%//'); \
	if [ -z "$$total" ]; then \
		echo "FAIL: could not compute coverage"; \
		exit 1; \
	fi; \
	if (( $$(echo "$$total < $(COVER_THRESHOLD)" | bc -l) )); then \
		echo "FAIL: coverage $$total% < threshold $(COVER_THRESHOLD)%"; \
		exit 1; \
	fi; \
	echo "PASS: coverage $$total% >= threshold $(COVER_THRESHOLD)%"

.PHONY: vet
vet:
	$(RAW_GO) vet ./...

.PHONY: lint
lint:
	golangci-lint run

.PHONY: bench
bench:
	@mkdir -p $(BUILD_DIR)/bench
	$(GO) test -bench=. -benchmem -count=5 -run=^$$ ./... 2>&1 | tee $(BUILD_DIR)/bench/output.txt

# 本地基准测试（保留 metadata 头，供 benchstat 本地对比用）
.PHONY: bench-local
bench-local: prepare
	@mkdir -p $(BENCH_DATA_DIR)
	@echo "=== Running benchmarks ==="
	@outfile="$(BENCH_DATA_DIR)/$(shell git rev-parse --abbrev-ref HEAD)-$(shell git rev-parse --short HEAD)-$(shell date +%Y%m%dT%H%M%S).txt"; \
	  echo "Benchmark results will be saved to: $$outfile"; \
	  echo "branch: $(shell git rev-parse --abbrev-ref HEAD)" > "$$outfile"; \
	  echo "commit: $(shell git rev-parse --short HEAD)" >> "$$outfile"; \
	  echo "date: $(shell date -u +%Y%m%dT%H%M%SZ)" >> "$$outfile"; \
	  echo "" >> "$$outfile"; \
	  rc=0; \
	  $(GO) test -bench=. -benchmem -count=3 -benchtime=500ms -run=^$$ ./internal/... ./pkg/... ./cmd/sproxy/... > "$$outfile.tmp" 2>&1 || rc=$$?; \
	  cat "$$outfile.tmp" >> "$$outfile"; \
	  cat "$$outfile.tmp"; \
	  rm -f "$$outfile.tmp"; \
	  echo ""; \
	  echo "=== 清理旧记录（保留最近 10 条）==="; \
	  cd $(BENCH_DATA_DIR) && ls -t *.txt 2>/dev/null | tail -n +11 | xargs -r rm -f; \
	  echo "Done. Records in $(BENCH_DATA_DIR): $$(ls $(BENCH_DATA_DIR)/*.txt 2>/dev/null | wc -l)"; \
	  exit $$rc

bench-old: bench-local

.PHONY: check-loopback
check-loopback:
	@echo "=== Checking for unsafe listen addresses ==="; \
	issues=0; \
	# Check non-test source files for 0.0.0.0 (excluding pkg/server/config.go which has intentional defaults); \
	if grep -rn '0\.0\.0\.0' --include='*.go' . \
		| grep -v 'pkg/server/downloader/ssrf.go' \
		| grep -v '_test.go' \
		| grep -v 'vendor/' \
		| grep -v 'testdata/' \
		| grep -v 'fixtures/' \
		| grep -v '\.pb\.go' \
		| grep -v 'docs/' \
		| grep -v '\.claude/' \
		| grep -v 'pkg/server/config.go' \
		| grep '.' > /dev/null 2>&1; then \
		echo "FAIL: found potential unsafe listen addresses (0.0.0.0) in source:"; \
		grep -rn '0\.0\.0\.0' --include='*.go' . \
			| grep -v '_test.go' \
			| grep -v 'vendor/' \
			| grep -v 'testdata/' \
			| grep -v 'fixtures/' \
			| grep -v '\.pb\.go' \
			| grep -v 'docs/' \
			| grep -v '\.claude/' \
			| grep -v 'pkg/server/config.go'; \
		issues=$$((issues + 1)); \
	fi; \
	# Check test files for unsafe listen addresses; \
	if grep -rn --include='*_test.go' 'Listen.*0\.0\.0\.0\|\.Addr\s*=\s*"localhost' . 2>/dev/null \
		| grep -v './.claude/' \
		| grep '.' > /dev/null 2>&1; then \
		echo "FAIL: test files contain unsafe listen addresses:"; \
		grep -rn --include='*_test.go' 'Listen.*0\.0\.0\.0\|\.Addr\s*=\s*"localhost' . 2>/dev/null \
			| grep -v './.claude/' \
			| grep -v 'xfer/grpc'; \
		issues=$$((issues + 1)); \
	fi; \
	if [ "$$issues" -gt 0 ]; then exit 1; fi; \
	echo "OK: all loopback checks passed"

.PHONY: gofix
gofix:
	$(RAW_GO) fix ./...

.PHONY: addlicense
addlicense:
	addlicense -c "The Cocomhub Authors. All rights reserved." -s=only -ignore ".claude/**" -ignore ".trae/**" -ignore ".cursor/**" .

.PHONY: fmt
fmt: gofix addlicense
	@echo "Running gofmt on ALL_SRC ..."
	@$(GOFMT) -e -s -l -w $(ALL_SRC)

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(VERSION_DIR)
	rm -f cover*.out coverage.tmp *.cover coverage.out

.PHONY: test-all
test-all:
	@for dir in $(SUB_MODULE_DIRS); do \
		echo "=== Testing $$dir ==="; \
		cd $$dir && $(RAW_GO) test $(GORACE) $(GOTEST_COUNT) $(GOTEST_TIMEOUT) ./... || exit 1; \
		cd $(CURDIR); \
	done

.PHONY: build-all
build-all:
	@for dir in $(SUB_MODULE_DIRS); do \
		echo "=== Building $$dir ==="; \
		cd $$dir && $(RAW_GO) build ./... || exit 1; \
		cd $(CURDIR); \
	done

.PHONY: check-ci
check-ci: vet lint check-loopback notest build-ci test-cover cover-check test-all build-all

.PHONY: sonar-analyze
sonar-analyze:
	@if [ ! -f sonar-project.properties ]; then \
		echo "missing sonar-project.properties"; exit 1; \
	fi
	sonar-scanner

.PHONY: sonar-remediate
sonar-remediate:
	@if [ ! -f sonar-project.properties ]; then \
		echo "missing sonar-project.properties"; exit 1; \
	fi
	sonar-scanner -Dsonar.remediation.projectKey=$(SONAR_PROJECT_KEY)

.PHONY: help
help:
	@echo "Usage: make <target>"
	@echo ""
	@echo "Standard targets:"
	@echo "  build           Build all command binaries (depends on fmt)"
	@echo "  build-ci        Build without fmt, for CI"
	@echo "  test            Run tests (no coverage)"
	@echo "  test-ci         Run tests with coverage (alias: test-cover)"
	@echo "  test-cover      Run tests with coverage"
	@echo "  cover-check     Check coverage meets threshold"
	@echo "  notest          Verify all packages have test files"
	@echo "  vet             Run go vet"
	@echo "  lint            Run golangci-lint"
	@echo "  bench-local     Run benchmarks with metadata (local use)"
	@echo "  bench           Run benchmarks (CI, output to build/bench/output.txt)"
	@echo "  check-loopback  Check for unsafe listen addresses"
	@echo "  gofix           Run go fix"
	@echo "  addlicense      Add license headers"
	@echo "  fmt             Format code (gofix + addlicense + gofmt)"
	@echo "  clean           Clean build artifacts"
	@echo "  test-all        Test all sub-modules"
	@echo "  build-all       Build all sub-modules"
	@echo "  check-ci        Full CI pipeline"
	@echo "  sonar-analyze    Run SonarQube Cloud analysis"
	@echo "  sonar-remediate  Run SonarQube Cloud remediation"
	@echo ""
	@echo "Custom targets:"
	@echo "  build-<name>    Build a specific command (e.g., build-sproxy, build-sclient)"
	@echo "  test-packages   Run tests grouped by package (with vet + check-loopback)"
	@echo "  cover-html      Generate coverage HTML report"
	@echo "  cover-trend     Coverage trend visualization"
	@echo "  bench-old       Alias for bench-local"
	@echo "  bench-compare   Compare two benchmark runs"
	@echo "  bench-web       Benchmark web report"
	@echo "  timing-trend    Timing trend visualization"
	@echo "  report          Generate unified report"
	@echo "  run             Build and run sproxy"
	@echo "  show-version    Show sproxy version"
	@echo "  tools           Install build tools"
	@echo "  githooks        Install git hooks"

# ═══════════════════════════════════════════════════════════════════════════════
# CUSTOM TARGETS — 本项目特有
# ═══════════════════════════════════════════════════════════════════════════════

# 构建单个命令
.PHONY: build-%
build-%: fmt
	@mkdir -p $(BIN_DIR)
	@echo "Building $*"
	@$(GO) build $(GOBUILD_EXTRA) $(GO_LDFLAGS) -o $(BIN_DIR)/$*$(EXE) ./cmd/$*

# 分组运行测试（简化调试时定位失败的包）
.PHONY: test-packages
test-packages: vet check-loopback
	@echo "=== cmd/sproxy/... ===" && $(GO) test -race -count=1 -timeout=30s ./cmd/sproxy/... 2>&1
	@echo "=== cmd/sclient/... ===" && $(GO) test -race -count=1 -timeout=30s ./cmd/sclient/... 2>&1
	@echo "=== internal/... ===" && $(GO) test -race -count=1 -timeout=30s ./internal/... 2>&1
	@echo "=== pkg/tunnel/... ===" && $(GO) test -race -count=1 -timeout=30s ./pkg/tunnel/... 2>&1
	@echo "=== pkg/client/... ===" && $(GO) test -race -count=1 -timeout=30s ./pkg/client/... 2>&1
	@echo "=== pkg/server/... ===" && $(GO) test -race -count=1 -timeout=60s ./pkg/server/... 2>&1
	@echo "=== test/... ===" && $(GO) test -race -count=1 -timeout=60s ./test/... 2>&1

# 覆盖率 HTML 报告
.PHONY: cover-html
cover-html: test-cover
	@go tool cover -html=$(BUILD_DIR)/cover.out -o $(BUILD_DIR)/cover.html
	@echo "Coverage report: file://$(abspath $(BUILD_DIR)/cover.html)"

# 覆盖率趋势
.PHONY: cover-trend
cover-trend:
	@mkdir -p $(COVER_WEB_DIR)
	@go run tools/gencoverview/main.go -data=$(COVER_DATA_DIR) -out=$(COVER_WEB_DIR)
	@echo "Coverage trend: file://$(abspath $(COVER_WEB_DIR)/index.html)"

.PHONY: bench-old
bench-old:
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

# 基准比较
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

# 基准 web 报告
.PHONY: bench-web
bench-web:
	@mkdir -p $(BENCH_WEB_DIR)
	@go run tools/genbenchview/main.go -data=$(BENCH_DATA_DIR) -out=$(BENCH_WEB_DIR)
	@echo "Benchmark web report: file://$(abspath $(BENCH_WEB_DIR)/index.html)"

# 测试耗时趋势
.PHONY: timing-trend
timing-trend:
	@mkdir -p $(TIMING_WEB_DIR)
	@go run tools/gentimingview/main.go -data=$(TIMING_DATA_DIR) -out=$(TIMING_WEB_DIR)
	@echo "Timing trend: file://$(abspath $(TIMING_WEB_DIR)/index.html)"

# 统一报告
.PHONY: report
report: cover-html cover-trend bench bench-web timing-trend
	@mkdir -p $(REPORT_DIR)
	@go run tools/genreport/main.go -out=$(REPORT_DIR)
	@echo "=== 报告生成完成 ==="
	@echo "Dashboard: file://$(abspath $(REPORT_DIR)/index.html)"
	@echo "Benchmark: file://$(abspath $(BENCH_WEB_DIR)/index.html)"
	@echo "Coverage:  file://$(abspath $(COVER_WEB_DIR)/index.html)"
	@echo "Timing:    file://$(abspath $(TIMING_WEB_DIR)/index.html)"

.PHONY: run
run: build
	$(BIN_NAME) --config $(CONFIG_FILE)

.PHONY: show-version
show-version:
	$(BIN_NAME) --version

.PHONY: tools
tools:
	@for tool in $(TOOLS); do \
		echo "Installing $$tool..."; \
		go install $$tool; \
	done

.PHONY: githooks
githooks:
	@git config core.hooksPath .githooks
	@echo "Git hooks configured: .githooks/"

# 达尔文-哥德尔机（Goedel-Go）— 自动优化工具
GOEDEL_DIR := $(abspath $(dir $(firstword $(MAKEFILE_LIST)))/../goedel-go)
GOEDEL_BIN := $(GOEDEL_DIR)/goedel-go
GOEDEL_DATA := $(abspath $(BUILD_DIR)/optimize)

.PHONY: optimize optimize-scan optimize-baseline optimize-report optimize-compare

$(GOEDEL_BIN):
	cd $(GOEDEL_DIR) && go build -o goedel-go .

optimize: $(GOEDEL_BIN)
	@mkdir -p $(GOEDEL_DATA)
	cd $(GOEDEL_DIR) && ./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) baseline && \
		./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) scan && \
		./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) report

optimize-scan: $(GOEDEL_BIN)
	@mkdir -p $(GOEDEL_DATA)
	cd $(GOEDEL_DIR) && ./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) scan

optimize-baseline: $(GOEDEL_BIN)
	@mkdir -p $(GOEDEL_DATA)
	cd $(GOEDEL_DIR) && ./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) baseline

optimize-report: $(GOEDEL_BIN)
	@mkdir -p $(GOEDEL_DATA)
	cd $(GOEDEL_DIR) && ./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) report

optimize-compare: $(GOEDEL_BIN)
	@mkdir -p $(GOEDEL_DATA)
	cd $(GOEDEL_DIR) && ./goedel-go --project $(realpath .) --data-dir $(GOEDEL_DATA) compare
