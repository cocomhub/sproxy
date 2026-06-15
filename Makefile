VERSION ?= $(shell git describe --tags --always --dirty)
BUILD_DIR ?= build
BUILD_AT ?= $(shell date +"%Y-%m-%dT%H:%M:%SZ")

GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
GO ?= GOOS=$(GOOS) GOARCH=$(GOARCH) go
GOFLAGS ?= -v
GO_LDFLAGS ?= -w -s
GO_LDFLAGS += -X main.Version=$(VERSION) -X main.BuildAt=$(BUILD_AT)

PROJECT_NAME ?= sproxy
BIN_DIR ?= $(BUILD_DIR)/bin
BIN_NAME ?= $(BIN_DIR)/$(PROJECT_NAME)-$(GOOS)-$(GOARCH)
CONFIG_FILE ?= $(BUILD_DIR)/config.yaml

GOFMT=gofmt

# OS binary suffix
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif

# Static list of commands (cross-platform: no shell for-loop needed).
CMD_NAMES := sproxy sclient
# all .go files using go list (cross-platform).
ALL_SRC = $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./...)

.PHONY: build build-%

build: fmt
	@mkdir -p $(BIN_DIR)
	@$(foreach name,$(CMD_NAMES),echo "Building $(name)"; GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$(name)$(EXE) ./cmd/$(name);)

build-%: fmt
	@mkdir -p $(BIN_DIR)
	@echo "Building $*"
	@GOOS=$(GOOS) GOARCH=$(GOARCH) go build $(GOFLAGS) -ldflags "$(GO_LDFLAGS)" -o $(BIN_DIR)/$*$(EXE) ./cmd/$*

# 格式化目标
.PHONY: fmt
fmt: addlicense fix
	@echo Running gofmt on ALL_SRC ...
	@$(GOFMT) -e -s -l -w $(ALL_SRC)
	@echo Running gofumpt on ALL_SRC ...
# 	@$(GOFUMPT) -e -l -w $(ALL_SRC)

# 添加许可证
.PHONY: addlicense
addlicense:
	addlicense -c "The Cocomhub Authors. All rights reserved." -s=only .

# 修复目标
.PHONY: fix
fix:
	@echo Running go fix ./...
	@$(GO) fix ./...

.PHONY: clean

clean:
	rm -rf $(BIN_DIR)

.PHONY: vet
vet:
	@echo Running go vet ./...
	@$(GO) vet ./...

.PHONY: check-loopback
check-loopback:
	@echo "=== 检查测试监听地址 ==="
	@! grep -rn --include='*_test.go' 'Listen.*0\.0\.0\.0\|Listen.*localhost\|httptest.*0\.0\.0\.0\|\.Addr\s*=\s*"localhost' . 2>/dev/null | grep -v './.claude/worktrees/' | grep -v 'xfer/grpc' || { echo "错误: 发现测试文件含 0.0.0.0 或 localhost 监听地址！（worktrees 和已废弃 xfer/grpc 除外）"; exit 1; }
	@echo "OK"

.PHONY: test

test: vet check-loopback
	@echo "Running go test -race ./..."
	@mkdir -p $(TIMING_DATA_DIR)
	@outfile="$(TIMING_DATA_DIR)/$(shell git rev-parse --abbrev-ref HEAD)-$(shell git rev-parse --short HEAD)-$(shell date +%Y%m%dT%H%M%S).txt"; \
	  echo "branch: $(shell git rev-parse --abbrev-ref HEAD)" > "$$outfile"; \
	  echo "commit: $(shell git rev-parse --short HEAD)" >> "$$outfile"; \
	  echo "date: $(shell date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$$outfile"; \
	  echo "---" >> "$$outfile"; \
	  $(GO) test -race -count=1 -timeout=120s ./... 2>&1 | tee -a "$$outfile"; \
	  echo ""; \
	  echo "=== 清理旧记录（保留最近 10 条）==="; \
	  cd $(TIMING_DATA_DIR) && ls -t *.txt 2>/dev/null | tail -n +11 | xargs -r rm -f; \
	  echo "Done."

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

COVER_THRESHOLD ?= 85

.PHONY: cover

cover: vet
	@mkdir -p $(BUILD_DIR)/coverage $(COVER_DATA_DIR)
	$(GO) test -count=1 -coverprofile=$(BUILD_DIR)/coverage/cover.out ./...
	@$(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out | grep -E "total"
	@echo "=== 保存覆盖率记录 ==="
	@outfile="$(COVER_DATA_DIR)/$(shell git rev-parse --abbrev-ref HEAD)-$(shell git rev-parse --short HEAD)-$(shell date +%Y%m%dT%H%M%S).txt"; \
	  echo "branch: $(shell git rev-parse --abbrev-ref HEAD)" > "$$outfile"; \
	  echo "commit: $(shell git rev-parse --short HEAD)" >> "$$outfile"; \
	  echo "date: $(shell date -u +%Y-%m-%dT%H:%M:%SZ)" >> "$$outfile"; \
	  echo "---" >> "$$outfile"; \
	  $(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out >> "$$outfile"; \
	  echo "保存到: $$outfile"; \
	  echo "=== 清理旧记录（保留最近 10 条）==="; \
	  cd $(COVER_DATA_DIR) && ls -t *.txt 2>/dev/null | tail -n +11 | xargs -r rm -f; \
	  echo "Done."
	@echo "=== 覆盖率门禁检查 ==="
	@pct=$$($(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out | grep -E "^total" | awk '{print $$NF}' | sed 's/%//; s/\.[0-9]*//'); \
	  echo "total coverage: $$pct% (threshold: $(COVER_THRESHOLD)%)"; \
	  if [ "$$CI" = "true" ] && [ "$$pct" -lt "$(COVER_THRESHOLD)" ]; then \
	    echo "FAIL: coverage $$pct% < threshold $(COVER_THRESHOLD)%"; exit 1; \
	  fi

# 覆盖率 HTML 报告（不含 race，避免 test/e2e_test.go 已知 race 阻断报告生成）
.PHONY: cover-html

cover-html: vet
	@mkdir -p $(BUILD_DIR)/coverage
	$(GO) test -count=1 -coverprofile=$(BUILD_DIR)/coverage/cover.out ./...
	@$(GO) tool cover -func=$(BUILD_DIR)/coverage/cover.out | grep -E "total"
	$(GO) tool cover -html=$(BUILD_DIR)/coverage/cover.out -o $(BUILD_DIR)/coverage/cover.html
	@echo "Coverage report: file://$(BUILD_DIR)/coverage/cover.html"

.PHONY: run

run: build
	$(BIN_NAME) --config $(CONFIG_FILE)

.PHONY: show-version

show-version:
	$(BIN_NAME) --version

TOOLS := \
    github.com/google/addlicense@latest \
    golang.org/x/perf/cmd/benchstat@latest

.PHONY: tools
tools:
	@for tool in $(TOOLS); do \
		echo "Installing $$tool..."; \
		go install $$tool; \
	done

BENCH_DIR := $(BUILD_DIR)/benchmark
BENCH_DATA_DIR := $(BENCH_DIR)/data
BENCH_WEB_DIR := $(BENCH_DIR)/web

COVER_DATA_DIR := $(BUILD_DIR)/coverage/data
COVER_WEB_DIR := $(BUILD_DIR)/coverage/web
TIMING_DATA_DIR := $(BUILD_DIR)/timing/data
TIMING_WEB_DIR := $(BUILD_DIR)/timing/web
REPORT_DIR := $(BUILD_DIR)/report

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

.PHONY: bench-web
bench-web:
	@mkdir -p $(BENCH_WEB_DIR)
	@go run tools/genbenchview/main.go -data=$(BENCH_DATA_DIR) -out=$(BENCH_WEB_DIR)
	@echo "Benchmark web report: file://$(abspath $(BENCH_WEB_DIR)/index.html)"

# === 覆盖率趋势 ===
.PHONY: cover-trend
cover-trend:
	@mkdir -p $(COVER_WEB_DIR)
	@go run tools/gencoverview/main.go -data=$(COVER_DATA_DIR) -out=$(COVER_WEB_DIR)
	@echo "Coverage trend: file://$(abspath $(COVER_WEB_DIR)/index.html)"

# === 测试耗时趋势 ===
.PHONY: timing-trend
timing-trend:
	@mkdir -p $(TIMING_WEB_DIR)
	@go run tools/gentimingview/main.go -data=$(TIMING_DATA_DIR) -out=$(TIMING_WEB_DIR)
	@echo "Timing trend: file://$(abspath $(TIMING_WEB_DIR)/index.html)"

# === 统一报告 ===
.PHONY: report
report: cover cover-trend bench bench-web timing-trend
	@mkdir -p $(REPORT_DIR)
	@go run tools/genreport/main.go -out=$(REPORT_DIR)
	@echo "=== 报告生成完成 ==="
	@echo "Dashboard: file://$(abspath $(REPORT_DIR)/index.html)"
	@echo "Benchmark: file://$(abspath $(BENCH_WEB_DIR)/index.html)"
	@echo "Coverage:  file://$(abspath $(COVER_WEB_DIR)/index.html)"
	@echo "Timing:    file://$(abspath $(TIMING_WEB_DIR)/index.html)"

.PHONY: githooks
githooks:
	@git config core.hooksPath .githooks
	@echo "Git hooks 已配置: .githooks/"
