# 文件服务子包抽取 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 `pkg/server` 的文件服务（list/stat/download/upload/delete/rename/mkdir/rmdir + 分块传输 + 校验和 + 版本）抽成领域包 `pkg/files`（含 `chunked`/`version` 子包），并把跨领域复用的抽象提升为顶级包（`pkg/pathguard`、`pkg/checksum`），全程**逐字不变**。

**架构：** 本计划是**纯搬迁**：文件按领域移动、包名与导入路径变更、`pkg/server` 侧留一行薄适配转发，**零逻辑变更**。新增 `internal/archcheck` 门禁把「分层方向」与「子包只被父域导入」变成可执行断言。

**技术栈：** Go 1.26；纯标准库（`log/slog`、`os/exec` 用于门禁）；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-12-file-service-extraction-design.md`

---

## 计划对规格的一处切分调整（已确认）

规格 §5 的 **A-3**（`pkg/storage/capacity` + `pkg/volume/registry`）在本计划中**拆成两个任务**：容量核算与卷装配**互不依赖**，各自可独立审查、独立回退。故本计划共 **9 个任务**（规格 8 片 → 计划 9 片），其余切分与规格一致。

## 全局约束

- **Go 1.26**；**纯标准库**；不新增任何依赖（含 `golang.org/x/`）。
- **逐字不变**：控制流、错误语义、HTTP 状态码、响应头、错误消息文案、分块语义（chunk 大小/偏移/校验）、配置默认值，**一律不得变化**。
- **不允许顺带的"小修小补"**——哪怕明显是缺陷。发现缺陷记入阶段 D 或独立议题。
- **包名按领域命名**；**类型名不改**（`ChecksumStore`/`UploadStore` 等保持原名，留到阶段 D）。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint-all`**（根 `golangci-lint run ./...` **不跨 module**，子模块有盲区），必须 0 issues。
- 提交时**只 `git add` 本任务改动的文件**（禁止 `git add -A`/`git add .`）；message 用**多重 `-m`**；**不加任何署名行**。
- 每次 Bash 调用都要在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`（shell 状态不跨调用保留；否则 `addlicense` 缺失导致提交被拒，且 pre-commit 的 lint 会因 `command -v` 守卫**静默跳过**）。
- 工作分支：`feature/files-domain-extraction`（规格提交 `db99de06`）。
- **archcheck 登记约定**：每新抽/新建一个包，都要**同时**写入 `Levels`（层级）与 `Managed`（本工作新增）；若是**子包**，另需写入 `ParentDomain`。只写 `Levels` 会让 R3 不作用于它，只写 `Managed` 会让 R1 看不见它的层级——两条都要写才有效。

## 四条机械核对（每片必跑）

抽取阶段的正确性靠这四条**可执行**核对，不靠"跑测试看看"：

```bash
# ① e2e 零改动（必须输出为空）
git diff --stat db99de06 -- test/

# ② 用例名零丢失（基线直接从 base commit 派生，不落任何产物；
#    输出必须**没有 "-" 行**——新增允许，丢失绝不允许）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)

# ③ 生产路由表逐字一致（只比非测试文件——测试里的路由注册不是产品契约）
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- 'pkg/server/*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- 'pkg/server/*.go' ':(exclude)*_test.go' | sort)

# ④ 分层与子包可见性门禁
go test -count=1 ./internal/archcheck/
```

**为什么基线从 git 派生而不是存一份**：`.superpowers/` 被 gitignore，存那里的基线换 clone/worktree 就没了；而**事后重采等于拿自己比自己，恒空、丧失证据力**。`git grep <base-commit>` 让基线**随时可从版本库重建**，且任何评审者/CI 都能复现，不必依赖控制者的机器。

**核对口径**：①②③ 均是「base commit vs **当前工作树**」，故可在提交**之前**跑（提交前就能发现问题）。② 的判据是**零丢失**，新增用例允许但必须在报告里说明来源。

**规则（规格 §6）**：**专属**于被搬代码的测试文件**随包迁移**；**混合**测试文件（同时覆盖被搬与未搬代码）**留在 `pkg/server`**，靠②保证用例名不丢。

---

## 文件结构

**新建包（按层级）**

| 包 | 层级 | 职责 | 来源 |
|---|---|---|---|
| `pkg/pathguard` | L0 | 路径安全校验（拒绝穿越/绝对路径/非法字符） | `pkg/server/validate.go` |
| `pkg/checksum` | L0 | 文件校验和台账（名→校验和，持久化） | `pkg/server/checksum_store.go` |
| `pkg/storage/capacity` | L2 | 容量与占用核算、配额对账 | `pkg/server/storage_manager.go` |
| `pkg/volume/registry` | L2 | 运行时卷集合装配与定位 | `pkg/server/volumes.go`（装配部分） |
| `pkg/files/chunked` | L3 | 分块上传会话与块传输 | `upload_store.go` + `chunked_upload.go` + `chunked_download.go` |
| `pkg/files/version` | L3 | 文件版本存储 | `pkg/server/version.go`（存储部分） |
| `pkg/files` | L4 | 文件服务根：Deps 接缝 + 路由 + 读写处理器 | `list_handler.go` 等 8 个文件 |

**新建门禁**

| 文件 | 职责 |
|---|---|
| `internal/archcheck/layers.go` | 层级表与子包归属表（**唯一事实源**，新增包必须登记） |
| `internal/archcheck/arch_test.go` | 解析导入图并断言三条规则 |

**修改**：`pkg/server/*.go` 的调用点（改导入 + 一行薄适配）。

---

### 任务 1：抽出 `pkg/pathguard` 并建立 `internal/archcheck` 门禁

**文件：**
- 创建：`pkg/pathguard/validate.go`（由 `pkg/server/validate.go` 迁入）
- 创建：`pkg/pathguard/validate_test.go`、`pkg/pathguard/validate_fuzz_test.go`（由 `pkg/server/` 迁入）
- 创建：`internal/archcheck/layers.go`、`internal/archcheck/arch_test.go`
- 修改：`pkg/server/{archive,checksum,chunked_download,chunked_upload,delete_handler,dirs,download_handler,list_handler,remote_read,rename_handler,share,upload_handler,version,volumes_api}.go`（14 个调用点）
- 修改：`Makefile`（`check-ci` 追加 `archcheck`）

- [ ] **步骤 1：确认基线锚点并自证（基线不落任何产物）**

后续 8 个任务都拿 `db99de06`（本分支起点，即抽取前的 master 状态）当基线，用 `git grep` 直接从它派生。
先自证这条命令在当前树上**恒等**——搬迁前 base 与工作树一致，两条 diff 必须都为空，这同时证明命令、pathspec 与 pattern 都正确：

```bash
git rev-parse db99de06
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- 'pkg/server/*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- 'pkg/server/*.go' ':(exclude)*_test.go' | sort)
```

预期：两条 diff 均**无输出**。**把两条命令各自输出的行数记进报告**——它们是后续各片的对照基准（后续只要 diff 无 `-` 行即守恒）。

**注意**：pathspec `':(exclude)*_test.go'` 的引号不可省，否则被 shell 解释。

- [ ] **步骤 2：迁移源码文件**

```bash
git mv pkg/server/validate.go pkg/pathguard/validate.go
sed -i 's/^package server$/package pathguard/' pkg/pathguard/validate.go
```

- [ ] **步骤 3：修可见性**

`ValidateFilePath` 已导出，无需改。检查未导出助手：

```bash
grep -rn "hasServiceInternalPrefix" pkg/server/ pkg/pathguard/
```

- 若**仅** `pkg/pathguard/validate.go` 内使用 → 保持未导出，无需改动。
- 若被 `pkg/server` 其他文件使用 → 导出为 `HasServiceInternalPrefix`，并同步改那些调用点。

- [ ] **步骤 4：改 `pkg/server` 的 14 个调用点**

对下列每个文件：加导入 `"github.com/cocomhub/sproxy/pkg/pathguard"`，把 `ValidateFilePath(` 改为 `pathguard.ValidateFilePath(`：

```
pkg/server/archive.go  pkg/server/checksum.go  pkg/server/chunked_download.go
pkg/server/chunked_upload.go  pkg/server/delete_handler.go  pkg/server/dirs.go
pkg/server/download_handler.go  pkg/server/list_handler.go  pkg/server/remote_read.go
pkg/server/rename_handler.go  pkg/server/share.go  pkg/server/upload_handler.go
pkg/server/version.go  pkg/server/volumes_api.go
```

- [ ] **步骤 5：迁移专属测试**

`validate_test.go`、`validate_fuzz_test.go` 只测 `ValidateFilePath`，属**专属** → 随包迁移：

```bash
git mv pkg/server/validate_test.go pkg/pathguard/validate_test.go
git mv pkg/server/validate_fuzz_test.go pkg/pathguard/validate_fuzz_test.go
sed -i 's/^package server$/package pathguard/' pkg/pathguard/validate_*_test.go
```

改完后编译若报未定义符号，说明该测试引用了 `pkg/server` 的非公开符号 → 该测试实为**混合**测试，`git mv` 回 `pkg/server`（并在报告中说明）。

- [ ] **步骤 6：建立 `internal/archcheck` 门禁**

创建 `internal/archcheck/layers.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package archcheck 是分层与包可见性的可执行门禁：把「文件服务抽取」设计
// （docs/superpowers/specs/2026-09-12-file-service-extraction-design.md §3.3）
// 中约定的层级关系变成断言，而不是文档里的一句话。
//
// 新增包必须登记进 Levels，否则校验失败——这是刻意的：门禁的价值就在于
// 强迫作者显式声明新包在依赖图中的位置。
package archcheck

// Managed 是本工作新增（抽取）的包。R3 只作用于它们——若把仓库存量包也纳入
// 「必须登记依赖」的范围，登记一个包就会拖出它整条子图（pkg/tunnel →
// xfer / mux / hub / …），门禁根本落不了地。
var Managed = map[string]bool{
	"github.com/cocomhub/sproxy/pkg/pathguard": true,
}

// Levels 是包 → 层级（数字越小越底层）。L(n) 不得导入 L(>n)。
// 表内既含 Managed 的新包，也含它们依赖的存量基础包——后者必须显式登记，
// 才能让「新包依赖了哪一层」有据可查。存量基础包统一记 L1：本工作不引入
// 它们之间的方向约束（它们彼此的历史依赖不在本计划范围内）。
var Levels = map[string]int{
	// 本工作新增
	"github.com/cocomhub/sproxy/pkg/pathguard": 0,
	"github.com/cocomhub/sproxy/pkg/checksum":  0,
	// 存量基础包（新包的依赖；pkg/tunnel 由 volumes.go 实测依赖）
	"github.com/cocomhub/sproxy/pkg/storage": 1,
	"github.com/cocomhub/sproxy/pkg/quota":   1,
	"github.com/cocomhub/sproxy/pkg/volume":  1,
	"github.com/cocomhub/sproxy/pkg/tunnel":  1,
}

// ParentDomain 声明子包 → 父域包。子包只允许父域子树与装配层导入。
// 装配层是必要例外：路由注册在 pkg/server，它必须引用子包的处理器。
// 随各片 PR 增量登记，例如 pkg/files/chunked → pkg/files。
var ParentDomain = map[string]string{}

// AssemblyPackages 是允许导入任意子包的装配层（前缀匹配）。
var AssemblyPackages = []string{
	"github.com/cocomhub/sproxy/pkg/server",
	"github.com/cocomhub/sproxy/pkg/client",
	"github.com/cocomhub/sproxy/cmd/",
}
```

创建 `internal/archcheck/arch_test.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

import (
	"os/exec"
	"strings"
	"testing"
)

const modulePrefix = "github.com/cocomhub/sproxy/"

// importGraph 调 `go list` 解析工作区导入图（直接导入，不含测试导入）。
// 用子进程而非 go/build：模块模式下的路径解析交给工具链，避免自己实现一套。
func importGraph(t *testing.T) map[string][]string {
	t.Helper()
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}}|{{join .Imports \" \"}}", "./...").Output()
	if err != nil {
		t.Fatalf("go list 失败（需要 go 在 PATH 中）: %v", err)
	}
	graph := map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		pkg := parts[0]
		if len(parts) == 1 || strings.TrimSpace(parts[1]) == "" {
			graph[pkg] = nil
			continue
		}
		graph[pkg] = strings.Fields(parts[1])
	}
	return graph
}

// TestLayeringDirection 断言 R1：已登记包不得导入层级更高的已登记包。
func TestLayeringDirection(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		own, ok := Levels[pkg]
		if !ok {
			continue
		}
		for _, imp := range imports {
			other, ok := Levels[imp]
			if !ok {
				continue
			}
			if other > own {
				t.Errorf("分层违规：%s(L%d) 导入了 %s(L%d)，低层不得导入高层", pkg, own, imp, other)
			}
		}
	}
}

// isInSubtree 报告 pkg 是否等于 root 或位于 root 子树内。
func isInSubtree(pkg, root string) bool {
	return pkg == root || strings.HasPrefix(pkg, root+"/")
}

// isAssembly 报告 pkg 是否属于装配层（前缀匹配 AssemblyPackages）。
func isAssembly(pkg string) bool {
	for _, a := range AssemblyPackages {
		if isInSubtree(pkg, strings.TrimSuffix(a, "/")) {
			return true
		}
	}
	return false
}

// TestSubpackageVisibility 断言 R2：子包只允许父域子树与装配层导入。
func TestSubpackageVisibility(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		for _, imp := range imports {
			parent, ok := ParentDomain[imp]
			if !ok {
				continue
			}
			if isInSubtree(pkg, parent) || isAssembly(pkg) {
				continue
			}
			t.Errorf("子包可见性违规：%s 导入了子包 %s，但只有父域 %s 子树与装配层允许导入", pkg, imp, parent)
		}
	}
}

// TestManagedDependenciesRegistered 断言 R3：Managed 包不得导入 pkg/ 下未登记的包。
// 未登记的 pkg/ 包意味着依赖图里出现了一个没声明层级的节点；特别地，它也会拦下
// 「新包反向导入 pkg/server」——那正是本工作最需要防的成环方向。
func TestManagedDependenciesRegistered(t *testing.T) {
	graph := importGraph(t)
	for pkg, imports := range graph {
		if !Managed[pkg] {
			continue
		}
		for _, imp := range imports {
			if !strings.HasPrefix(imp, modulePrefix+"pkg/") {
				continue
			}
			if _, ok := Levels[imp]; !ok {
				t.Errorf("未登记依赖：Managed 包 %s 导入了 %s，它未在 Levels 登记。"+
					"若它是本工作的新包，同时加入 Managed 与 Levels；若是存量包，登记为 L1。", pkg, imp)
			}
		}
	}
}
```

- [ ] **步骤 7：把门禁并入 `check-ci`**

在 `Makefile` 的 `check-ci` 依赖列表中追加 `archcheck`，并新增目标：

```make
.PHONY: archcheck
archcheck:
	$(RAW_GO) test -count=1 ./internal/archcheck/
```

（`RAW_GO` 是 Makefile 既有变量；若不存在则用 `go`。改 Makefile 用 Read + Edit，**不要**用 sed。）

- [ ] **步骤 8：跑四条机械核对**

逐条执行本文档「四条机械核对」的 ①②③④ 命令。预期：① 输出为空；② ③ diff 无差异；④ PASS。

- [ ] **步骤 9：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/pathguard/ ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/pathguard internal/archcheck
```

预期：全绿；`lint-all` 0 issues；`gofmt -l` 空。

- [ ] **步骤 10：Commit**

```bash
git add pkg/pathguard pkg/server internal/archcheck Makefile
git commit -m "refactor(pathguard): 路径安全校验抽为顶级包 + 分层门禁" \
  -m "pkg/server/validate.go 迁入 pkg/pathguard（14 个调用点改导入）；\n新增 internal/archcheck 把分层方向与子包可见性变成可执行断言。\n逐字不变：仅搬迁与导入路径变更。" \
  -m "本片为文件服务抽取的第 1 片，规格见 docs/superpowers/specs/2026-09-12-file-service-extraction-design.md"
```

---

### 任务 2：抽出 `pkg/checksum`（校验和台账）

**文件：**
- 创建：`pkg/checksum/store.go`（由 `pkg/server/checksum_store.go` 迁入）
- 修改：`pkg/server/{chunked_download,chunked_upload,cloud_download,delete_handler,dirs,download_handler,handlers,list_handler,rename_handler,storage_manager,upload_handler,version}.go`（12 个调用点）
- 修改：`internal/archcheck/layers.go`（登记 `pkg/checksum`）

**范围裁定**（不在本片做，记录在案）：`pkg/server/checksum.go`（校验和**计算**）**本片不搬**——它是否同属本领域、是否应并入 `pkg/checksum`，留到阶段 D 评估。

- [ ] **步骤 1：迁移源码文件**

```bash
git mv pkg/server/checksum_store.go pkg/checksum/store.go
sed -i 's/^package server$/package checksum/' pkg/checksum/store.go
```

- [ ] **步骤 2：修可见性**

`ChecksumStore`、`ChecksumStoreIface`、`NewChecksumStore` 均已导出，无需改。检查未导出符号：

```bash
grep -rn "checksumStoreIface\|checksumStore\b" pkg/server/ | grep -v "_test.go" | head
```

仅 `store.go` 内使用的一律保持未导出。

- [ ] **步骤 3：改 `pkg/server` 的 12 个调用点**

对下列每个文件：加导入 `"github.com/cocomhub/sproxy/pkg/checksum"`，把 `ChecksumStore` → `checksum.ChecksumStore`、`NewChecksumStore` → `checksum.NewChecksumStore`、`ChecksumStoreIface` → `checksum.ChecksumStoreIface`：

```
pkg/server/chunked_download.go  pkg/server/chunked_upload.go  pkg/server/cloud_download.go
pkg/server/delete_handler.go  pkg/server/dirs.go  pkg/server/download_handler.go
pkg/server/handlers.go  pkg/server/list_handler.go  pkg/server/rename_handler.go
pkg/server/storage_manager.go  pkg/server/upload_handler.go  pkg/server/version.go
```

- [ ] **步骤 4：迁移测试**

`pkg/server` 无 `checksum_store*_test.go`（覆盖弥散在 `handlers_test.go`/`integration_test.go` 等**混合**测试中）→ **本片不迁移任何测试文件**，混合测试留在 `pkg/server` 并必须继续通过。用机械核对 ② 证明用例名未丢。

- [ ] **步骤 5：登记层级（`Levels` 与 `Managed` 两张表都要写）**

`internal/archcheck/layers.go`：

```go
// Managed
	"github.com/cocomhub/sproxy/pkg/checksum": true,
// Levels
	"github.com/cocomhub/sproxy/pkg/checksum": 0,
```

**两张表缺一不可**：任务 1 已把 `pkg/checksum` 预登记进 `Levels`，但 `Managed` 里没有它 —— 而**只有 `Managed` 才让 R3（依赖须登记）生效**。只检查 `Levels` 会让你以为登记完了，实际 R3 对这个包视而不见。
（任务 1 实施时审查者专门点出了这个漏登记面。）

- [ ] **步骤 6：跑四条机械核对**

执行 ①②③④。预期同任务 1。

- [ ] **步骤 7：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/checksum/ ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/checksum
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/checksum pkg/server internal/archcheck
git commit -m "refactor(checksum): 校验和台账抽为顶级包" \
  -m "pkg/server/checksum_store.go 迁入 pkg/checksum（12 个调用点改导入）；\ncloud_download 等跨领域消费者改为直接依赖该顶级包。\n逐字不变：仅搬迁与导入路径变更。"
```

---

### 任务 3：抽出 `pkg/storage/capacity`（容量与占用核算）

**文件：**
- 创建：`pkg/storage/capacity/manager.go`（由 `pkg/server/storage_manager.go` 迁入）
- 创建：`pkg/storage/capacity/manager_test.go`（由 `pkg/server/storage_manager_test.go` 迁入）
- 修改：`pkg/server/{chunked_upload,cloud_archive_handler,cloud_download,cloud_download_handler,config_api,handlers,quota_reconcile,stats,sync_handler,upload_store}.go`（10 个调用点）
- 修改：`internal/archcheck/layers.go`、`internal/archcheck/arch_test.go`（登记层级与子包归属）

- [ ] **步骤 1：迁移源码与专属测试**

```bash
git mv pkg/server/storage_manager.go pkg/storage/capacity/manager.go
git mv pkg/server/storage_manager_test.go pkg/storage/capacity/manager_test.go
sed -i 's/^package server$/package capacity/' pkg/storage/capacity/manager.go pkg/storage/capacity/manager_test.go
```

- [ ] **步骤 2：修可见性**

`StorageManager`、`StorageCategory`、`ReconcileFunc`、`NewStorageManager` 均已导出。检查未导出符号：

```bash
grep -rnE "storageScanTotals|storageCategory|newStorageManager" pkg/server/ | grep -v _test
```

仅在 `manager.go` 内使用 → 保持未导出。

- [ ] **步骤 3：改 `pkg/server` 的 10 个调用点**

加导入 `"github.com/cocomhub/sproxy/pkg/storage/capacity"`，把 `StorageManager` → `capacity.StorageManager`、`NewStorageManager` → `capacity.NewStorageManager`、`StorageCategory` → `capacity.StorageCategory`、`StorageCat*` 常量 → `capacity.StorageCat*`、`ReconcileFunc` → `capacity.ReconcileFunc`：

```
pkg/server/chunked_upload.go  pkg/server/cloud_archive_handler.go  pkg/server/cloud_download.go
pkg/server/cloud_download_handler.go  pkg/server/config_api.go  pkg/server/handlers.go
pkg/server/quota_reconcile.go  pkg/server/stats.go  pkg/server/sync_handler.go
pkg/server/upload_store.go
```

**注意**：`pkg/server/handlers.go` 的字段名 `storageMgr *StorageManager` 只改**类型**，**字段名不改**。

- [ ] **步骤 4：登记层级与子包归属**

`internal/archcheck/layers.go`：

```go
	"github.com/cocomhub/sproxy/pkg/storage/capacity": 2,
```

`internal/archcheck/arch_test.go` 无需改动（表在 `layers.go`）。同时在 `layers.go` 的文档注释里保持与设计文档 §3.3 一致。

- [ ] **步骤 5：跑四条机械核对**

执行 ①②③④。注意 ④ 此时才真正生效：`pkg/storage/capacity`(L2) 导入 `pkg/checksum`(L0) 合法；若它导入了 `pkg/server` 会被 R1 判红。

- [ ] **步骤 6：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/storage/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/storage
```

- [ ] **步骤 7：Commit**

```bash
git add pkg/storage pkg/server internal/archcheck
git commit -m "refactor(capacity): 容量与占用核算抽为 pkg/storage 子包" \
  -m "pkg/server/storage_manager.go 迁入 pkg/storage/capacity（10 个调用点改导入）；\n它是 cloud/sync/stats/config 共用的跨领域能力，但强关联存储域，故为 storage 域子包。\n逐字不变：仅搬迁与导入路径变更。"
```

---

### 任务 4：抽出 `pkg/volume/registry`（运行时卷集合装配与定位）

**文件：**
- 创建：`pkg/volume/registry/set.go`（由 `pkg/server/volumes.go` 的**装配与定位部分**迁入）
- 修改：`pkg/server/{handlers,chunked_upload,chunked_download,cloud_download,share,upload_handler,download_handler,list_handler,rename_handler,delete_handler,version,volumes_api}.go`（调用点）
- 修改：`internal/archcheck/layers.go`

- [ ] **步骤 1：先摸清 `volumes.go` 的职责分布**

```bash
grep -nE "^(func|type) " pkg/server/volumes.go
```

按结果分成两类：
- **装配与定位**（`volumeSet` 类型、`newVolumeSet`、`tenantFor`、`tenantOf`、`volumeTenant`、`defaultVolumeAllows`、`locateOwnerFile`、`locateForRead`、`resolveDownloadPath`、卷容量池构造）→ **搬入 `pkg/volume/registry`**。
- **HTTP 处理**（签名带 `http.ResponseWriter`/`*http.Request` 的函数）→ **留在 `pkg/server`**，改为调用 `registry`。

若该文件整体只含装配与定位（无 HTTP 函数），则整文件迁移，无需拆分。

- [ ] **步骤 2：迁移**

```bash
mkdir -p pkg/volume/registry
# 若整体迁移：
git mv pkg/server/volumes.go pkg/volume/registry/set.go
sed -i 's/^package server$/package registry/' pkg/volume/registry/set.go
# 若需拆分：新建 pkg/volume/registry/set.go 并在其中 `package registry`，
# 把装配/定位函数连注释原样移入；pkg/server/volumes.go 保留 HTTP 函数并删除已移走部分。
```

- [ ] **步骤 3：修可见性**

跨包后，凡被 `pkg/server` 使用的标识符必须导出：`volumeSet` → `registry.Set`、`newVolumeSet` → `registry.NewSet`、`tenantFor` → `registry.Set.TenantFor` 等。**逐条**按编译错误驱动修改，**不改逻辑、不改方法体**。

- [ ] **步骤 4：改调用点**

`pkg/server` 各调用点加导入 `"github.com/cocomhub/sproxy/pkg/volume/registry"` 并改限定名。`handlers.go` 的 `volSet *volumeSet` 改为 `volSet *registry.Set`（**字段名不改**）。

- [ ] **步骤 5：迁移专属测试**

`volumes_test.go`、`volumes_api_test.go`（共 1056 行）需**按内容判断**：只测装配/定位的 → 迁入 `pkg/volume/registry/`并改包名；含 HTTP 行为的 → **留在 `pkg/server`**。

- [ ] **步骤 6：登记层级与子包归属**

`internal/archcheck/layers.go`：

```go
	"github.com/cocomhub/sproxy/pkg/volume/registry": 2,
```

`ParentDomain`：

```go
	"github.com/cocomhub/sproxy/pkg/volume/registry": "github.com/cocomhub/sproxy/pkg/volume",
```

- [ ] **步骤 7：跑四条机械核对**

执行 ①②③④。④ 此时同时校验 R2（`pkg/volume/registry` 只允许 `pkg/volume` 子树导入）。

- [ ] **步骤 8：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/volume/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/volume
```

- [ ] **步骤 9：Commit**

```bash
git add pkg/volume pkg/server internal/archcheck
git commit -m "refactor(volumeregistry): 运行时卷集合装配抽为 pkg/volume 子包" \
  -m "volumes.go 的装配与定位部分迁入 pkg/volume/registry；HTTP 处理留在 pkg/server。\n逐字不变：仅搬迁、导出与导入路径变更，无逻辑改动。"
```

---

### 任务 5：抽出 `pkg/files/chunked`（分块会话与块传输）

**文件：**
- 创建：`pkg/files/chunked/{store.go,upload.go,download.go}`（由 `upload_store.go`、`chunked_upload.go`、`chunked_download.go` 迁入）
- 创建：`pkg/files/chunked/{store_test.go,upload_test.go,upload_sessions_test.go,download_test.go}`（由同名 `*_test.go` 迁入）
- 修改：`pkg/server/handlers.go`（路由接线与字段类型）
- 修改：`internal/archcheck/layers.go`

- [ ] **步骤 1：迁移源码**

```bash
mkdir -p pkg/files/chunked
git mv pkg/server/upload_store.go pkg/files/chunked/store.go
git mv pkg/server/chunked_upload.go pkg/files/chunked/upload.go
git mv pkg/server/chunked_download.go pkg/files/chunked/download.go
sed -i 's/^package server$/package chunked/' pkg/files/chunked/*.go
```

- [ ] **步骤 2：迁移专属测试**

```bash
git mv pkg/server/chunked_upload_test.go pkg/files/chunked/upload_test.go
git mv pkg/server/chunked_upload_sessions_test.go pkg/files/chunked/upload_sessions_test.go
git mv pkg/server/chunked_download_test.go pkg/files/chunked/download_test.go
sed -i 's/^package server$/package chunked/' pkg/files/chunked/*_test.go
```

`upload_store` 无同名测试（覆盖在混合测试中）→ 不迁移。

- [ ] **步骤 3：修可见性**

跨包后导出被 `pkg/server` 使用的标识符：`UploadStore`/`UploadStoreIface`/`NewUploadStore`/`ChunkedUploadSession`/`ChunkFileLocker` 已导出；HTTP 处理函数（`uploadInit`/`uploadChunk`/`uploadStatus`/`uploadComplete`/`downloadChunk`）需导出为 `UploadInit`/`UploadChunk`/`UploadStatus`/`UploadComplete`/`DownloadChunk` 等——**逐条按编译错误驱动**，方法体不动。

- [ ] **步骤 4：改 `pkg/server` 接线**

`pkg/server/handlers.go`：路由注册改为指向新包的方法（**pattern 字符串逐字不变**），相关字段类型改为 `*chunked.*`。

- [ ] **步骤 5：登记层级与子包归属**

`internal/archcheck/layers.go`：

```go
	"github.com/cocomhub/sproxy/pkg/files/chunked": 3,
```

`ParentDomain`：

```go
	"github.com/cocomhub/sproxy/pkg/files/chunked": "github.com/cocomhub/sproxy/pkg/files",
```

- [ ] **步骤 6：跑四条机械核对**

执行 ①②③④。**③ 是关键**：路由 pattern 字符串必须逐字不变。

- [ ] **步骤 7：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/files/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/files
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/files pkg/server internal/archcheck
git commit -m "refactor(files): 分块会话与块传输抽为 pkg/files/chunked 子包" \
  -m "upload_store/chunked_upload/chunked_download 三个文件迁入 pkg/files/chunked；\n路由 pattern 逐字不变，pkg/server 仅改接线与限定名。"
```

---

### 任务 6：抽出 `pkg/files/version`（文件版本存储）

**文件：**
- 创建：`pkg/files/version/store.go`（由 `pkg/server/version.go` 的**存储部分**迁入）
- 创建：`pkg/files/version/{store_test.go,id_test.go,crossvolume_test.go}`（按内容判断）
- 修改：`pkg/server/upload_handler.go`（`saveVersionBeforeOverwrite` 调用点）
- 修改：`internal/archcheck/layers.go`

- [ ] **步骤 1：先摸清 `version.go` 的职责分布**

```bash
grep -nE "^(func|type) " pkg/server/version.go
```

- **版本存储**（版本条目类型、读写版本目录、ID 生成、`saveVersionBeforeOverwrite` 的实现）→ 搬入 `pkg/files/version`。
- **HTTP 处理**（`/api/versions` 的 list/restore/delete 处理器）→ 留在 `pkg/server`。

- [ ] **步骤 2：迁移**

```bash
mkdir -p pkg/files/version
```

新建 `pkg/files/version/store.go`（`package version`），把版本存储相关的类型与函数连注释**原样**移入；`pkg/server/version.go` 保留 HTTP 部分并删除已移走内容。

- [ ] **步骤 3：修可见性**

按编译错误逐条导出被 `pkg/server` 使用的标识符。**不改方法体**。

- [ ] **步骤 4：迁移测试（按内容判断）**

`version_test.go`、`version_id_test.go`、`version_crossvolume_test.go`：

```bash
grep -lE "http\.|httptest\." pkg/server/version*_test.go
```

- 不含 HTTP 的 → 迁入 `pkg/files/version/` 并改包名。
- 含 HTTP 的 → **留在 `pkg/server`**。

- [ ] **步骤 5：登记层级与子包归属**

```go
	"github.com/cocomhub/sproxy/pkg/files/version": 3,
```
```go
	"github.com/cocomhub/sproxy/pkg/files/version": "github.com/cocomhub/sproxy/pkg/files",
```

- [ ] **步骤 6：跑四条机械核对**

执行 ①②③④。

- [ ] **步骤 7：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/files/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/files
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/files pkg/server internal/archcheck
git commit -m "refactor(files): 文件版本存储抽为 pkg/files/version 子包" \
  -m "version.go 的存储部分迁入 pkg/files/version；/api/versions 的 HTTP 处理留在 pkg/server。"
```

---

### 任务 7：抽出 `pkg/files` 根（只读面 + Deps 接缝 + 路由导出）

**文件：**
- 创建：`pkg/files/files.go`（`Deps` 接缝 + 服务构造）、`pkg/files/read.go`（list/stat/download 处理器）
- 修改：`pkg/server/{list_handler,download_handler}.go`（改为薄适配）
- 修改：`pkg/server/handlers.go`（构造 `files.Service`、接线）
- 修改：`internal/archcheck/layers.go`

- [ ] **步骤 1：确认只读面的外部依赖**

```bash
grep -ho 'h\.[A-Za-z_][A-Za-z0-9_]*' pkg/server/list_handler.go pkg/server/download_handler.go | sort -u
```

**过滤掉假信号**：`filepath.X` / `hash.X` 会被该 grep 匹配成 `h.X`（子串），局部 `h := sha256.New()` 会遮蔽 receiver。逐个确认真实的 `*Handlers` 成员。

- [ ] **步骤 2：定义 `Deps` 接缝**

在 `pkg/files/files.go` 定义（**只放装配注入类依赖**；下层包直接 import）：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package files 是文件服务领域包：上传/下载/列表/改名/删除/目录操作。
// 分块会话见 pkg/files/chunked，版本存储见 pkg/files/version。
package files

// Deps 是文件服务的外部依赖接缝。只放「必须由 pkg/server 注入」的装配项；
// 下层能力（校验和台账、容量核算、卷注册表、路径安全）由本包直接 import，
// 不经过接缝——接缝越小，包边界越清楚。
type Deps struct {
	Logger    *slog.Logger
	Config    func() *Config // 原子读当前配置
	Metrics   *Metrics
	Audit     func(...)
	Uploading *sync.Map
}
```

`Config`/`Metrics` 的类型来自 `pkg/server` → **不可行**（会成环）。故这两者改为**本包内定义的最小接口**或**窄函数**，例如：

```go
type Deps struct {
	Logger  *slog.Logger
	MaxUploadBytes func() int64
	ChunkSize      func() int
	Audit          func(action, target string, err error)
	Uploading      *sync.Map
}
```

**实现者须按步骤 1 的实测结果决定接缝的最小形状**：凡是能用「窄函数/窄接口」表达的就不要传整个 `Config`——这是避免 `pkg/files` 反向依赖 `pkg/server` 的关键。

- [ ] **步骤 3：迁移只读面**

`git mv` `pkg/server/list_handler.go` → `pkg/files/read.go`、`download_handler.go` 的只读部分 → `pkg/files/read.go`（`package files`），HTTP 处理器方法挂到 `*files.Service`，方法体**逐字不改**，仅把 `h.<下层能力>` 改为直接使用下层包实例、`h.<接缝项>` 改为 `s.deps.<项>`。

- [ ] **步骤 4：`pkg/server` 改为薄适配**

`h.listFiles` / `h.stat` / `h.download` 变为一行转发：

```go
func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) { h.files.List(w, r) }
```

**路由注册 pattern 逐字不变。**

- [ ] **步骤 5：迁移专属测试**

`list_handler_test.go`（67 行）按内容判断：只测 list 行为的迁入 `pkg/files/`；含 `Handlers` 装配的留在 `pkg/server`。

- [ ] **步骤 6：登记层级**

```go
	"github.com/cocomhub/sproxy/pkg/files": 4,
```

- [ ] **步骤 7：跑四条机械核对**

执行 ①②③④。

- [ ] **步骤 8：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/files/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/files
```

- [ ] **步骤 9：Commit**

```bash
git add pkg/files pkg/server internal/archcheck
git commit -m "refactor(files): 文件服务只读面抽为 pkg/files 领域包" \
  -m "list/stat/download 迁入 pkg/files，引入最小 Deps 接缝（避免反向依赖 pkg/server）；\npkg/server 保留一行薄适配转发，路由逐字不变。"
```

---

### 任务 8：`pkg/files` 写面

**文件：**
- 创建：`pkg/files/write.go`（由 `upload_handler.go` 迁入）
- 创建：`pkg/files/simple.go`（由 `rename_handler.go`、`delete_handler.go`、`dirs.go` 迁入）
- 修改：`pkg/server/handlers.go`（薄适配 + 接线）
- 修改：`internal/archcheck/layers.go`（无新包，仅确认）

- [ ] **步骤 1：迁移源码**

```bash
git mv pkg/server/upload_handler.go pkg/files/write.go
git mv pkg/server/rename_handler.go pkg/files/rename.go
git mv pkg/server/delete_handler.go pkg/files/delete.go
git mv pkg/server/dirs.go pkg/files/dirs.go
sed -i 's/^package server$/package files/' pkg/files/*.go
```

（若 `upload_handler.go` 与 `read.go` 已有同名符号冲突，按编译错误重命名内部未导出助手——**不改行为**。）

- [ ] **步骤 2：迁移专属测试**

```bash
git mv pkg/server/upload_handler_test.go pkg/files/upload_test.go
git mv pkg/server/dirs_owner_test.go pkg/files/dirs_owner_test.go
sed -i 's/^package server$/package files/' pkg/files/*_test.go
```

`rename_handler`、`delete_handler` 无同名测试 → 不迁移。若上述测试编译失败（引用 `pkg/server` 非公开符号）→ 移回 `pkg/server`。

- [ ] **步骤 3：`pkg/server` 薄适配**

`h.upload` / `h.rename` / `h.delete` / `h.mkdir` / `h.rmdir` 各变一行转发。路由 pattern 逐字不变。

- [ ] **步骤 4：跑四条机械核对**

执行 ①②③④。

- [ ] **步骤 5：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/files/... ./pkg/server/ ./internal/archcheck/
make lint-all
gofmt -l pkg/files
```

- [ ] **步骤 6：整包 e2e 验证**

```bash
make test-e2e
```

预期：全绿（`test/**` 相对基线**一行未改**，是行为不变的最强证据）。

- [ ] **步骤 7：Commit**

```bash
git add pkg/files pkg/server internal/archcheck
git commit -m "refactor(files): 文件服务写面抽入 pkg/files（upload/rename/delete/dirs）" \
  -m "写面四个文件迁入 pkg/files，pkg/server 保留一行薄适配；路由逐字不变。"
```

---

### 任务 9：`pkg/client` 客户端对称

**文件：**
- 创建：`pkg/client/chunked/`（由 `pkg/client/chunked.go` 迁入）
- 创建：`pkg/client/files/`（由 `pkg/client` 的文件操作方法迁入）
- 修改：`pkg/client/*.go`（保留薄适配）、`pkg/client/*_test.go`
- 修改：`internal/archcheck/layers.go`

- [ ] **步骤 1：确认客户端侧现状**

```bash
grep -nE "^(func|type) " pkg/client/chunked.go | head -30
ls pkg/client/
```

- [ ] **步骤 2：抽取分块能力**

```bash
mkdir -p pkg/client/chunked
git mv pkg/client/chunked.go pkg/client/chunked/chunked.go
sed -i 's/^package client$/package chunked/' pkg/client/chunked/chunked.go
```

按编译错误逐条导出被 `pkg/client` 使用的标识符。

- [ ] **步骤 3：抽取文件操作方法**

把 `pkg/client` 中文件操作方法（`Upload`/`Download`/`List`/`Delete`/`Rename`/`Mkdir`/`Rmdir` 等）移入 `pkg/client/files`，`pkg/client` 保留 `FileClient` 的薄适配方法（一行转发）以**不改公开 API**。

- [ ] **步骤 4：迁移专属测试**

`pkg/client/client_test.go` 的 `newMockServer` 等混合测试**留在 `pkg/client`**；仅搬完全属于分块能力的测试（若有）。

- [ ] **步骤 5：登记层级**

```go
	"github.com/cocomhub/sproxy/pkg/client/chunked": 3,
	"github.com/cocomhub/sproxy/pkg/client/files":   4,
```

- [ ] **步骤 6：跑四条机械核对**

执行 ①②③④。注意 ② 的基线只覆盖 `./pkg/... ./internal/...`，已含 `pkg/client`。

- [ ] **步骤 7：跑测试与 lint**

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
go build ./... && go test -count=1 ./pkg/client/... ./internal/archcheck/
make lint-all
gofmt -l pkg/client
```

- [ ] **步骤 8：Commit**

```bash
git add pkg/client internal/archcheck
git commit -m "refactor(client): 客户端文件操作与分块能力抽为同构子包" \
  -m "pkg/client 保留 FileClient 公开 API 的薄适配；与服务端 pkg/files 形成对称结构。"
```

---

## 收尾

- [ ] **全分支最终确认**：`make lint-all` 0 issues；`go test -count=1 ./pkg/... ./internal/...` 全绿；`make test-e2e` 全绿；四条机械核对全部无差异。
- [ ] **逐片 PR**：按任务顺序，每片一个 PR（squash 合并）；每片 CI 全绿后再开下一片。
- [ ] **Y-C 衔接**：A–C 全部合并 master 后，把 `feature/y-read-transport` rebase 到新 master，继续 T6（届时 T6 复用 `pkg/files`，不再自写下载路径）。

## 自检记录

**1. 规格覆盖度**

| 规格章节 | 对应任务 |
|---|---|
| §3 目标布局（7 个新包） | 任务 1–9（`pkg/pathguard`→1、`pkg/checksum`→2、`capacity`→3、`registry`→4、`files/chunked`→5、`files/version`→6、`files`→7/8、`pkg/client`→9） |
| §3.2 已有接口随包迁移 | 任务 2（`ChecksumStoreIface`）、任务 5（`UploadStoreIface`）步骤 2/3 |
| §4 Deps 收缩 | 任务 7 步骤 2（含"窄接口替代整个 Config"的关键约束） |
| §5 阶段与 PR 切分 | 9 个任务；A-3 拆为任务 3+4（已在文档头部说明） |
| §6 四条机械核对 | 每任务均有"跑四条机械核对"步骤；基线在任务 1 步骤 1 建立 |
| §7 逐字不变 | 全局约束 + 每任务的"方法体不动/pattern 不变"要求 |
| §8 archcheck 门禁 | 任务 1 步骤 6/7 建立；任务 3–9 登记层级与子包归属 |
| §9 与 Y 衔接 | 收尾节 |

**2. 占位符扫描**：无"待定/TODO"。任务 4/6/7 中"按内容判断"的步骤均给出了**判定命令**与**两种分支的完整处置**，非占位。

**3. 类型一致性**：`Deps` 字段在任务 7 定义、任务 8 沿用；`registry.Set`（任务 4 导出）在任务 4 之后的调用点统一；`capacity.*` 前缀在任务 3 与后续任务一致；`chunked.*` 在任务 5/7 一致。
