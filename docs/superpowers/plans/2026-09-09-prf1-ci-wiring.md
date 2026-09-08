# PR-F1 CI Wiring 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 修复 sproxy CI 测试接线断裂——把从未进 CI 的 10 个子 module 测试套件接入 CI；把 `test/` 下 4041 行真二进制 e2e 从"无条件挂在 `make test` 里"改为显式 `e2e` build-tag + 独立门控 job，使默认单测 run 变轻、hermetic，且不丢失任何既有 CI 门禁。

**架构：** 两条独立接线：(1) 新增 CI `test-submodules` job 跑现成 `make test-all`（SUB_MODULE_DIRS 已排除 web/e2e 与根 module，正好覆盖 cmd/sclient、cmd/sproxy 与 8 个 ext/hub/mesh 子 module）；(2) 为 `test/*.go` 整目录加 `//go:build e2e`，新增 `make test-e2e` target（`go test -tags=e2e ./test/...`）并在 CI 加独立 `e2e` job，从默认 `make test` 路径摘除重型二进制 e2e。

**技术栈：** Makefile（GNU make）、GitHub Actions（.github/workflows/ci.yml）、Go 1.26 build tags。

---

## 现场事实（已核验，供实现者直接采信）

- `go.work` 组合 11 个 module（根 `.` + cmd/sclient + cmd/sproxy + certmgr/ext/dnspod + telemetry/ext/otel + tunnel/hub/ext/kad + tunnel/mesh + xfer/ext/{grpc,quic,webrtc,ws} + web/e2e）。
- **workspace 模式下 `go test ./...`（根目录）只测根 module**：`go list ./...` = 46 包，cmd/sclient、cmd/sproxy、web/e2e 命中 0。`make test`（ci.yml test job 用）因此从不编译/运行子 module 任何测试。
- `Makefile` `SUB_MODULE_DIRS` = `find . -name go.mod` 排除 build/.claude/vendor/**web/e2e** 与根 `.` → 恰 10 个：cmd/sclient、cmd/sproxy、pkg/certmgr/ext/dnspod、pkg/telemetry/ext/otel、pkg/tunnel/hub/ext/kad、pkg/tunnel/mesh、pkg/tunnel/xfer/ext/{grpc,quic,webrtc,ws}。`test-all` target 已存在（逐个 `cd $dir && go test -race -count=1 -timeout=5m ./... || exit 1`），**CI 从不调用**。
- `cmd/sclient` 有 54 个 `_test.go`（约 16k 行）；`cmd/sproxy` 有 6 个 `_test.go`；均只在 `make test-all` 时运行。
- `test/` 目录 10 个文件、4041 行、package `sproxy_test`、无 `go:build`、无 `TestMain`、**全部是 `_test.go`**，helper 与用例同文件自足。因无 tag，现被 `make test` 的 `go test ./...` 吞入 → ubuntu + windows 两个 test job 每次都现场 `go build ./cmd/sproxy` 跑重型二进制 e2e（ci.yml test job 与 test-windows 皆然）。
- CI 现有 job：lint / test（ubuntu+Vault，`make test` + cover-check）/ test-windows / build（matrix，`make build-ci` + `build-all`）/ benchmark / sonar / ui-e2e。push master 与 PR 均触发。`paths-ignore` 已忽略 `docs/**`。
- 既定红线（memory）：所有 test job 监听 127.0.0.1；make target 是 CI 唯一入口（不写裸 go 命令）；lint 0 issues。

---

### 任务 1：CI `test-submodules` job —— 10 个子 module 测试套件首次入 CI

**文件：**
- 修改：`.github/workflows/ci.yml`（新增 job）
- 修改：`Makefile`（仅当需要时微调 `test-all`——预期无需，除非 window 下 find 行为差异）
- 验证：本地 `make test-all` 对全部 10 子 module 全绿；CI 新 job 跑绿

- [ ] **步骤 1：本地确认 `make test-all` 当前全绿基线**

运行：`make test-all 2>&1 | tail -20`
预期：每个子 module 打印 `=== Testing <dir> ===` 且无 `FAIL`、退出码 0。
若某个子 module 已有红/花测试，**记录并先修到绿**（这是它们首次受 CI 门控，必须从绿开始）。修不动 → 记入报告回传，不静默跳过。

- [ ] **步骤 2：ci.yml 新增 `test-submodules` job**

在 `test-windows` job 之后、`build` job 之前插入：

```yaml
  test-submodules:
    name: Test Sub-Modules (cmd + ext + hub + mesh)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v6
        with:
          go-version: "1.26"
          check-latest: true
      - name: prepare (buildmeta embed)
        run: make prepare
      - name: go test all sub-modules (race)
        run: make test-all
```

要点：**不加 `-timeout` 超短覆盖**（10 个 module 全 race，`test-all` 内建每 module `-timeout=5m`，整 job 天然会长）。`make test-all` 的 `|| exit 1` 已保证任一 module 失败即 job 失败。job 间 `concurrency`/`cancel-in-progress` 沿用文件级设置，无需改动。

- [ ] **步骤 3：push 分支 → 开 PR → CI 新 job 跑绿**

运行：`git push -u origin <branch>`，`gh pr create`（PR title 聚焦功能：`ci: 子 module 测试套件接入 CI（test-submodules job）`）。
预期：CI `Test Sub-Modules` job 绿；若花/失败，按 flake-vs-真缺陷判定修到绿（首次受门控，任何失败都不静默放行）。

---

### 任务 2：`test/` 真二进制 e2e 归位显式门控（build tag + `make test-e2e` + CI `e2e` job）

**文件：**
- 修改：`test/*.go`（10 个文件，整目录加同一 `//go:build e2e`）
- 修改：`Makefile`（新增 `test-e2e` target；视需要并入 `check-ci` 之外）
- 修改：`.github/workflows/ci.yml`（新增 `e2e` job）
- 验证：无 tag 时 `go test ./...` 跳过 test/；`make test-e2e` 全绿；CI e2e job 绿

- [ ] **步骤 1：给 `test/*.go` 全部 10 个文件加 build tag**

每个文件 SPDX 头之后、`package sproxy_test` 之前插入：

```go
//go:build e2e

```

用脚本统一处理并人工抽查（UTF-8 无 BOM；`sed -i` 在 Windows Git Bash 可用，或用 Edit 逐个文件）。处理后核验：
运行：`go list ./... | grep -c '/test$'` → 预期 `0`（test/ 因无 tag 命中非 test 文件而从 workspace 包列表消失；`go list ./...` 从 46 减到 45）。
运行：`go test -count=1 ./test/...` → 预期 `[no test files]` 或等价跳过，**不构建任何二进制**。
运行：`go vet -tags=e2e ./test/...` → 预期干净（tag 下可编译）。

- [ ] **步骤 2：Makefile 新增 `test-e2e` target**

紧邻 `test-all` target 附近插入：

```make
# 真二进制端到端测试：构建 sproxy/sclient 真实二进制 + 子进程启动，覆盖文件面/隧道/
# mesh/relay/quota 等完整链路。默认 make test 不含（build-tag e2e 门控），CI e2e job 调用。
.PHONY: test-e2e
test-e2e: prepare
	$(GO) test $(GORACE) $(GOTEST_COUNT) -timeout=20m -tags=e2e ./test/...
```

（`$(GO)` 含 `GOOS/GOARCH` 环境前缀，与 `make test` 一致；`test/` 在根 module 内，无需 cd。）
确认 `test` / `test-ci` / `check-ci` **不**新增 `-tags=e2e`——保持默认路径摘除重型 e2e 的意图。

- [ ] **步骤 3：ci.yml 新增 `e2e` job（守护归位后的门禁不丢失）**

在 `test-submodules` job 之后插入：

```yaml
  e2e:
    name: E2E (real binaries)
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0 # v7.0.0
      - uses: actions/setup-go@b7ad1dad31e06c5925ef5d2fc7ad053ef454303e # v6
        with:
          go-version: "1.26"
          check-latest: true
      - name: prepare (buildmeta embed)
        run: make prepare
      - name: go test e2e (real binaries)
        run: make test-e2e
```

要点：**同一 PR 内 tag 化与 e2e job 必须原子落地**——否则 master 会短暂失去该门禁。`make test-e2e` 超时 20m 放足（现 4041 行真二进制套件，`-race` 下更久）。

- [ ] **步骤 4：本地双路径验证 + 文档同步**

运行：`make test`（无 tag）→ 预期：根单测绿、明显变快（不再现场构建 sproxy 二进制跑 4041 行 e2e）、无 test/ 参与。
运行：`make test-e2e` → 预期：tag 下全量真二进制 e2e 绿（与本 PR 前 `make test` 中它们的行为等价，必须 1:1 不丢场景）。
运行：`make lint`（主 module）→ 0 issues。
文档：`CLAUDE.md`「常用命令」补 `make test-e2e` 一行（真二进制端到端，CI e2e job），并在「测试规范」注明 `test/` 由 `//go:build e2e` 门控、默认单测不含。
`check-ci` target 是否纳入 `test-e2e`：**不纳入**（重型，本地 check-ci 保持快）；门禁由 CI e2e job 承担。

- [ ] **步骤 5：push → 开 PR → CI 全绿**

`git push -u origin <branch>`，`gh pr create`（title：`test: test/e2e 真二进制套件 build-tag 归位 + make test-e2e + CI e2e job`）。
预期：`E2E (real binaries)` job 与本 PR 引入的 `Test Sub-Modules` job 均绿；`Test`/`Test Windows` job 因摘除重型 e2e 显著变快且仍绿。
注意两个任务可以合成**一个 PR**（同一分支顺序落地任务 1 再任务 2），也可拆两 PR；按实施节奏自决，但每 PR 独立可合并、CI 全绿。

---

## 自检

1. **覆盖度：** (a) cmd/sclient+cmd/sproxy（54+6 测试文件，唯一真正零覆盖面）→ 任务 1；(b) 其余 8 个 ext/hub/mesh 子 module 同样零 CI → 任务 1 一并接入（`test-all` 天然含）；(c) web/e2e 已有独立 ui-e2e job，**不**动其排除状态；(d) `test/` 重型 e2e 无条件吞入 `make test` → 任务 2 归位。全规格闭环。
2. **占位符扫描：** 无 TODO/待定；每步含精确命令与预期。
3. **类型/命令一致性：** `make test-all`、`make test-e2e`、job 名 `test-submodules`/`e2e` 在 Makefile 与 ci.yml 间逐一对应；`$(GO)`/`$(RAW_GO)` 沿用现有变量。
4. **风险注记：** 10 个子 module 测试是首次受 CI 门控，任务 1 步骤 1 强制先本地修绿再上 CI，防止把既有花测试放进门禁制造首个红 master；任务 2 的 tag 与 e2e job 同 PR 原子落地防门禁真空。

## 执行交接

计划已完成并保存到 `docs/superpowers/plans/2026-09-09-prf1-ci-wiring.md`。执行方式：**子代理驱动（SDD）**——每任务全新 implementer + 独立 reviewer，CI 全绿后自动 squash 合并。
