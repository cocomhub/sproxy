# sproxy 开源库基线标准化 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 把仓内已确认的遗留问题（死代码、重复实现、散落的测试工具）规范化，建立 CHANGELOG 单源与嵌套模块 tag 规则，并接入 release-please 发布自动化（方案 B）。

**架构：** 分三相：① 代码规范化（删死代码 / 统一重复实现 / 测试工具归位 / 加防复发门禁）；② CHANGELOG 与 tag 规范化（AI 生成的 CHANGELOG 校对 + 回溯 tag + 嵌套模块 tag）；③ 发布自动化（release-please 管 CHANGELOG 与 tag，GoReleaser 只构建与产物）。

**技术栈：** Go 1.26；纯标准库；release-please（GitHub Action）；GoReleaser v2；`golang.org/x/tools/cmd/deadcode`（Go tool 指令）。

**规格：** `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md`（本文档所实现的规格；执行者两份都读）

---

## 全局约束

- 每个任务一个分支/PR 的切片，从最新 `master` 切出；本系列已在 `chore/oss-baseline` 上进行。
- **PR 不必拆太细（用户 2026-09-14 决策）**：相关任务合并在同一 PR。本轮切两片：
  - **PR-1｜开源库基线规范化**：任务 0 + 任务 1–5（死代码、重复实现、测试工具归位、门禁、文档）。
  - **PR-2｜发布机制标准化**：任务 6–8（CHANGELOG 校对、嵌套模块 tag、release-please）。
- **`cmd/` 保持薄（用户 2026-09-14 明示）**：`cmd/**` 只做参数解析、装配与输出；任何可复用逻辑/
  领域能力必须放到对应领域 `pkg/<domain>` 包，不得在 `cmd/` 下堆积实现。涉及 `cmd/` 的重构一律
  朝「薄适配 → 领域包」方向做。
- **CHANGELOG 硬规则（用户 2026-09-14 明示）**：**每次 commit / 开 PR 前必须先分析「本次改动是否需要
  同步 CHANGELOG」**——需要就改，不需要就在 PR 描述写明理由。**CHANGELOG 按功能维度管理**：按面向用户
  的能力组织条目，不按提交/PR 数量堆砌；删除对外 API 必须落 `### Removed`。本规则自身也要落库（见任务 6 步骤 0）。
- **只删有证据的代码**：删除前必须在任务描述中给出「零生产引用」的取证命令与输出；不确定者不删，写进报告。
- 源码带 SPDX 头；注释用简体中文；注释必须与实测一致。
- 测试纯标准库（`t.Fatalf`/`t.Errorf`）；只绑 `127.0.0.1`。
- 每次 Bash 调用在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`。
- `go build ./...` 只覆盖根 module；必须另跑 `make build-all`。
- lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues。
- 提交时只 `git add` 本任务改动的文件；message 用多重 `-m`；**不加署名行**。
- **不要单独开纯文档 PR**：文档任务必须与至少一个代码任务在同一 PR。
- 删除动作（文件/符号）在提交前需在任务报告中列出清单；`git tag` 推送前需最终人工确认（见任务 9）。

## 四条机械核对（每片必跑）

```bash
git diff --stat db99de06 -- test/          # ① 应为空（沿用既有披露）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)   # ② 无 "-" 行
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)   # ③ 应为空
go test ./internal/archcheck/               # ④
```

## 验证命令（每片必跑）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ internal/ cmd/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
make test-e2e
```

---

## 文件结构

| 文件 | 职责 | 动作 |
|------|------|------|
| `internal/archcheck/dead_symbols_test.go` | 墓碑门禁：已删除符号不得复活 | 创建 |
| `Makefile` | 新增 `deadcode` target 并接入 `check-ci` | 修改 |
| `go.mod` | **不改**（用固定版本 `go run pkg@version`，避免 `go get -tool` 连带升级生产依赖） | 不改 |
| `cmd/sclient/archive.go` | 删 `writeArchiveResponse` | 修改 |
| `cmd/sclient/batch.go` | 删 `runBatchOperation` | 修改 |
| `cmd/sclient/cloud_download.go` | 删 `extractTarGz` | 修改 |
| `cmd/sproxy/mesh_node.go` | 删 `startMeshNodeRole` 包装 | 修改 |
| `pkg/server/handlers.go` | 删 `TunnelUpdater` / `TunnelHandler()`，修正注释 | 修改 |
| `pkg/tunnel/xfer/ext/grpc/grpc.go` | 删未实现断言的空接口 `XferServer` | 修改 |
| `pkg/tunnel/handler_client.go` | 删 `NewHandler`，统一到 `NewLocalHandler` | 修改 |
| `cmd/sclient/internal/clientfactory/mock.go` | 从 `factory.go` 拆出测试 mock | 创建 |
| `pkg/files/testutil.go` | 归位 `MustNewUploadStore` 等测试 helper | 创建 |
| `CHANGELOG.md` | 校对版本/日期/链接 | 修改 |
| `scripts/tag-release.sh` | 按 CHANGELOG 生成根 + 嵌套模块 tag | 创建 |
| `release-please-config.json` / `.release-please-manifest.json` | release-please 配置 | 创建 |
| `.github/workflows/release-please.yml` | release-please Action | 创建 |
| `.goreleaser.yaml` | 去 `before.hooks` 改源码、`draft:false`、release notes 单源 | 修改 |

---

## 任务 0：GoReleaser CI 修复（**已完成**，用户 2026-09-14 追加）

**根因：** `.goreleaser.yaml` 使用已废止字段，`goreleaser release` 在 YAML 解析阶段即失败——
`v0.4.0`–`v0.11.0` 共 8 个 tag 的 Release workflow run 全部 `failure`。

**已交付（commit `0aa1d17b`）：**
- `nfpm` → `nfpms`（v2 段名）；`files` → `contents`（`src`/`dst`）；
- `archives.builds` → `ids`；`archives.format_overrides.format` → `formats`；
- `dockers` → `dockers_v2`（复用预编译二进制，消除 deprecation）；`Dockerfile` 改为拷贝产物；
- 移除 `before.hooks` 的 `go mod tidy` / `go fmt`（发布期修改源码）；`.gitignore` 忽略 `dist/`。

**验证：** `goreleaser check` 通过；`goreleaser release --snapshot --clean --skip=publish --skip=docker`
全绿（12 平台构建 + deb/rpm + checksums）。

**遗留（归入任务 7）：** `release.draft` 与「release-please 建 Release vs GoReleaser 建 Release」的
单写者归属；历史 tag 不回溯补 Release（重跑旧 run 会检出旧 tag 的旧配置，无意义）。

---

## 任务 1：建立死代码防复发门禁（红 → 绿）

**文件：**
- 创建：`internal/archcheck/dead_symbols_test.go`
- 修改：`Makefile`、`go.mod`

- [ ] **步骤 1：编写失败的墓碑门禁**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os/exec"
	"strings"
	"testing"
)

// deadSymbols 是已确认删除的历史遗留符号。它们不得在任何非测试源码中复活：
// 每个条目都对应一次有 git 取证的清理（见 docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md §2.1）。
var deadSymbols = []string{
	"writeArchiveResponse",
	"runBatchOperation",
	"extractTarGz",
	"startMeshNodeRole",
	"TunnelUpdater",
	"XferServer",
}

func TestNoResurrectedDeadSymbols(t *testing.T) {
	for _, sym := range deadSymbols {
		out, err := exec.Command("git", "grep", "-nF", sym, "--", "*.go", ":(exclude)*_test.go").Output()
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			if !ok || exitErr.ExitCode() != 1 {
				t.Fatalf("git grep %s: %v", sym, err)
			}
			continue // ExitCode 1 = 无匹配 = 期望结果
		}
		t.Errorf("已删除符号 %q 复活：\n%s", sym, strings.TrimSpace(string(out)))
	}
}
```

- [ ] **步骤 2：运行门禁验证失败（红）**

运行：`cd /d/workdir/leon/cocomhub/sproxy && go test -count=1 -run TestNoResurrectedDeadSymbols ./internal/archcheck/`
预期：FAIL，报出 6 个符号各自的定义位置（此时它们都还在）。

- [ ] **步骤 3：新增 `make deadcode` 并接入 `check-ci`**

在 `Makefile` 的 `.PHONY` 列表补 `deadcode`，新增（**实际落地形式**，见裁决：`go get -tool` 会连带升级 `x/crypto` 等生产依赖，故改用固定版本 `go run`，`go.mod` 零改动）：

```make
DEADCODE_TOOL ?= golang.org/x/tools/cmd/deadcode@v0.47.0

deadcode: ## 检测从 main 不可达的函数（信息输出，不作为失败条件）
	@echo "==> deadcode (cmd/sproxy cmd/sclient)"
	go run $(DEADCODE_TOOL) ./cmd/sproxy ./cmd/sclient
```

并在 `check-ci` 依赖链中调用 `deadcode`（放在 `archcheck` 之后）。**裁决（见账本）：`deadcode` 只作信息输出，不得成为 `check-ci` 的失败条件**——不带 `-test` 时它会把仅被测试引用的 helper（`NewMock`/`DiscardLogger`/`SetHostOnly` 等）全报为不可达，输出永不为空。

- [ ] **步骤 4：运行 `make deadcode` 记录当前基线（预期非空）**

运行：`make deadcode`
预期：输出 `startMeshNodeRole`、`writeArchiveResponse`、`runBatchOperation`、`extractTarGz`、`NewMock` 等——这些就是后续任务要清理的目标。

- [ ] **步骤 5：Commit**

```bash
git add internal/archcheck/dead_symbols_test.go Makefile
# 注：go.mod/go.sum 不改（固定版本 go run 方案）
git commit -m "test(archcheck): 死代码墓碑门禁（R11）+ make deadcode" -m "先红后绿：门禁在删除前点名 6 个遗留符号；deadcode 仅信息输出。"
```

> 实际落地见 `internal/archcheck/dead_symbols_test.go`（`git grep -nw` + 显式 `cmd.Dir=repoRoot`，后者防 cwd 落在包内导致假绿），
> 以及 `Makefile` 的 `DEADCODE_TOOL ?= golang.org/x/tools/cmd/deadcode@v0.47.0`。

---

## 任务 2：删除 A 类替代遗留（4 处）

**文件：**
- 修改：`cmd/sclient/archive.go`、`cmd/sclient/batch.go`、`cmd/sclient/cloud_download.go`、`cmd/sproxy/mesh_node.go`
- 测试：`cmd/sclient/cmd_test.go`、`cmd/sclient/batch_test.go`、`cmd/sclient/cloud_download_test.go`、`cmd/sproxy/mesh_node_test.go`

- [ ] **步骤 1：取证零生产引用**

运行：
```bash
for s in writeArchiveResponse runBatchOperation extractTarGz startMeshNodeRole; do
  echo "--- $s"; grep -rn "$s" --include="*.go" cmd/ pkg/ | grep -v "_test.go" | grep -v "func .*$s"
done
```
预期：仅 `startMeshNodeRole` 有 1 行（`return startMeshNodeRoleWithCreds(...)`，即函数自身），其余三处输出为空。

- [ ] **步骤 2：删除函数并清理 import**

1. `cmd/sclient/archive.go`：删 `writeArchiveResponse` 及其上注释；若 `net/http`/`io`/`os` 不再被使用则从 import 块删除。
2. `cmd/sclient/batch.go`：删 `runBatchOperation` 及其注释（保留 `batchOperationResult`/`printBatchResults`/`countBatchSuccess`）。
3. `cmd/sclient/cloud_download.go`：删 `extractTarGz` 及其注释；按需清理 `archive/tar`、`compress/gzip`、`path/filepath`、`strings` import。
4. `cmd/sproxy/mesh_node.go`：删 `startMeshNodeRole`，把注释合并到 `startMeshNodeRoleWithCreds` 的文档。

- [ ] **步骤 3：调整测试**

1. `cmd/sclient/cmd_test.go`：删 `// ---- writeArchiveResponse ----` 段及其测试函数。
2. `cmd/sclient/batch_test.go`：删仅测 `runBatchOperation` 的用例；确认 `printBatchResults`/`countBatchSuccess` 的用例保留。
3. `cmd/sclient/cloud_download_test.go`：删 `TestExtractTarGz_*` 全部用例（含 `TestExtractTarGz_PathTraversalPrevented`）。
4. `cmd/sproxy/mesh_node_test.go`：把 `startMeshNodeRole(...)` 调用改为 `startMeshNodeRoleWithCreds(..., nil, ...)`。

> 记录（诚实披露）：`extractTarGz` 的路径穿越防护测试随之删除。该能力当前由 `download-archive` 下载原始归档取代（`cmd/sclient/cloud_download.go:474`），不再有本地解压路径；如未来恢复本地解压，必须复用 `pkg/pathguard` 校验并重加该回归测试。此说明写入任务报告。

- [ ] **步骤 4：运行验证**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l cmd/ && go build ./... && make build-all
go test -count=1 ./cmd/sclient/ ./cmd/sproxy/
go test -count=1 -run TestNoResurrectedDeadSymbols ./internal/archcheck/
```
预期：全部 PASS；墓碑门禁剩余 2 个符号（`TunnelUpdater`、`XferServer`）仍红（任务 3 处理）。

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/archive.go cmd/sclient/batch.go cmd/sclient/cloud_download.go cmd/sproxy/mesh_node.go \
        cmd/sclient/cmd_test.go cmd/sclient/batch_test.go cmd/sclient/cloud_download_test.go cmd/sproxy/mesh_node_test.go
git commit -m "refactor(sclient,sproxy): 删除替代后遗留的 4 个无生产调用函数" \
  -m "writeArchiveResponse/runBatchOperation/extractTarGz 的调用已被 svc.Archive、内联循环、download-archive 取代；startMeshNodeRole 自 S5 起只被测试使用（root.go 走 WithCreds 版本）。"
```

---

## 任务 3：清理 B 类陈旧死类型

**文件：**
- 修改：`pkg/server/handlers.go`、`pkg/tunnel/xfer/ext/grpc/grpc.go`
- 测试：`pkg/server/server_handler_gaps_test.go`

- [ ] **步骤 1：取证**

运行：
```bash
grep -rn "TunnelUpdater\|UpdateKey\|\.TunnelHandler()" --include="*.go" cmd/ pkg/ | grep -v "_test.go"
grep -rn "XferServer" --include="*.go" pkg/ | grep -v "_test.go"
```
预期：`TunnelUpdater`/`UpdateKey`/`TunnelHandler()` 仅出现在定义与注释；`XferServer` 仅定义，无实现断言。

- [ ] **步骤 2：删除并修正注释**

1. `pkg/server/handlers.go`：删 `TunnelUpdater` 接口定义与 `TunnelHandler()` 方法；修正 `localHandler` 字段附近提到"与 TunnelHandler() 互补"的注释（改为描述 `POST /tunnel` 路由使用 `h.tunnelHandler` 字段）。
2. `pkg/tunnel/xfer/ext/grpc/grpc.go`：删 `XferServer` 接口定义（若注释指出它未被使用，改为说明由生成代码的 `Xfer_StreamServer` 承担）。

- [ ] **步骤 3：调整测试**

`pkg/server/server_handler_gaps_test.go`：删除 `TestTunnelHandler_ReturnsHandler`；用路由行为替代——构造 handler 后 `POST /tunnel` 无凭据应 401（若既有测试已覆盖，则直接删该用例并说明）。

- [ ] **步骤 4：运行验证**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ && go build ./... && make build-all
go test -count=1 ./pkg/server/ ./pkg/tunnel/xfer/ext/grpc/
go test -count=1 -run TestNoResurrectedDeadSymbols ./internal/archcheck/
make deadcode
```
预期：全 PASS；墓碑门禁全绿；`make deadcode` **输出非空属正常**（仅被测试引用的 helper 恒被报为不可达），它只作信息输出，真门禁是 R11。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/handlers.go pkg/tunnel/xfer/ext/grpc/grpc.go pkg/server/server_handler_gaps_test.go
git commit -m "refactor(server,grpc): 删除已废止的 TunnelUpdater/TunnelHandler 与空接口 XferServer" \
  -m "tunnel_key 已废除，handleSighup 不再热替换密钥，UpdateKey 全仓零调用；注释同步订正。"
```

---

## 任务 4：重复实现统一

**文件：**
- 修改：`pkg/tunnel/handler_client.go`
- 测试：`pkg/tunnel/example_test.go`、`pkg/tunnel/tunnel_test.go`

- [ ] **步骤 1：`tunnel.NewHandler` 统一到 `NewLocalHandler`**

取证：
```bash
grep -rn "tunnel\.NewHandler(\|NewHandler(nil" --include="*.go" . | grep -v "_test.go"
```
预期：无生产调用（仅 `example_test.go` 等测试）。删除 `NewHandler`，其唯一差异是"不支持本地路由"，`NewLocalHandler(key, nil, logger)` 是其超集。文档与示例改用 `NewLocalHandler(nil, nil, logger)`。

- [ ] **步骤 2：`tunnel.AccessKeyMesh` 保持（已统一为薄委托）**

确认 `pkg/tunnel/tunnel.go:139` 为 `func AccessKeyMesh(ak string) string { return accesskey.ParseMesh(ak) }`——已是单一事实源，无重复逻辑；仅在其 GoDoc 中保留"薄委托"说明，**不改动**。

- [ ] **步骤 3：`hub.NewFederationClient` / `hub.ParseRegisterAck` / `client.CloudCreateGroup` / `client.CloudListGroups` / `DownloadItemsSequential` / `WithStructCodec` / `WithOffset` / `ResetRunners` 保持**

这些是公开 SDK 便捷入口，生产走更精确变体。**本轮不动**；在任务报告中列出，作为「有意保留」记录。

- [ ] **步骤 4：运行验证**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ && go build ./... && make build-all
go test -count=1 ./pkg/tunnel/... 
make deadcode
```

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/handler_client.go pkg/tunnel/example_test.go pkg/tunnel/tunnel_test.go
git commit -m "refactor(tunnel): 删除被 NewLocalHandler 超集取代的 NewHandler"
```

---

## 任务 5：测试工具归位

**文件：**
- 创建：`cmd/sclient/internal/clientfactory/mock.go`、`pkg/files/testutil.go`
- 修改：`cmd/sclient/internal/clientfactory/factory.go`、`pkg/files/chunked_store.go`

- [ ] **步骤 1：`clientfactory` 的 mock 拆出独立文件**

把 `factory.go:347-363` 的 `mockFactory` 结构体、`NewClient` 方法、`NewMock` 构造与 `var _ Factory` 断言整体移到新文件 `cmd/sclient/internal/clientfactory/mock.go`，文件头注释说明「仅供测试构造，不参与生产路径」。**同包拆文件，零 import 变更**（26 个测试文件不受影响）。

- [ ] **步骤 2：`pkg/files` 测试 helper 归位**

把 `chunked_store.go:283` 的 `MustNewUploadStore` 移到新文件 `pkg/files/testutil.go`，GoDoc 明确「跨包测试使用（`pkg/files` 与 `pkg/server`），必须导出」。同文件集中后续同类 helper。

- [ ] **步骤 3：登记测试专用接缝（不迁移）**

在任务报告中登记这些「生产代码中的测试接缝」，本轮保留：`SetHostOnly`、`SetMDNSLoopbackOnly`、`SetRejectPrivateRemoteCandidates`、`SetCandidatesForTest`、`SetClock`、`SetTTL`。理由：跨 module/跨包测试需要导出；`SetCandidatesForTest` 名字已自述。

- [ ] **步骤 4：运行验证**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l cmd/ pkg/ && go build ./... && make build-all
go test -count=1 ./cmd/sclient/... ./pkg/files/ ./pkg/server/
make deadcode
```
预期：全 PASS；`make deadcode` 非空属正常（只作信息输出，真门禁是 R11）。

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/internal/clientfactory/mock.go cmd/sclient/internal/clientfactory/factory.go \
        pkg/files/testutil.go pkg/files/chunked_store.go
git commit -m "refactor(test): 测试 mock 与 helper 归位独立文件" \
  -m "clientfactory mock 拆到 mock.go；pkg/files 跨包测试 helper 集中到 testutil.go。"
```

---

## 任务 6：CHANGELOG 校对与规范化（按功能维度）+ 规则落库

**文件：**
- 修改：`CHANGELOG.md`
- 修改：`docs/superpowers/learnings/2026-09-13-agent-operating-rules.md`（§1 硬规则）
- 修改：`AGENTS.md`、`CLAUDE.md`（硬规则同步）

- [ ] **步骤 0：把「CHANGELOG 硬规则」写进规则文档**
  在 `docs/superpowers/learnings/2026-09-13-agent-operating-rules.md` §1 新增一条：**每次 commit / 开 PR 前必须
  判断是否需同步 CHANGELOG；CHANGELOG 按功能维度（面向用户的能力）管理，不按提交/PR 堆砌；删除对外 API
  必须落 `### Removed`**；并在 `AGENTS.md`/`CLAUDE.md` 的硬规则清单同步同款（两文件逐字一致）。

- [ ] **步骤 1：逐版本核对日期与提交范围**

对每个版本，用 `git log --until="<版本日期> 23:59:59" --format="%h %ci %s" -1` 取边界提交，记为 tag 目标：

```bash
for d in 2026-06-01 2026-06-02 2026-06-04 2026-06-06 2026-06-13 2026-07-01 \
         2026-08-02 2026-08-21 2026-09-02 2026-09-09 2026-09-14; do
  printf "%s -> %s\n" "$d" "$(git log --until="$d 23:59:59" --format='%h %ci %s' -1)"
done
```
把结果写入临时记录（任务 9 使用），并确认与 CHANGELOG 版本日期一一对应；不一致则改 CHANGELOG 日期。

- [ ] **步骤 2：修 `Unreleased` 与链接**

1. `[Unreleased]` 段**保留 PR #249 新增的 `### Removed`**（不得改回“暂无未发布变更”）；并按功能维度归并条目。release-please 不维护该段，合并 release PR 前需人工并入/清空。
2. 文末 compare 链接与版本号严格对应；确认 `[0.1.0]` 使用 `/releases/tag/` 形式。
3. 顶部说明中的 `0.1.0–0.11.0 的版本 tag 按提交时间线回溯建立` 保留，作为任务 9 的依据。

- [ ] **步骤 3：验证**

运行：`grep -n "^## \[" CHANGELOG.md` 与 `grep -c "^\[0\." CHANGELOG.md`
预期：版本节与链接数量一致（12 个版本 + Unreleased）。

- [ ] **步骤 4：Commit**

```bash
git add CHANGELOG.md
git commit -m "docs(changelog): 校对回溯版本日期与 compare 链接" -m "AI 汇总稿的规范化收口。"
```

---

## 任务 7：发布自动化（release-please，方案 B）

**文件：**
- 创建：`release-please-config.json`、`.release-please-manifest.json`、`.github/workflows/release-please.yml`
- 修改：`.goreleaser.yaml`

- [ ] **步骤 1：配置 release-please（根本 module，release-type go）**

`release-please-config.json`：

```json
{
  "release-type": "go",
  "packages": {
    ".": {
      "release-type": "go",
      "component": "sproxy",
      "changelog-path": "CHANGELOG.md",
      "include-component-in-tag": false
    }
  },
  "separate-pull-requests": false,
  "include-v-in-tag": true
}
```

`.release-please-manifest.json`：

```json
{ ".": "0.11.0" }
```

- [ ] **步骤 2：新增 Action**

`.github/workflows/release-please.yml`：

```yaml
name: Release Please
on:
  push:
    branches: [master]
permissions:
  contents: write
  pull-requests: write
jobs:
  release-please:
    runs-on: ubuntu-latest
    steps:
      - uses: googleapis/release-please-action@v4
        with:
          config-file: release-please-config.json
          manifest-file: .release-please-manifest.json
```

- [ ] **步骤 3：调整 GoReleaser（去掉改源码的 hooks，发布改为非草稿）**

`.goreleaser.yaml`：
- 删除 `before.hooks` 中的 `go mod tidy` 与 `go fmt ./...`（改为 `goreleaser check` 作为本地校验，不写入配置）。
- `release.draft: true` → `false`。
- 删除 `release.header` 中失效的 `go install ...@{{ .Tag }}` 指令（嵌套 module 带相对 `replace`，代理安装不可用）；改为「下载预编译二进制」为唯一官方安装方式。
- `changelog` 段设为 `disable: true`（Release 正文归 release-please）；并设 `release.mode: keep-existing`，使 GoReleaser 只上传制品、**不覆盖** release-please 写入的 notes。

- [ ] **步骤 4：本地干跑验证**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
goreleaser check
goreleaser release --snapshot --clean --skip=publish
```
预期：`check` 无错误；snapshot 在 `dist/` 产出 sproxy/sclient 全平台产物与 checksums，无 draft 相关报错。

- [ ] **步骤 5：Commit**

```bash
git add release-please-config.json .release-please-manifest.json .github/workflows/release-please.yml .goreleaser.yaml
git commit -m "ci(release): 接入 release-please 作为 CHANGELOG/版本单源（方案 B）" \
  -m "GoReleaser 去掉改源码的 before.hooks、发布改为非草稿、离线安装指令订正。"
```

---

## 任务 8：嵌套模块 tag（参考 CHANGELOG 生成）

**文件：**
- 创建：`scripts/tag-release.sh`
- 修改：`docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md`（补 tag 规则落地说明）

- [ ] **步骤 1：编写 tag 脚本（干跑优先）**

`scripts/tag-release.sh` 行为：

```bash
#!/usr/bin/env bash
# 用法：
#   scripts/tag-release.sh --dry-run          # 只打印将创建的 tag 与目标提交
#   scripts/tag-release.sh --apply            # 实际创建并推送（需人工确认）
#   scripts/tag-release.sh --version 0.11.0 --apply   # 单版本（release-please 已打根 tag 时）
set -euo pipefail
DRY=1
VERSION=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --dry-run) DRY=1; shift;;
    --apply) DRY=0; shift;;
    --version) VERSION="$2"; shift 2;;
    *) echo "unknown arg: $1" >&2; exit 2;;
  esac
done

# 版本 -> 日期 从 CHANGELOG 解析：## [X.Y.Z] - YYYY-MM-DD
versions=$(grep -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\] - [0-9]{4}-[0-9]{2}-[0-9]{2}' CHANGELOG.md \
  | sed -E 's/^## \[([^]]+)\] - (.*)$/\1 \2/' | sort -V)

tag_one() { # <tag> <commit>
  if git rev-parse -q --verify "refs/tags/$1" >/dev/null; then
    echo "SKIP  $1 (已存在)"; return 0
  fi
  echo "TAG   $1 -> $2"
  if [[ $DRY -eq 0 ]]; then git tag -a "$1" "$2" -m "Release $1"; fi
}

while read -r v d; do
  [[ -z "$v" ]] && continue
  [[ -n "$VERSION" && "$v" != "$VERSION" ]] && continue
  c=$(git log --until="$d 23:59:59" --format=%H -1)
  tag_one "v$v" "$c"
  tag_one "cmd/sproxy/v$v" "$c"
  tag_one "cmd/sclient/v$v" "$c"
done <<< "$versions"

# 实际实现：scripts/tag-release.sh 在 `--apply --push` 时逐条显式 refspec 推送本次新建的
# tag（绝不用 `git push --tags`），并在既有 tag 上 SKIP。此处不重复粘贴脚本正文。
```

- [ ] **步骤 2：干跑并核对**

```bash
chmod +x scripts/tag-release.sh
./scripts/tag-release.sh --dry-run | tee /tmp/tags.txt
wc -l /tmp/tags.txt
```
预期：33 行（11 个版本 × 3 个 tag）；`v0.3.0` 显示 SKIP（已存在）；每个版本三个 tag 指向同一提交。

- [ ] **步骤 3：人工确认（**不可逆**）**

把 `/tmp/tags.txt` 交给控制者确认后才执行 `--apply`。确认项：
1. 每个 tag 的目标提交是否为该版本的合理边界；
2. 是否接受向后回溯创建历史 tag（会进入 Go module proxy 与 GitHub Releases 链接）；
3. `cmd/*` 子模块带相对 `replace`，`go install ...@cmd/sproxy/vX.Y.Z` 仍不可用——本轮仅保证 tag/CHANGELOG 自洽，不承诺 `go install`。

- [ ] **步骤 4：执行并验证**

```bash
./scripts/tag-release.sh --apply
git tag -l | sort -V | head -40
git describe --tags --abbrev=0            # 应为 v0.11.0 或其后
```

- [ ] **步骤 5：Commit**

```bash
git add scripts/tag-release.sh docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md
git commit -m "chore(release): 按 CHANGELOG 生成根与嵌套模块 tag 的脚本" \
  -m "嵌套 module tag 形如 cmd/sproxy/vX.Y.Z（Go 官方规则）；支持 --dry-run 干跑。"
```

> 注：tag 创建/推送在 PR 合并后由控制者执行，不属于 PR 内容；脚本随 PR 交付。

---

## 任务 9：文档与全量回归

**文件：**
- 保持不变：`docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md`（规格）
- 新增：本计划文档

- [ ] **步骤 1：确认文档与代码同 PR**

本系列所有 `docs/**` 改动均与任务 1–8 的代码改动在同一 PR（遵守「不单独开纯文档 PR」）。确认 `paths-ignore` 不会让 CI 跳过：PR 至少含 `.go`/`Makefile`/`.yaml` 改动。

- [ ] **步骤 2：全量回归**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ internal/ cmd/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
make test-all
make check-ci
make test-e2e
make web-test
```

- [ ] **步骤 3：四条机械核对**

```bash
git diff --stat db99de06 -- test/
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)
go test -count=1 ./internal/archcheck/
```
预期：① 空；② 无 `-` 行（删除测试用例须在报告中逐条披露）；③ 空；④ 全绿。

- [ ] **步骤 4：Commit 并开 PR**

```bash
git add docs/superpowers/plans/2026-09-14-oss-baseline-standardization.md
git commit -m "docs(plan): 开源库基线标准化实施计划" -m "与死代码清理/发布自动化同 PR 交付。"
gh pr create --title "chore: 开源库基线标准化（死代码清理 + CHANGELOG/tag + release-please）" \
  --body-file /tmp/pr-body.md
```

---

## 收尾（全部任务后）

- [ ] 更新 `docs/superpowers/specs/2026-09-14-sproxy-next-roadmap.md` §2.1：把「待处置」改为「已处置」，附实际删除清单与保留清单。
- [ ] 在 learnings 记录本轮踩坑（如有），例如 `deadcode` 工具与 Go tool 指令在 workspace 下的行为、GoReleaser v2 字段迁移。
- [ ] PR-1 合并后再开 PR-2（每片从最新 `master` 切出）；等 CI 全绿（`total≥14 且 pending=0`）后合并；合并后删除远端与本地分支。
- [ ] 核对：`make deadcode` 只作信息输出（非空正常）；R11 墓碑门禁绿；根 tag 与 CHANGELOG 一致且 `cmd/*` 嵌套 tag 已补；`goreleaser check` 绿。

## 自检记录

**1. 规格覆盖度：** 规格 §2.1 死代码（任务 1–5）、§2.2 测试工具（任务 5）、§2.4 发布缺口（任务 6–8）、
§4 决策 6/7/8/9（任务 7/8 落实 B、不打 tag 之外不做供应链与分发）、§5.2 嵌套 tag 规则（任务 8）——
均有对应任务。

**2. 占位符扫描：** 无「待定/TODO/类似任务 N」；每个删除步骤均给出取证命令与预期输出。

**3. 类型一致性：** `deadSymbols` 列表（任务 1）与任务 2/3 的删除对象一一对应；`startMeshNodeRoleWithCreds`
在任务 2 的测试调整中与 `cmd/sproxy/mesh_node.go` 实际签名一致；`tag_one` 的三类 tag 与规格 §5.2 一致。
