# 文件服务子包抽取 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 `pkg/server` 的文件服务（list/stat/download/upload/delete/rename/mkdir/rmdir + 分块传输 + 校验和 + 版本）抽成领域包 `pkg/files`（含 `chunked`/`version` 子包），并把跨领域复用的抽象提升为顶级包（`pkg/pathguard`、`pkg/checksum`），全程**逐字不变**。

**架构：** 本计划是**纯搬迁**：文件按领域移动、包名与导入路径变更、`pkg/server` 侧留一行薄适配转发，**零逻辑变更**。新增 `internal/archcheck` 门禁把「分层方向」与「子包只被父域导入」变成可执行断言。

**技术栈：** Go 1.26；纯标准库（`log/slog`、`os/exec` 用于门禁）；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-12-file-service-extraction-design.md`

---

## 计划对规格的一处切分调整（已确认）

规格 §5 的 **A-3**（`pkg/storage/capacity` + `pkg/volume/registry`）在本计划中**拆成两个任务**：容量核算与卷装配**互不依赖**，各自可独立审查、独立回退。

另据**任务 4 的实测**（B 阶段文件全部 handler 耦合），**新增任务 5（`pkg/files` 接缝设计 + 最小一族）**并重排后续（**R29：接缝先行**，见「任务 5–10 重排」一节）。故本计划共 **10 个任务**（规格 8 片 → 计划 10 片）。

## 全局约束

- **Go 1.26**；**纯标准库**；不新增任何依赖（含 `golang.org/x/`）。
- **逐字不变**：控制流、错误语义、HTTP 状态码、响应头、错误消息文案、分块语义（chunk 大小/偏移/校验）、配置默认值，**一律不得变化**。
- **不允许顺带的"小修小补"**——哪怕明显是缺陷。发现缺陷记入阶段 D 或独立议题。
- **包名按领域命名**；**类型名不改**（`ChecksumStore`/`UploadStore` 等保持原名，留到阶段 D）。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues**。二者**互补、缺一不可**：`make lint` 覆盖**根 module**、`make lint-all` 只遍历 `SUB_MODULE_DIRS`（10 个子 module，`Makefile:200`）——**`lint-all` 不含根 module**。
  > 任务步骤里若只写了 `make lint-all`，**一律理解为 `make lint` + `make lint-all` 两条**。
  > 本片（任务 4）即踩此坑：改动 100% 在根 module，而报告只列了 `lint-all` —— 证据链恰好落在盲区的**反向侧**。审查者补跑 `make lint` 才闭环。
- 提交时**只 `git add` 本任务改动的文件**（禁止 `git add -A`/`git add .`）；message 用**多重 `-m`**；**不加任何署名行**。
- **不要改动计划/规格文件**：实施中发现的经验、口径修正、边界规则（例如"某类测试该留在哪"），请**写进报告**，由控制者落进计划。计划是控制者的产物——**两人并发改同一文件是竞态**，本次（任务 3）侥幸无冲突，换个时序就会互相覆盖。
- 每次 Bash 调用都要在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`（shell 状态不跨调用保留；否则 `addlicense` 缺失导致提交被拒，且 pre-commit 的 lint 会因 `command -v` 守卫**静默跳过**）。
- **工作分支：每片一个分支，从最新 master 切出**，该片 PR 合并后再开下一片——这样每片 PR 小而聚焦、可独立回退（与"逐片 PR"一致；单一长命分支无法做到一片一 PR）。
  - 基线锚点**始终**是 `db99de06`（抽取前的 master 状态），它**不随各片合并而失效**：核对 ② 允许新增用例、只禁丢失。
- **archcheck 登记约定**：每新抽/新建一个包，都要**同时**写入 `Levels`（层级）与 `Managed`（本工作新增）；若是**子包**，另需写入 `ParentDomain`。只写 `Levels` 会让 R3 不作用于它，只写 `Managed` 会让 R1 看不见它的层级——两条都要写才有效。
- **调用点清单是 grep 估计值，编译器才是权威**：各任务里列举的调用点文件，是按 `grep -rl <符号>` 数出来的，**会把只在注释/文档字符串里提及该符号的文件也算进去**（加导入会 `unused import` 编译失败），也可能遗漏。实施时**以 `go build ./...` 的报错为准**逐条加减，并在报告里写明**实际**数量与偏离原因。
  > 已实测的实例：任务 1 的"14 个调用点"实为 **11 个非测试文件**（`checksum.go`/`chunked_download.go`/`remote_read.go` 仅注释提及）。**后续各任务的数字同样需要复核，不要照抄。**
  > 另一实测（任务 2）：简报称 12，实际**生产 5**——含简报**遗漏**的 `pkg/testutil/mockserver/checksum.go`；另有 8 个文件只调方法 `h.checksumStoreFor`、从不写类型名，故编译器确认无需改动。
  > 第三次实测（任务 3）：简报称 10 个生产调用点，其中 `config_api.go` **零命中**（只调 `h.storageMgr` 的方法、从不写类型名）；但简报**漏了 11 个 `*_test.go`** 与**漏了 `cmd/sproxy/root.go`**（跨 module）。
  > **⚠️ 跨 module 盲区（与 `golangci-lint run ./...` 不跨 module 同源）**：根 `go build ./...` **不覆盖 `cmd/*` 等子 module**。所以"编译器才是权威"必须说清**用哪次编译**：`go build ./...` 只能证明根 module；**必须**同时跑 **`make build-all`**（10 个子 module）才能证明全仓。两者都过，才算"编译器确认"。
- **子包专属测试的边界**：子包不能反向导入 `pkg/server`（成环 + 被 R2 判红）。故被搬测试中**依赖装配层测试基座**（如 `newOwnerEnv`/`newAssemblyTestHandlers`）的用例，其断言对象实为**装配层行为**（子包只是驱动器）——这类用例**留在 `pkg/server`**（改名迁移到该领域的测试文件里）而不是随包搬走。这**不违反**核对 ②：用例名仍守恒（零丢失），只是换了宿主文件。
- **变异验证前，先证明变异真的生效**（二阶假绿的防线）：用 overlay 或仓库外副本做变异时，**必须先用一个正探针**确认覆盖生效——例如注入一个会让相关命令**报错**的改动，看它是否真的报错。
  > **实测陷阱**：把 `import _ "x"` **追加在函数声明之后**是 **Go 语法错误**，而 `go list` 对语法错误的覆盖文件会**静默回退到原文件** → 变异未生效、门禁保持**假绿** → 你会得出「**门禁无牙**」的**错误结论**。必须在既有 `import (...)` 块内插入。
  > 这是**二阶假绿**：不是"门禁假绿"，而是"**证明门禁假绿的实验**假绿"。后续任何声称"变异后变红/变绿"的报告，都必须附上"探针证明变异生效"的证据。

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

> **⚠️ ② 的能力边界（任务 7 实测后补记）**：② 证明的是「**用例名**零丢失」，**不是「断言内容」未变**。对**纯搬迁**二者等价（方法体连测试一起搬），但**结构性改动的片**（任务 4 起）理论上可以"名不变、断言被削弱"。故：**凡本片删减了测试行数（`git diff --stat` 里测试文件出现大幅 `-`），实现者必须逐条说明那些行去哪了**（迁走 / 合并 / 删除及理由），审查者必须核实——**不得因 ② 的 `-`=0 即认定"测试场景未丢"**。

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
| ~~`pkg/files/chunked`~~ | — | **已取消（R34/P6）** → 平铺进 `pkg/files` | `upload_store.go` + `chunked_upload.go` + `chunked_download.go` |
| ~~`pkg/files/version`~~ | — | **已取消（R34/P6）** → 平铺进 `pkg/files` | `pkg/server/version.go`（存储部分） |
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
// 随各片 PR 增量登记，例如 pkg/volume/registry → pkg/volume。
// （注意：R34 后 pkg/files 不再分子包，故不存在 pkg/files/* 的父域条目。）
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

## ⚠️ 任务 4 起飞前的前提修正：任务 4–9 不是纯搬迁

**实测（任务 4 起飞前）**：任务 1–3 能做到字节级不动，是因为它们恰好都是**纯叶子**（0 个 `*Handlers` 方法）。从任务 4 起，每个文件都挂着 `(h *Handlers)` 方法：

| 文件 | `*Handlers` 方法数 | | 文件 | `*Handlers` 方法数 |
|---|---|---|---|---|
| `volumes.go` | **10** | | `upload_handler.go` | 6 |
| `chunked_upload.go` | **15** | | `download_handler.go` | 5 |
| `version.go` | **10** | | `delete_handler.go` | 5 |
| `list_handler.go` | 7 | | `rename_handler.go` / `dirs.go` / `chunked_download.go` | 3 / 2 / 2 |
| ~~`validate.go` / `checksum_store.go` / `storage_manager.go`（已搬）~~ | **0** | | `upload_store.go`（纯 store） | **0** |

**跨包就必须换接收者或定义接缝 —— 这是结构性改动，不是文件移动。** 故：

- **字节级证据（blob diff / 逆变换 sha256）只对纯叶子成立**，本阶段**拿不到**。**不要**在报告里声称字节级不变，那会是夸大。
- **"功能与测试场景一致"仍可证**，本阶段的证据主体是：核对 **②**（用例名零丢失）+ **③**（路由表逐字一致）+ **e2e 零改动**（`test/**` 一行不改，走真二进制）+ diff 审查。
- **接缝设计（新增的构造/接口、接收者怎么改、调用方怎么变）是本片审查重点**，报告须单独成节说明"为什么这样切"。
- 原计划把接缝只安排给任务 7（称"全计划唯一有设计判断处"）**是错的** —— 任务 4、5、6、8 同样需要。

---

### 任务 4：抽出 `pkg/volume/registry`（运行时卷集合装配与定位）

**文件：**
- 创建：`pkg/volume/registry/set.go`（`pkg/server/volumes.go` 中**不依赖 `Handlers` 接收者**的部分）
- 修改：`pkg/server/{handlers,chunked_upload,chunked_download,cloud_download,share,upload_handler,download_handler,list_handler,rename_handler,delete_handler,version,volumes_api}.go`（调用点）
- 修改：`internal/archcheck/layers.go`

**本片走「最小版」（控制者裁定）**：只搬**不需要新 API 决策**的部分，用这一片的真实形态取数据，再决定后续片怎么做。

- [ ] **步骤 1：先量清耦合面（不要照抄原计划的二分法）**

```bash
grep -nE "^(func|type) " pkg/server/volumes.go
grep -cE "^func \(h \*Handlers\)" pkg/server/volumes.go   # 实测 = 10
grep -cE "net/http" pkg/server/volumes.go                  # 实测 = 1（仅状态码常量）
```

**实测结论（已由控制者核对）**：**带 `http.ResponseWriter` 的函数数 = 0** —— 所以原计划"按 HTTP 处理与否二分"**在这个文件上不成立**（HTTP 面在隔壁 `volumes_api.go`）。真实的分界是**要不要 `Handlers` 接收者**。

**搬迁判据（机械，不需要设计判断）**：
- **搬**：能**不引用 `h` 就编译通过**的部分 —— 预期是 `volumeSet` + 其 8 个方法（`Default`/`All`/`ByName`/`DefaultRoot`/`Root`/`Pool`/`Close`/`Tenant`），以及（若同样不引用 `h`）`routeErrorKind`/`routeError`/`newRouteError`、`volumeRoute` + `commit`/`release`、`fileLocation`。**以编译器为准**，不靠预判。
- **留**：`(h *Handlers)` 方法共 **10 个** —— `routeUpload`、`reserveVolume`、`volumeTenant`、`volumeFileExists`、`checkVolumeUniqueness`、`locateOwnerFile`、`volumePoolForTenant`、`defaultVolumeAllows`、`primaryViewTenant`、`locateForRead`。它们是消费 registry 的装配层逻辑。
- **留（配置耦合，搬了会成环）**：`resolveDefaultVolumeRoot(cfg *Config)`、`assembleVolumes(cfg *Config, log)`、`parseVolumeACL(ac *VolumeACLConfig, log)` —— 它们把**服务端配置**翻译成领域类型；搬走就要连 `Config`/`VolumeACLConfig` 一起搬（超出本片范围），且会造成 `registry → pkg/server` **成环**。

- [ ] **步骤 2：迁移（唯一的 API 决策：构造接缝）**

**本片只引入一个新 API**：`Set` 的构造函数，签名 = `volumeSet` **现有的全部字段**（字段顺序与语义不变）。这样 `assembleVolumes` 的函数体只在其**最后一行构造处**改变，其余逐字不动。

```bash
mkdir -p pkg/volume/registry
```

新建 `pkg/volume/registry/set.go`（`package registry`）：把步骤 1 判定为"搬"的部分**连注释原样移入**，补上：

```go
// NewSet 由装配层已解码的卷集合构造运行时卷视图。
// 参数与顺序刻意与 Set 的字段一一对应，使调用方（pkg/server 的 assembleVolumes）
// 只需替换最后一行构造，函数体其余部分逐字不变。
func NewSet(/* …与 Set 字段一一对应… */) *Set {
	return &Set{/* … */}
}
```

`pkg/server/volumes.go` 保留步骤 1 判定为"留"的部分，并把构造处改为 `registry.NewSet(...)`。**类型名 `Set` 是本片唯一的重命名**（原 `volumeSet` 未导出，跨包必须导出）。

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

## 任务 5–10 重排（R29：接缝先行）

**理由（任务 4 实测得出）**：任务 4 起每个文件都 handler 耦合。若让各族**各自**发明接缝，5 片之后会出现 **5 种边界** —— 正是本工作要消除的"各自实现"。故改为**接缝先行**。

**新顺序**：

## ⚠️ R34（用户 2026-09-12 定）：子包判据修正 —— **取消两个子包**

**两条判据变更**（已写进规格 §2）：
- **P1 修订**：**组织单位是「文件」，不是「包」**——逻辑上强相关的内容留在同一领域包内，用文件组织。**不要为了拆分而拆分。**
- **P6 新增**：子包**只应是二者之一** —— ① **可复用的扩展工具集合**；② **真正的子领域**。**"某个功能的处理器 + 它的存储"不属于任何一类。**

**据此取消**：`pkg/files/chunked` 与 `pkg/files/version`——分块族与版本族**一律平铺进 `pkg/files`**。
（依据：任务 6 实测 `chunked` 子包注入面 **19 字段全部是 handler 需要、store 需要 0 个**，并产生 4 个镜像类型与约 130 行装配适配器。）

**保留**：`pkg/storage/capacity`（容量核算）与 `pkg/volume/registry`（运行时卷集合）——属 P6 的"**真正的子领域**"，消费者均为装配层。

| 新序号 | 内容 | 接缝 |
|---|---|---|
| **5** | `pkg/files` 接缝设计 + 最小一族（`dirs.go`） | ✅ **已合并** |
| **6** | **分块族平铺进 `pkg/files`**（消解 `pkg/files/chunked`） | 合并进同一 `Deps` |
| 7 | 文件版本平铺进 `pkg/files` | 同上 |
| 8 | `pkg/files` 只读面（list/stat/download）平铺 | 同上 |
| 9 | `pkg/files` 写面（upload/rename/delete）平铺 | 同上 |
| 10 | `pkg/client` 客户端对称 | — |

> 下述各节标题里的序号是**原计划序号**，按上表映射到新序号执行。

---

### 任务 5（新）：`pkg/files` 接缝设计 + 最小一族（`dirs.go`）验证接缝

**本片的主要交付物是「接缝」，不是那 2 个 handler。** 它定下的形态是任务 6–9 的**规范依据**。

**文件：**
- 创建：`pkg/files/service.go`（`Deps` + `Service` + 构造）
- 创建：`pkg/files/dirs.go`（由 `pkg/server/dirs.go` 迁入）
- 创建/迁移：`pkg/files/dirs_test.go`（若 `pkg/server/dirs_owner_test.go` 为**专属**测试）
- 修改：`pkg/server/handlers.go`、`pkg/server/dirs.go`（薄适配 + 接线）
- 修改：`internal/archcheck/layers.go`（登记 `pkg/files`）

**为什么选 `dirs.go` 验证接缝**：它是最小的一族（2 个 `(h *Handlers)` 方法 + 2 个顶层函数），足以跑通整条接缝（Deps 注入 → handler 迁入 → 薄适配 → 路由不变），又小到能一眼审完。

- [ ] **步骤 1：量清 `dirs.go` 的外部依赖（小心假信号）**

```bash
grep -ho 'h\.[A-Za-z_][A-Za-z0-9_]*' pkg/server/dirs.go | sort | uniq -c | sort -rn
```

> ⚠️ **该 grep 有已知假信号**：`filepath.Join` 会被匹配成 `h.Join`（子串），局部 `h := sha256.New()` 会**遮蔽 receiver**。**每一条都要回源码确认它是不是真的 `*Handlers` 成员**（用 `sed -n` 读上下文，别只看 grep 行）。

- [ ] **步骤 2：设计 `Deps`（接缝的唯一新增 API 面）**

`Deps` **只放"必须由 `pkg/server` 注入"的装配项**；下层能力（`pkg/pathguard`、`pkg/checksum`、`pkg/storage/capacity`、`pkg/volume/registry`）**直接 import**，不经过接缝——**接缝越小，包边界越清楚**。

**硬约束**：`Deps` 里**不得出现 `pkg/server` 的任何类型**（如 `*Config`、`*Metrics`）——那会让 `pkg/files` 反向依赖 `pkg/server`，**被门禁 R3 判红**。需要配置时用**窄函数**（如 `MaxUploadBytes func() int64`），不要传整个 `Config`。

- [ ] **步骤 3：定义子包窄接口的约定（并写进 `service.go` 的包文档）**

**新发现的设计约束**（**R34 后已不适用，保留为历史**）：当时设想 `pkg/files/chunked`（子包）**不能导入父域 `pkg/files`** —— 会违反门禁 R1。该约束随 R34 取消子包而失效：分块族与版本族**平铺进 `pkg/files`**，全部共用**同一个** `Deps`。

**约定（本片定死，任务 6–9 遵守）**：
- **各族在自己的包里定义只含自身所需能力的窄接口**（Go 的"消费者定义接口"惯用法），字段/方法名由该族自己定；
- **实现方只有一处**：`pkg/server` 的装配层实现**所有**这些窄接口（可在一个 `fileServiceDeps` 适配器上实现多个）。
- 因此"接口可以多份，**实现只有一处**"——这与"避免各自实现"并不冲突：冲突的是各族自己重写文件读写逻辑，而那正是要被消除的。
- `pkg/files` 根族的接缝就是本片的 `Deps`（它是领域根，不存在"不能导入父域"的问题）。

- [ ] **步骤 4：迁 `dirs.go` 并接上接缝**

```bash
git mv pkg/server/dirs.go pkg/files/dirs.go
sed -i 's/^package server$/package files/' pkg/files/dirs.go
```

handler 方法挂到 `*Service`，方法体**逐字不改**，只把 `h.<下层能力>` 改为直接使用下层包实例、`h.<接缝项>` 改为 `s.deps.<项>`。可见性按编译错误逐条调整（因跨包而必须的导出允许）。

- [ ] **步骤 5：`pkg/server` 改为薄适配**

`h.mkdir` / `h.rmdir` 变为一行转发。**路由注册 pattern 逐字不变**（核对③会验）。

- [ ] **步骤 6：登记 `internal/archcheck/layers.go`**

`Levels`：`pkg/files` → **4**；`Managed`：→ true。**`pkg/files` 是领域根，不写 `ParentDomain`**（它的子包才写）。

- [ ] **步骤 7：四条机械核对 + 变异 + lint**

① `git diff --stat db99de06 -- test/` 为空；② **用例名零丢失**（无 `-` 行）；③ 生产路由表逐字一致；④ 门禁 PASS。
**变异验证**：至少证明 `pkg/files` 受规则③约束（导入未登记的 `pkg/` 包 → 红），**先跑正探针证明变异生效**。
`go build ./...` **与** `make build-all`；`make lint` **与** `make lint-all` **均 0 issues**。

- [ ] **步骤 8：Commit**

**报告必须单独成节的「接缝设计」**（它是任务 6–9 的规范依据）：`Deps` 的完整字段与类型、`Service` 的形状、薄适配的做法、子包窄接口的约定与**一个具体示例草图**、以及你在实践中撞到的判据边界。

---

### 任务 6（重做）：分块族**平铺**进 `pkg/files`（消解 `pkg/files/chunked` 子包）

> **本任务是回炉重做**。上一轮把分块族拆成了 `pkg/files/chunked` 子包，实测证明该切分错误：
> - 子包**逐字段实测**：其 `Deps` **19 个字段全部是 handler 需要、store 需要 0 个**——接缝规模 100% 是"处理器被放进子包"的产物（`store.go` 对 `deps.` 的引用数 = **0**）。
> - 子包被迫**重新定义** `StorageManager`/`Metrics`/`DownloadPath`/`FileLocation`/`UploadRoute`/`HTTPError`，其中 3 个在父域已有对应物（**镜像类型**）。
> - 因 R1 禁子包上行导入父域，这 5 个父域能力字段被**永久固化**——后续读/写面各片还会把同样的重名字段 + 值类型 + 约 130 行适配器**再复制一遍**。
> - 按修订后的 **P6**："某个功能的处理器 + 它的存储"**不构成子领域** → 应留在父域包内、用**文件**组织。
>
> 分支 `refactor/files-chunked` **未 push**，故可直接重写提交历史（或追加一个消解提交）。

**文件：**
- **删除子包**：`pkg/files/chunked/` 整个目录（其内容平铺进 `pkg/files/`）
- 结果形态：`pkg/files/chunked_store.go`、`chunked_upload.go`、`chunked_download.go`、`chunked_response.go` 与相应 `*_test.go`
- 修改：`pkg/files/service.go`（`Deps` 合并子包那 12 个字段；删子包自有的 `Deps`/`Handlers`/`New`/`missingDeps` 与重复的 `sendJSON`/`normalizeOwner`/`anonymousOwner`/`defaultLogger`/第三份 `UploadResponse`）
- 修改：`pkg/server/handlers.go`（薄适配直接指向 `h.fileService()`；删 `chunkedSvc`/`chunkedOnce` 与 `pkg/server/chunked_service.go` 大部分）
- 修改：`internal/archcheck/layers.go`（**删掉**三张表里的 `pkg/files/chunked` 条目；`pkg/files` 从 L4 降为 **L3**）
- **撤销** `pkg/testutil/mockserver` 的 `AssemblyPackages` 例外（替身改导入 `pkg/files`——它**不是子包**，R2 不适用，**该例外根本不必存在**）

- [ ] **步骤 1：消解子包——把它的文件平铺回 `pkg/files`**

```bash
git mv pkg/files/chunked/store.go     pkg/files/chunked_store.go
git mv pkg/files/chunked/upload.go    pkg/files/chunked_upload.go
git mv pkg/files/chunked/download.go  pkg/files/chunked_download.go
# response.go / helpers.go 的内容**并入** pkg/files 既有同名职责文件（勿留第二份）
# 相应 *_test.go 同样平铺
sed -i 's/^package chunked$/package files/' pkg/files/chunked_*.go pkg/files/chunked_*_test.go 2>/dev/null || true
rmdir pkg/files/chunked 2>/dev/null || ls pkg/files/chunked/
```

- [ ] **步骤 2：合并接缝（删掉第二套）**

把 `chunked.Deps` 的 12 个字段**并入 `pkg/files.Deps`**（`pkg/files.Deps` 最终 = 19 字段）。**同时删除**：
- 子包自有的 `Deps` / `Handlers` / `New` / `missingDeps` / 构造期校验（父域已有）
- 重复的 `sendJSON` / `normalizeOwner` / `anonymousOwner` / `defaultLogger` / 第三份 `UploadResponse`（父域已各有一份）
- 三个装配映射与错误映射（`chunked_service.go` 里的那批）
- `VolumesAssembled bool` → **改用父域既有的 `VolSet != nil`**（少一个字段、少一类不变式）

**注意**：`Deps.StorageManager` 的类型此前是子包的 `chunked.StorageManager`——平铺后它就在 `pkg/files` 内，**保持单一事实源，不要复制第二份接口**。

- [ ] **步骤 3：`pkg/server` 薄适配**

6 个路由处理器改为一行转发到 `h.fileService()`（与既有 `h.mkdir` 同形）。**路由注册 pattern 逐字不变**（核对③会验）。

- [ ] **步骤 4：archcheck 表更新**

删 `pkg/files/chunked` 在 `Levels`/`Managed`/`ParentDomain` 三张表的条目；`pkg/files` 改 **L3**；**删 `pkg/testutil/mockserver` 的 `AssemblyPackages` 例外**（并核实它改为导入 `pkg/files` 后门禁确实不报红）。

- [ ] **步骤 5：四条机械核对 + 变异 + 两条 lint**

① `git diff --stat db99de06 -- test/` 为空；② **用例名零丢失**（无 `-` 行）；③ 路由表逐字一致；④ 门禁 PASS。
`go build ./...` **与** `make build-all`；`make lint` **与** `make lint-all` **均 0 issues**；`make test-e2e`。

- [ ] **步骤 6：Commit**

**报告须说明**：消解后 `pkg/files` 的最终文件清单、`Deps` 字段数、**被消除的样板行数**（对比子包形态），以及是否还有**其它**子包按 P6 判据站不住（本任务只处理本族；发现别的记进报告）。

---

### 任务 7（重做）：文件版本**平铺**进 `pkg/files`

> **按修订后的 P6 改判**：版本是文件操作的**机制**，不是独立子领域 → **不设 `pkg/files/version` 子包**，直接把存储部分平铺进 `pkg/files`（如 `pkg/files/version_store.go` + 相应测试）。**下文"创建 `pkg/files/version/`"一律读作"平铺进 `pkg/files/`"**；`internal/archcheck` 也**不登记**任何 `pkg/files/version`。
> 判据：该功能与文件操作**强相关**（P1），且"处理器 + 存储"不构成子领域（P6）——与任务 6 同理。

**文件：**
- 创建：`pkg/files/version_store.go`（由 `pkg/server/version.go` 的**存储部分**迁入；**平铺，不建子包**）
- 创建/迁移：`pkg/files/version_store_test.go` 等（按内容判断，**平铺**）
- 修改：`pkg/server/upload_handler.go`（`saveVersionBeforeOverwrite` 调用点）
- **不改** `internal/archcheck/layers.go`（不新增包）

- [ ] **步骤 1：先摸清 `version.go` 的职责分布**

```bash
grep -nE "^(func|type) " pkg/server/version.go
```

- **版本存储**（版本条目类型、读写版本目录、ID 生成、`saveVersionBeforeOverwrite` 的实现）→ **平铺进 `pkg/files`**。
- **HTTP 处理**（`/api/versions` 的 list/restore/delete 处理器）→ 留在 `pkg/server`。

- [ ] **步骤 2：迁移（平铺）**

新建 `pkg/files/version_store.go`（**`package files`**），把版本存储相关的类型与函数连注释**原样**移入；`pkg/server/version.go` 保留 HTTP 部分并删除已移走内容。

- [ ] **步骤 3：修可见性**

按编译错误逐条导出被 `pkg/server` 使用的标识符。**不改方法体**。

- [ ] **步骤 4：迁移测试（按内容判断）**

`version_test.go`、`version_id_test.go`、`version_crossvolume_test.go`：

```bash
grep -lE "http\.|httptest\." pkg/server/version*_test.go
```

- 不含 HTTP 的 → **平铺进 `pkg/files/`**（**不改包名**——平铺后同为 `package files`）。
- 含 HTTP 的 → **留在 `pkg/server`**。

- [ ] **步骤 5：archcheck —— 本片不登记任何新包**

平铺后**没有新包**，故 `internal/archcheck/layers.go` **不改**（`pkg/files` 已是 L3）。

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
git add pkg/files pkg/server
git commit -m "refactor(files): 文件版本存储平铺进 pkg/files" \
  -m "version.go 的存储部分平铺进 pkg/files；/api/versions 的 HTTP 处理留在 pkg/server。"
```

---

### 任务 8（原 7）：`pkg/files` 只读面（list/stat/download）

> **本任务已被任务 5 部分取代**：原计划把「`Deps` 接缝」放在这里定义，现改为**任务 5 定义**。本任务只做**搬入只读面**（复用任务 5 的接缝），**不再自行设计接缝**。原「Deps 不能含 `pkg/server` 类型」等约束已上移到任务 5。

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
// 分块会话与版本存储**平铺在本包内**（R34：不分子包），见 chunked_*.go / version_store.go。
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

### 任务 9（原 8）：`pkg/files` 写面（upload/rename/delete）

**文件：**
- 创建：`pkg/files/write.go`（由 `upload_handler.go` **整文件**迁入）
- 创建：`pkg/files/rename.go`（由 `rename_handler.go` **整文件**迁入）
- 创建：`pkg/files/delete.go`（由 `delete_handler.go` **整文件**迁入）
- **不搬**：`pkg/server/dirs.go` —— 第 5 片已把 mkdir/rmdir 的**实现**迁入 `pkg/files/dirs.go`，此处已只剩 **23 行薄适配**（同名同签名的一行转发）；按薄适配形态**保留**（去除属重设计阶段）
- 修改：`pkg/server/handlers.go`（薄适配 + 接线）
- 修改：`internal/archcheck/layers.go`（无新包，仅确认）

- [ ] **步骤 1：迁移源码**

```bash
git mv pkg/server/upload_handler.go pkg/files/write.go
git mv pkg/server/rename_handler.go pkg/files/rename.go
git mv pkg/server/delete_handler.go pkg/files/delete.go
# ⚠️ 不要 mv pkg/server/dirs.go —— 它自第 5 片起已是 23 行薄适配，
#    而 pkg/files/dirs.go 是它的实现；误搬会覆盖实现（同一文件名）。
sed -i 's/^package server$/package files/' pkg/files/write.go pkg/files/rename.go pkg/files/delete.go
```

（若 `upload_handler.go` 与 `read.go` 已有同名符号冲突，按编译错误重命名内部未导出助手——**不改行为**。）

> **口径说明（任务 8 实测补记）**：本任务各条 `git mv` **一律按整文件搬迁**执行；"文件："清单里若出现比步骤更窄的表述，**以步骤为准**。（任务 8 因简报正文与步骤口径不一致而请示裁定——控制者已确认**以步骤为准**。）

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

### 任务 10：`pkg/client` 客户端侧整理（**R34 改判：不做"同构子包"**）

> **本任务原写"抽取为同构子包"（`pkg/client/chunked/`、`pkg/client/files/`），与 R34/P6 直接冲突**——按修订后的判据，**"组织单位是文件、不是包"**，且子包只应是①可复用工具集合或②真子领域；客户端的文件操作方法**不属任何一类**。
>
> **改判后的目标**：客户端侧若需要整理，**在同一包内用文件组织**（`pkg/client/` 内已有按子命令拆分的多文件形态），**不新建子包**。**本任务的必要性因此下降**——若现状已经清楚（`pkg/client` 内本就是按职责分文件），**可以只做核对、不搬迁**；请先在报告里给出"现状是否已满足'用文件组织'"的结论，再决定是否需要动作。
>
> 原计划中"与服务端形成对称结构"的动机**已不成立**——服务端也不再拆子包了（`pkg/files` 是单一领域包）。

**文件（若确认需要动作）：**
- 修改：`pkg/client/*.go`（**同包内**按职责重排文件；**不新建子包**）
- **不改**：`internal/archcheck/layers.go`（不新增包）

- [ ] **步骤 1：确认客户端侧现状，并给出"是否需要动作"的结论**

```bash
ls pkg/client/
grep -nE "^(func|type) " pkg/client/chunked.go | head -30
```

- [ ] **步骤 2：若需要动作** —— 在 **`pkg/client` 同包内**按职责重排（例如把文件操作方法归入 `files.go`、分块能力留在 `chunked.go`）；**保持公开 API 与包名不变**（`FileClient` 的方法集不动）。
  若判断**不需要动作**（现状已是"用文件组织"），**如实报告并跳过**，不要为了凑一片而搬迁。

- [ ] **步骤 3：验证** —— 四条机械核对 + 两条 lint + `make test-e2e`；**② 用例名零丢失**（若只重排文件而不改名，应 `-` = 0）。

- [ ] **步骤 4：测试** —— 只重排文件时**测试文件也随之重排**；`pkg/client/client_test.go` 的 `newMockServer` 等混合测试**留在 `pkg/client`**。

- [ ] **步骤 5：archcheck —— 本片不登记任何新包**（R34 后不再新建 `pkg/client/*` 子包）。

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
| §3 目标布局（7 个新包） | 任务 1–10（`pkg/pathguard`→1、`pkg/checksum`→2、`capacity`→3、`registry`→4、**`pkg/files` 接缝→5**、`files/chunked`→6、`files/version`→7、`files` 只读面→8、写面→9、`pkg/client`→10） |
| §3.2 已有接口随包迁移 | 任务 2（`ChecksumStoreIface`）、任务 5（`UploadStoreIface`）步骤 2/3 |
| §4 Deps 收缩 | 任务 7 步骤 2（含"窄接口替代整个 Config"的关键约束） |
| §5 阶段与 PR 切分 | 10 个任务；A-3 拆为任务 3+4、B 阶段新增任务 5（接缝先行，R29），均已在文档头部说明 |
| §6 四条机械核对 | 每任务均有"跑四条机械核对"步骤；基线在任务 1 步骤 1 建立 |
| §7 逐字不变 | 全局约束 + 每任务的"方法体不动/pattern 不变"要求 |
| §8 archcheck 门禁 | 任务 1 步骤 6/7 建立；任务 3–9 登记层级与子包归属 |
| §9 与 Y 衔接 | 收尾节 |

**2. 占位符扫描**：无"待定/TODO"。任务 4/6/7 中"按内容判断"的步骤均给出了**判定命令**与**两种分支的完整处置**，非占位。

**3. 类型一致性**：`Deps` 字段在任务 7 定义、任务 8 沿用；`registry.Set`（任务 4 导出）在任务 4 之后的调用点统一；`capacity.*` 前缀在任务 3 与后续任务一致；`chunked.*` 在任务 5/7 一致。
