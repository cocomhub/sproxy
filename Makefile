# Copyright 2026 The Cocomhub Authors. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
#
# shellcheck disable=SC1073,SC1064,SC1065,SC1072
#   本文件是 Makefile，不是 shell 脚本：`ifeq (...)` 等 make 语法让 shellcheck 无法解析
#   （实测 7 个解析类错误，全部落在 `ifeq ($(OS),Windows_NT)` 一行）。此处按文件豁免这几个
#   解析类码，不影响 scripts/*.sh 的检查。

PROJECT_NAME := sproxy

# ═══════════════════════════════════════════════════════════════════════════════
# STANDARD VARIABLES — 所有项目一致
# ═══════════════════════════════════════════════════════════════════════════════
BUILD_DIR       ?= build
BIN_DIR         ?= $(BUILD_DIR)/bin
RAW_GO          ?= go
DEADCODE_TOOL   ?= golang.org/x/tools/cmd/deadcode@v0.47.0
GOOS            ?= $(shell $(RAW_GO) env GOOS)
GOARCH          ?= $(shell $(RAW_GO) env GOARCH)
HOST_GOARCH     ?= $(shell $(RAW_GO) env GOHOSTARCH)
ifeq ($(OS),Windows_NT)
EXE := .exe
else
EXE :=
endif
GO                 := GOOS=$(GOOS) GOARCH=$(GOARCH) $(RAW_GO)
GORACE             := -race
GOTEST_COUNT       ?= -count=1
GOTEST_TIMEOUT     ?= -timeout=5m
GOTEST_TIMEOUT_E2E ?= -timeout=30m
NOTEST_IGNORE      := .notestignore
SUB_MODULE_DIRS := $(shell find . -name 'go.mod' \
  -not -path './$(BUILD_DIR)/*' \
  -not -path './.claude/*' \
  -not -path './vendor/*' \
  -not -path './web/e2e/*' \
  -exec dirname {} \; | sort -u | grep -v '^\.$$')

# ═══════════════════════════════════════════════════════════════════════════════
# CUSTOM VARIABLES — 本项目按需配置
# ═══════════════════════════════════════════════════════════════════════════════
VERSION         ?= $(shell git describe --tags --always --dirty --match 'v[0-9]*' 2>/dev/null || echo dev)
BUILD_AT        ?= $(shell date +"%Y-%m-%dT%H:%M:%SZ")
COVER_THRESHOLD ?= 70
SONAR_PROJECT_KEY ?= cocomhub_sproxy
SKIP_VERSION    ?= false
VERSION_DIR     ?= internal/build
# buildmeta 包编译时 embed 其包内 build/dirty_info.txt，路径为
# internal/buildmeta/build/。prepare 在 $(VERSION_DIR) 生成后拷到 $(EMBED_BASE)/build/。
EMBED_BASE     ?= internal/buildmeta
GOTAGS          ?=
GOBUILD_EXTRA   ?= -v
# 构建信息 -X 注入。注意：VERSION/BUILD_AT 须在 GO_LDFLAGS 之前定义（:= 立即求值）。
# 符号路径使用相对导入路径（link 的 -X 接受 main.V / 包内局部导入别名）：
#   main.Version/main.BuildAt       → cmd/sproxy、cmd/sclient 的包级变量（version 子命令 + --version 共用）
#   github.com/cocomhub/buildinfo.* → buildinfo 包级变量（Branch/CommitID/ReleaseURL）
COMMIT_ID      ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BRANCH         ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)
RELEASE_URL    ?= https://github.com/cocomhub/sproxy/releases
# 构建元信息注入（-X）的**单一事实源**：.goreleaser.yaml 的 builds[].ldflags 必须与本组
# 键值语义一致——发布产物与 `make build` 产物必须可对齐（同键集、同 v 前缀版本形式、
# 同 ReleaseURL、同 -trimpath）。注意 `-trimpath` 是 go build 开关（在 -ldflags 之外），
# 在 .goreleaser.yaml 里对应 `flags:` 而非 `ldflags:`。
# 门禁：internal/archcheck/build_flags_alignment_test.go。
GO_LD_FLAGS_X  := \
  -X main.Version=$(VERSION) \
  -X main.BuildAt=$(BUILD_AT) \
  -X github.com/cocomhub/buildinfo.CommitID=$(COMMIT_ID) \
  -X github.com/cocomhub/buildinfo.Branch=$(BRANCH) \
  -X github.com/cocomhub/buildinfo.ReleaseURL=$(RELEASE_URL)
GO_LDFLAGS     := -ldflags "$(GO_LD_FLAGS_X)" -trimpath
CONFIG_FILE     ?= $(BUILD_DIR)/config.yaml
STORAGE_ROOT    ?= ./storage
CMD_NAMES       := sproxy sclient
BIN_NAME        := $(BIN_DIR)/$(PROJECT_NAME)-$(GOOS)-$(GOARCH)$(EXE)

# ═══════════════════════════════════════════════════════════════════════════════
# OTHER VARIABLES — 原有变量，保留不动
# ═══════════════════════════════════════════════════════════════════════════════
GOFMT := gofmt
ALL_SRC := $(shell go list -f '{{range .GoFiles}}{{$$.Dir}}/{{.}} {{end}}' ./... 2>/dev/null)
BENCH_DATA_DIR := $(BUILD_DIR)/benchmark/data
BENCH_WEB_DIR := $(BUILD_DIR)/benchmark/web
# BENCH_TIMEOUT 是**每个包** benchmark 二进制的总时长上限（`go test -timeout` 的语义是「每包」）。
#
# **重要更正（2026-09-16 实验）**：`-timeout` 对 **benchmark 不生效**——把 60s 睡眠放进 benchmark
# 并加 `-timeout 5s`，用例仍会 PASS；同一睡眠放进 Test 才会 `panic: test timed out`（A/B/C 实验
# 见 docs/superpowers/learnings/2026-09-15-benchmark-ci-timeout-disk-io.md §3.2 的更正小节）。
# 因此它**不能**把「单个 op 卡死不再返回」变成带栈的 panic——那正是 CI 上只剩
# `Terminate orphan process`、没有栈的原因；真正的机制是**进程外看门狗** tools/benchwatch
# （见 BENCH_STALL_LIMIT / BENCH_STARTUP_GRACE）。
# 保留本 flag 的理由：对**非 benchmark** 的测试仍是有效兜底（`-run=^$` 下目前无测试，属预防性）。
# 取 240s：正常最慢包（pkg/server）约 90–100s，留 2x+ 余量；且 < 6m 预算 ⇒ 失败仍拿得到完整日志。
# 本地临时放宽：`make bench BENCH_TIMEOUT=600s`（命令行变量覆盖）。
BENCH_TIMEOUT := 240s

# BENCH_STALL_LIMIT / BENCH_STARTUP_GRACE 是**进程外**停滞看门狗（tools/benchwatch）的两个静默窗口：
#   - BENCH_STARTUP_GRACE：**首个字节之前**的静默上限。`go test ./...` 建包/冷缓存期间本来就无输出，
#     用短窗口会误杀健康运行 ⇒ 取 180s（< BENCH_TIMEOUT，仍能在 job 预算内失败）。
#   - BENCH_STALL_LIMIT：**已开始输出之后**的停滞窗口。benchmark 结果行每隔几秒就会刷出，
#     零增长 120s 只能是卡死（实测形态③是「静默 5 分钟」）⇒ 取 120s。
# 触发时 benchwatch 打印诊断（含日志尾部）并终止 go test **及其后代**，以退出码 66 结束 ⇒
# CI 在 2 分钟内**响亮失败**（而不是被 job 级 timeout-minutes 静默取消、丢掉全部证据）。
BENCH_STALL_LIMIT := 120s
BENCH_STARTUP_GRACE := 180s
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
	@mkdir -p $(BUILD_DIR) $(BIN_DIR) $(VERSION_DIR) $(EMBED_BASE)/build
ifneq ($(SKIP_VERSION), true)
	@if ! git diff --quiet HEAD 2>/dev/null; then \
		git diff HEAD > $(VERSION_DIR)/dirty_info.txt 2>/dev/null; \
		echo "[prepare] dirty_info.txt updated ($(VERSION_DIR)/dirty_info.txt)"; \
	else \
		rm -f $(VERSION_DIR)/dirty_info.txt; \
	fi
endif
	@# embed 副本**无条件**生成（SKIP_VERSION=true 与非 make 编译路径的兜底）：无 diff 时置空
	@# 文件（等同 clean），保证 internal/buildmeta 始终可编译。该文件被 .gitignore 忽略，
	@# 故干净 checkout 下任何编译入口都必须先跑本目标（GoReleaser 走 .goreleaser.yaml 的
	@# before.hooks；门禁：internal/archcheck/makefile_bench_deps_test.go）。
	@cp -f $(VERSION_DIR)/dirty_info.txt $(EMBED_BASE)/build/dirty_info.txt 2>/dev/null || : > $(EMBED_BASE)/build/dirty_info.txt

.PHONY: build
build: fmt prepare
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
	@bash scripts/check-test-files.sh $$($(RAW_GO) list ./... | sed 's|^github.com/cocomhub/sproxy|.|')

# L2/L3 真实 Vault 集成测试：docker 可用时起 hashicorp/vault dev 容器自动跑；
# 无 docker 时测试自动 t.Skip（不失败）。
.PHONY: test-vault
test-vault:
	bash scripts/test-vault.sh

# 发布脚本门禁：scripts/tag-release.sh 的计划生成/跳过/同源规则（纯 bash 夹具测试）。
.PHONY: test-tag-release
test-tag-release:
	@bash scripts/tag-release_test.sh

# 备份/恢复脚本门禁：storage_root 多租户布局的打包/恢复/版本校验（纯 bash 夹具测试）。
.PHONY: test-backup-restore
test-backup-restore:
	@bash scripts/sproxy-backup_test.sh
	@bash scripts/sproxy-restore_test.sh

.PHONY: web-test
web-test:
	@node --check web/static/app.js
	@node --check web/static/upload.js
	@node --check web/static/cloudfilename.js
	@node --check web/static/sclient/crypto.js
	@node --check web/static/sclient/sha256.js
	@node --check web/static/sclient/sig.js
	@node --check web/static/sclient/config.js
	@node --check web/static/sclient/log.js
	@node --check web/static/sclient/transport.js
	@node --check web/static/sclient/util.js
	@node --check web/static/sclient/api/files.js
	@node --check web/static/sclient/api/cloud.js
	@node --check web/static/sclient/api/share.js
	@node --check web/static/sclient/api/config.js
	@node --check web/static/sclient/api/hub.js
	@node --check web/static/sclient/api/sync.js
	@node --check web/static/sclient/api/audit.js
	@node --check web/static/sclient/api/mesh.js
	@node --check web/static/sclient/api/index.js
	@node --check web/static/app-render.js
	@node --check web/static/user-volumes.js
	@node --check web/static/transfer-store.js
	@node --check web/static/download.js
	@node --check web/static/login.js
	@node --check web/static/qrcode.js
	@node --check web/static/download.test.js
	@node --check web/static/transfer-render.test.js
	@node --check web/static/sync.test.js
	node --test web/static/cloudfilename.test.js
	node --test web/static/transfer-store.test.js
	node --test web/static/transfer-render.test.js
	node --test web/static/sync.test.js
	node --test web/static/app-render.test.js
	node --test web/static/user-volumes.test.js
	node --test web/static/app-transfer-actions.test.js
	node --test web/static/upload.test.js
	node --test web/static/download.test.js
	node --test web/static/login.test.js
	node --test web/static/qrcode.test.js
	node --test web/static/sclient/sclient.test.js

.PHONY: cover-check
cover-check: test-cover
	@total=$$(go tool cover -func=$(BUILD_DIR)/cover.out | tail -1 | awk '{print $$NF}' | sed 's/%//'); \
	if [ -z "$$total" ]; then \
		echo "FAIL: could not compute coverage"; \
		exit 1; \
	fi; \
	below=$$(awk -v t="$$total" -v th="$(COVER_THRESHOLD)" 'BEGIN { print (t+0 < th+0) ? 1 : 0 }'); \
	if [ "$$below" = "1" ]; then \
		echo "FAIL: coverage $$total% < threshold $(COVER_THRESHOLD)%"; \
		exit 1; \
	fi; \
	echo "PASS: coverage $$total% >= threshold $(COVER_THRESHOLD)%"

# deadcode-check：把「不可达符号」从信息输出（make deadcode）升级为**失败门禁**。
#
# 口径与范围（**实测确认，勿想当然**）：
#   * 只覆盖 `./cmd/sproxy ./cmd/sclient` 两个 main 的**可达图**——deadcode 要求 main 包作入口，
#     且只报该图内不可达的函数。实测：cmd 侧未使用的函数会被报出；改用 `./...` 会把库包导出面
#     当根，输出 2000+ 行噪声（无可用信号）；把库包单列作入口则直接 `deadcode: no main packages`。
#     故库包内部的死代码不靠本门禁，而由 R11 墓碑清单
#     （internal/archcheck/dead_symbols_test.go）按已确认案例守。
#   * 不带 `-test`：会把「仅被测试引用」的 helper 一并报出——这正是期望口径（生产不可达即
#     死代码）；确需保留的逐条登记在 .deadcodeignore（ERE；路径分隔符写 `[/\\]` 以兼容 Windows
#     反斜杠），使豁免可审计而非静默。
#   * 消息一律 **ASCII**：Windows 控制台（CP936）下中文会乱码成“鍙戠幇...”（既有 echo 全为
#     ASCII 即此因），故不做中文化；由 gate_wiring_test.go 断言不得出现非 ASCII。
#   * `go run` 的下载/编译日志走 stderr（**不捕获**），只把 stdout 的发现当判定。
.PHONY: deadcode-check
deadcode-check: prepare
	@out=$$(go run golang.org/x/tools/cmd/deadcode@v0.47.0 ./cmd/sproxy ./cmd/sclient); \
	if [ -n "$$out" ]; then out=$$(printf '%s\n' "$$out" | grep -v -E -f .deadcodeignore || true); fi; \
	if [ -n "$$out" ]; then \
		echo "FAIL: unreachable symbols found (register intentional ones in .deadcodeignore):"; \
		printf '%s\n' "$$out"; \
		exit 1; \
	fi; \
	echo "PASS: deadcode has no unregistered symbols"

.PHONY: vet
vet: prepare
	$(RAW_GO) vet ./...

.PHONY: lint
lint: prepare
	golangci-lint run

# 各 sub-module 都是**独立 module**，`golangci-lint run ./...`（make lint）不跨 module——
# 这些 module 的 lint 问题会静默游离在门禁之外（曾发生：cmd/sclient 的 2 处恒真守卫
# 带 3 条 staticcheck SA4023 issue，而根 `./...` 全绿）。此处逐个 module 各跑一次，
# 覆盖的模块共 10 个，清单见 SUB_MODULE_DIRS（与 test-all / build-all 同一份，不另立
# 清单），即 cmd/sproxy、cmd/sclient、pkg/tunnel/mesh、pkg/tunnel/hub/ext/kad、
# pkg/tunnel/xfer/ext/{grpc,quic,webrtc,ws}、pkg/certmgr/ext/dnspod、
# pkg/telemetry/ext/otel。
# web/e2e 已被 SUB_MODULE_DIRS 的 -not -path './web/e2e/*' 排除，其专属门禁为
# lint-web-e2e。CI 侧由 Lint job 的 golangci-lint-action（无条件 addPath 导出二进制）
# 之后追加 `make lint-all` 步骤执行，PR 必经。
.PHONY: lint-all
lint-all: prepare
	@for dir in $(SUB_MODULE_DIRS); do \
		echo "=== Linting $$dir ==="; \
		cd $$dir && golangci-lint run --timeout=5m ./... || exit 1; \
		cd $(CURDIR); \
	done

# web/e2e 是嵌套 module（被 SUB_MODULE_DIRS 的 -not -path './web/e2e/*' 排除），
# 根 `golangci-lint run ./...` 扫不到它——CI 的 ui-e2e job 单独 lint 该 module，
# 本地用本 target 对齐同一门禁（GOWORK=off 避免 go.work 全 module 加载）。
.PHONY: lint-web-e2e
lint-web-e2e: prepare
	cd web/e2e && GOWORK=off golangci-lint run -c ../../.golangci.yml ./...

# e2e 测试文件（test/、test/e2e/ 下带 //go:build e2e 的套件）的 lint：
# 裸 `golangci-lint run`（make lint）在默认 build tags 下**不扫**这些文件，
# 故单列本 target 对齐同一门禁，避免 e2e 代码游离在 lint 之外。
.PHONY: lint-e2e
lint-e2e: prepare
	golangci-lint run --build-tags=e2e ./test/...

.PHONY: bench
bench: prepare
	@mkdir -p $(BUILD_DIR)/bench
	@# 退出码传播：POSIX sh 里管道的 `$?` 取自最后一个命令（tee）⇒ 失败会被吞成绿（2026-09-15 实证：
	@# pkg/server 的 `FAIL … exit status 1` 就发生在**成功**的 run 里）。因此把 go test 的退出码写进文件、
	@# 读完再 exit——纯 POSIX（dash 没有 pipefail），无需改 SHELL。
	@# 「边写日志边转发」现在由 tools/benchwatch 承担，它同时是**进程外**停滞看门狗（形态③：
	@# 单个 op 卡死不再返回）：零输出超过 -startup/-limit 即打印诊断（含日志尾部）、终止 go test
	@# **及其后代**、退出码 66 ⇒ CI 快速响亮失败并保留证据，而不是被 job 级取消静默掐断。
	@# 注：`-timeout $(BENCH_TIMEOUT)` 对 benchmark 不生效（见变量定义处更正），保留作预防性兜底。
	@# 先构建看门狗再运行（不用 `$(GO) run`：go run 会把子进程的非零退出码吞成 1，
	@# 丢失 66 这个「停滞」信号）。
	@$(GO) build -o $(BUILD_DIR)/bench/benchwatch ./tools/benchwatch
	@{ $(BUILD_DIR)/bench/benchwatch -startup $(BENCH_STARTUP_GRACE) -limit $(BENCH_STALL_LIMIT) -log $(BUILD_DIR)/bench/output.txt -- \
	     $(GO) test -bench=. -benchmem -count=5 -run=^$$ -timeout $(BENCH_TIMEOUT) ./... ; echo $$? > $(BUILD_DIR)/bench/.go_test_rc; } ; \
	  rc=$$(cat $(BUILD_DIR)/bench/.go_test_rc); rm -f $(BUILD_DIR)/bench/.go_test_rc; exit $$rc

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
	  $(GO) test -bench=. -benchmem -count=3 -benchtime=500ms -run=^$$ -timeout $(BENCH_TIMEOUT) ./internal/... ./pkg/... ./cmd/sproxy/... > "$$outfile.tmp" 2>&1 || rc=$$?; \
	  cat "$$outfile.tmp" >> "$$outfile"; \
	  cat "$$outfile.tmp"; \
	  rm -f "$$outfile.tmp"; \
	  echo ""; \
	  echo "=== 清理旧记录（保留最近 10 条）==="; \
	  cd $(BENCH_DATA_DIR) && ls -t *.txt 2>/dev/null | tail -n +11 | xargs -r rm -f; \
	  echo "Done. Records in $(BENCH_DATA_DIR): $$(ls $(BENCH_DATA_DIR)/*.txt 2>/dev/null | wc -l)"; \
	  exit $$rc

.PHONY: check-loopback
check-loopback:
	@echo "=== Checking for unsafe listen addresses ==="; \
	issues=0; \
	# Check non-test source files for 0.0.0.0 (excluding pkg/server/config.go which has intentional defaults); \
	# 注释行（`:行号: //`）一律跳过：注释绑不了端口，命中它们纯属误报（例：pkg/tunnel/mesh/mdns.go 的说明文字）； \
	if grep -rn '0\.0\.0\.0' --include='*.go' . \
		| grep -vE ':[0-9]+:[[:space:]]*//' \
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
			| grep -vE ':[0-9]+:[[:space:]]*//' \
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
	addlicense -c "The Cocomhub Authors. All rights reserved." -s=only -ignore ".claude/**" -ignore ".trae/**" -ignore ".cursor/**" -ignore "web/static/vendor/**" .

.PHONY: fmt
fmt: gofix addlicense
	@echo "Running gofmt on ALL_SRC ..."
	@$(GOFMT) -e -s -l -w $(ALL_SRC)

.PHONY: clean
clean:
	rm -rf $(BUILD_DIR) $(VERSION_DIR)
	rm -f cover*.out coverage.tmp *.cover coverage.out

.PHONY: test-all
test-all: prepare
	@for dir in $(SUB_MODULE_DIRS); do \
		echo "=== Testing $$dir ==="; \
		cd $$dir && $(RAW_GO) test $(GORACE) $(GOTEST_COUNT) $(GOTEST_TIMEOUT) ./... || exit 1; \
		cd $(CURDIR); \
	done

# 真二进制端到端测试：构建 sproxy/sclient 真实二进制 + 子进程启动，覆盖文件面/隧道/
# mesh/relay/quota/CLI 命令族等完整链路。默认 make test 不含（build-tag e2e 门控），
# CI e2e job 调用。递归 ./test/...，含 test/e2e/ 子包（CLI 真服务二进制 e2e；
# 该子包的孤儿用例已修复并纳入门禁）。
.PHONY: test-e2e
test-e2e: prepare
	$(GO) test $(GORACE) $(GOTEST_COUNT) $(GOTEST_TIMEOUT_E2E) -tags=e2e ./test/...

.PHONY: build-all
build-all: prepare
	@for dir in $(SUB_MODULE_DIRS); do \
		echo "=== Building $$dir ==="; \
		cd $$dir && $(RAW_GO) build ./... || exit 1; \
		cd $(CURDIR); \
	done

# 分层与包可见性门禁：断言 L(n) 不导入 L(>n)、子包只被父域/装配层导入、
# 新增（Managed）包的 pkg/ 依赖必须登记。登记表在 internal/archcheck/layers.go。
# 新增包若未登记层级，此目标即红。
.PHONY: archcheck
archcheck: prepare
	$(RAW_GO) test -count=1 ./internal/archcheck/

# 死代码检测（信息性，DEADCODE_TOOL 版本固定以保证可复现）：
# 不带 -test 会把「仅被测试引用」的 helper（NewMock/DiscardLogger/SetHostOnly/…）
# 一并报为不可达 ⇒ 输出永不为空，**不能**做失败条件。真正的防复活门禁是
# internal/archcheck 的墓碑用例（R11，TestNoResurrectedDeadSymbols）。
# 用 `go run pkg@version` 而非 `go get -tool`：避免为开发工具连带升级生产依赖。
.PHONY: deadcode
deadcode: prepare ## 列出从 main 不可达的函数（信息性输出，不作为失败条件）
	@echo "==> deadcode (cmd/sproxy cmd/sclient)"
	$(RAW_GO) run $(DEADCODE_TOOL) ./cmd/sproxy ./cmd/sclient

.PHONY: check-ci
check-ci: vet lint lint-all lint-web-e2e lint-e2e check-loopback notest archcheck deadcode build-ci test-cover cover-check test-all build-all

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
	@echo "  web-test        Run Web UI JS unit tests (node --test)"
	@echo "  test-tag-release Run scripts/tag-release.sh fixture tests"
	@echo "  test-backup-restore Run backup/restore scripts fixture tests"
	@echo "  backup           Backup storage_root to build/backups/"
	@echo "  restore          Restore storage_root from BACKUP=<tar.gz>"
	@echo "  notest          Verify all packages have test files"
	@echo "  vet             Run go vet"
	@echo "  lint            Run golangci-lint"
	@echo "  lint-all        Run golangci-lint for every sub-module (cmd + ext + hub + mesh)"
	@echo "  lint-e2e        Run golangci-lint for e2e-tagged test suites"
	@echo "  lint-web-e2e    Run golangci-lint for the nested web/e2e module"
	@echo "  bench-local     Run benchmarks with metadata (local use)"
	@echo "  bench           Run benchmarks (CI, output to build/bench/output.txt)"
	@echo "  check-loopback  Check for unsafe listen addresses"
	@echo "  gofix           Run go fix"
	@echo "  addlicense      Add license headers"
	@echo "  fmt             Format code (gofix + addlicense + gofmt)"
	@echo "  clean           Clean build artifacts"
	@echo "  test-all        Test all sub-modules"
	@echo "  test-e2e        Run real-binary e2e tests (build-tag e2e)"
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
build-%: fmt prepare
	@mkdir -p $(BIN_DIR)
	@echo "Building $*"
	@$(GO) build $(GOBUILD_EXTRA) $(GO_LDFLAGS) -o $(BIN_DIR)/$*$(EXE) ./cmd/$*

# 分组运行测试（简化调试时定位失败的包）
.PHONY: test-packages
test-packages: prepare vet check-loopback
	@echo "=== cmd/sproxy/... ===" && $(GO) test -race -count=1 -timeout=60s ./cmd/sproxy/... 2>&1
	@echo "=== cmd/sclient/... ===" && $(GO) test -race -count=1 -timeout=60s ./cmd/sclient/... 2>&1
	@echo "=== internal/... ===" && $(GO) test -race -count=1 -timeout=60s ./internal/... 2>&1
	@echo "=== pkg/tunnel/... ===" && $(GO) test -race -count=1 -timeout=60s ./pkg/tunnel/... 2>&1
	@echo "=== pkg/client/... ===" && $(GO) test -race -count=1 -timeout=60s ./pkg/client/... 2>&1
	@echo "=== pkg/server/... ===" && $(GO) test -race -count=1 -timeout=60s ./pkg/server/... 2>&1
	@echo "=== test/... (e2e tag) ===" && $(GO) test -race -count=1 -timeout=30m -tags=e2e ./test/... 2>&1

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

# 备份/恢复：storage_root 多租户布局的打包与恢复（脚本用法见 scripts/sproxy-backup.sh）。
# backup 默认把备份产出到 build/backups/；restore 需要 BACKUP=<tar.gz> 指定备份文件。
.PHONY: backup restore
backup:
	@bash scripts/sproxy-backup.sh --storage-root "$(STORAGE_ROOT)" --output $(BUILD_DIR)/backups
restore:
	@bash scripts/sproxy-restore.sh --backup "$(BACKUP)" --target "$(STORAGE_ROOT)"

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
