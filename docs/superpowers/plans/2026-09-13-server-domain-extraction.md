# `pkg/server` 其余域抽取 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 消除全仓**唯一**的生产分层倒置（`pkg/syncexec → pkg/server/syncmgr`），并把 `CredentialStore` 归位到它所属的顶层域包；同时新增两条门禁（R4 领域包不得导入装配层、R5 `Levels` 全表化），使这类倒置今后**必红**。

**架构：** 先写门禁（R4）→ 看它红 → 搬迁 `syncmgr` 到顶层 → 绿。搬迁**逐字不变**，`pkg/server` 只改导入路径与字段类型。

**技术栈：** Go 1.26；纯标准库；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-13-server-domain-extraction-design.md`

---

## 全局约束

- **逐字不变**：`syncmgr` 搬迁只允许改包路径与导入；**不改**控制流、错误语义、日志文案、配置默认值。
- **不允许顺带的"小修小补"**——发现缺陷记入报告，由控制者落进规格。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues**（前者根 module、后者 10 个子 module，**互补缺一不可**）。
- 提交时**只 `git add` 本任务改动的文件**；message 用**多重 `-m`**；**不加署名行**。
- **不要改动计划/规格文件**：经验与口径修正写进报告，由控制者落进计划。
- 每次 Bash 调用在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`。
- **工作分支：每片一个分支，从最新 master 切出**，该片 PR 合并后再开下一片。
- **`go build ./...` 只覆盖根 module**；**必须**另跑 **`make build-all`**（10 个子 module，`cmd/sproxy` 在搬迁面内）。
- **archcheck 登记**：新顶层包 `pkg/syncmgr` 必须**同时**写入 `Levels` 与 `Managed`（R3 才会作用于它）；它不是子包，**不写** `ParentDomain`。同时删除 `pkg/server/syncmgr` 的任何残留登记（现状未登记，故无需删）。
- **变异验证前，先证明变异真的生效**：R4 的"先红"必须附上**实际红字输出**（这是正探针）。

## 四条机械核对（每片必跑）

```bash
git diff --stat db99de06 -- test/
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)   # 必须无 "-" 行
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)   # 必须为空
go test ./internal/archcheck/
```

## 验证命令（每片必跑）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
go test -race -count=1 ./pkg/syncmgr/ ./pkg/syncexec/
make test-e2e
```

---

## 任务 S1：门禁 R4/R5 + `pkg/server/syncmgr` → `pkg/syncmgr`

**规格：** §3、§4。

- [ ] **先写 R4**（`internal/archcheck/arch_test.go`）：`TestNoDomainImportsAssembly`，断言 `pkg/**` 下**非** `pkg/server` 子树的包不得导入 `pkg/server/**`。
- [ ] **跑 `go test ./internal/archcheck/` 并**记录红字输出（应报 `pkg/syncexec -> pkg/server/syncmgr`）。**这是"门禁有牙"的正探针，必须粘进报告。**
- [ ] **写 R5**（`internal/archcheck/layers.go`）：把 `Levels` 扩为全表（规格 §4 的 G0–G4 分组）。**同时**保持 `Managed` 不变（仍只含本工作新增包），`ParentDomain` 不变。
      **注意**：`pkg/files` 的注释里逐字提到当前层级（L3），全表化后编号变为 G1 ⇒ 同步更新该注释（否则注释与实测不一致）。
- [ ] 迁移包目录：`git mv pkg/server/syncmgr pkg/syncmgr`（含全部 `*_test.go`，包名 `syncmgr` 不变）。
- [ ] `internal/archcheck/layers.go`：`Managed` 增加 `github.com/cocomhub/sproxy/pkg/syncmgr`；`Levels` 增加 `.../pkg/syncmgr: 0`（纯 stdlib，零 pkg 内部依赖，实测）。
- [ ] 改导入路径（以 `go build ./...` + `make build-all` 报错为准，逐条加减）：
      - `pkg/server/handlers.go`、`pkg/server/sync_handler.go`（+ 对应 `_test.go`）
      - `pkg/syncexec/executor.go`（+ `executor_test.go`、`quota_executor_test.go`）
      - `cmd/sproxy/root.go`
- [ ] 核对 `pkg/syncmgr` 的**测试是否依赖 `pkg/server` 测试基座**：若依赖 ⇒ 该用例**留在 `pkg/server`** 并改名迁入 `sync_handler_test.go`（核对 ② 允许改名，禁丢失），报告里写明迁移清单。
- [ ] 更新 `pkg/syncexec/executor.go` 包文档中「pkg/server（经 syncmgr）…」的措辞（导入关系已变，注释必须与实测一致）。
- [ ] 验证：全局验证命令 + 四条机械核对；**R4 必须转绿**；`pkg/syncmgr` 与 `pkg/syncexec` 在 `-race` 下通过。
- [ ] 提交（分支 `refactor/extract-syncmgr`）：spec + plan + R4/R5 + 搬迁。

**DoD：** ① R4 **先红后绿**且红字粘进报告；② 四条机械核对全过；③ `make lint`/`lint-all` 0 issues；④ `go build ./...`/`make build-all` 过；⑤ `go test ./pkg/... ./internal/...` 过（含 `-race`）；⑥ e2e 过；⑦ 全仓再跑一次 D1 反查命令，输出为**空**。

---

## 任务 S2：`CredentialStore` → `pkg/accesskey`

**规格：** §5。

- [ ] `git mv pkg/server/credentialstore.go pkg/accesskey/credentialstore.go`（包名 `accesskey`；删掉对 `accesskey.` 的包内限定——同包引用）。若有 `pkg/server/credentialstore_test.go` 一并搬迁。
- [ ] `pkg/server` 侧调用点：`NewCredentialStore(...)` → `accesskey.NewCredentialStore(...)`（以 `go build ./...` 报错为准）。
- [ ] 检查是否与 `pkg/accesskey` 既有文件重名/符号冲突（如已有 `credentialsFile` 之类私有类型）。
- [ ] 验证：全局验证命令 + 四条机械核对。
- [ ] 提交（分支 `refactor/credentialstore-to-accesskey`）。

**DoD：** 同 S1 的 ②–⑥；⑦ `go list ./... | grep credentialstore` 只出现在 `pkg/accesskey`。

### S2 实施记录（2026-09-13）

搬迁时发现两件必须记录的事实：

1. **`normalizeStorer` 不随 `CredentialStore` 走**。它原与 store 同文件，但用途是**装配层注入边界**的 typed-nil 归一（`opts.CredentialStore` 归一为真 nil，否则 `persistCredentials` 的 `== nil` 守卫失效）。实施取法：留在 `pkg/server`（新文件 `credentialstorer.go` + `credentialstorer_test.go`，用例名 `TestNormalizeStorer` 保留）。理由：不为此扩 `pkg/accesskey` 的公开 API，且它的消费者只有装配层一个。

2. **`seedTestRing` 在两侧各留一份**。它原定义在 `credentialstore_test.go`，但 `pkg/server` 还有 4 处用例（如 `credential_store_encrypt_test.go`）也用它；而 `pkg/accesskey` 不得反向依赖装配层 ⇒ 测试辅助无法跨包共享。实施取法：`pkg/accesskey/credentialstore_test.go` 侧保留一份（配本地 `testAccessSecretHex`），`pkg/server/server_test_common_test.go` 侧新增同构一份。

**顺带发现的独立议题（不在本片范围）**：`pkg/accesskey` 的 `EncryptingStorer` 在**明文模式**下的磁盘字节与 `CredentialStore` 一致（其注释原本就写「与 server.CredentialStore 字节一致」）——即同一个明文 JSON 格式有**两份实现**。归位后二者同包，重复变得显眼；是否收敛为「`EncryptingStorer` + 可选 `SecureStorer`（nil = 明文）」需要单独评估（涉及默认路径与配置语义），**本片不改**。

---

## 后续（不在本计划范围）

- **S3（`pkg/share`）：经评估结论为「不做」**——`ShareStore` 完全自包含但只有 1 个消费者，D2（可复用抽象埋在装配层）不成立，属 D3；纯搬迁无正确性驱动、也不减少注入面。证据与触发器见 `2026-09-13-server-domain-extraction-design.md` §9。
- **S4**：`pkg/cloud`（cloud 域）——**A 已交付（#203 `pkg/downloader` 顶层化）、B 已交付（#204 `pkg/cloud` 领域核心）**；**C（handler + `archive.go` 迁入）经评估结论为「不做」**：无第二消费者（Y-C 规格 cloud 提及 0 次）+ 5 个新增能力接口的消费者只有 handler（判据 D3，与 `pkg/files/chunked` 回炉同形态）⇒ 留装配层，`pkg/cloud` 保持零领域注入面的干净核心。证据、能力清单与触发器见 `2026-09-13-server-domain-extraction-design.md` §8。
