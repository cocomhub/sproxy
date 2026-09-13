# `pkg/cloud` 云下载域抽取 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 按已确认的推荐实施 **S4-A**（`pkg/downloader` 顶层化）与 **S4-B**（`pkg/cloud` 领域核心）；**S4-C**（HTTP 面接缝化）不在本计划范围，另立计划。

**架构：** A 是零风险纯搬迁（`downloader/` 只依赖 `pkg/plugin`）；B 是「领域核心搬出 + 消费方窄接口（`cloud.StorageManager`）+ 导出唯一被跨包访问的私有成员」，HTTP 处理器暂留装配层作为薄驱动。

**技术栈：** Go 1.26；纯标准库；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-13-cloud-extraction-design.md`

---

## 全局约束

- **逐字不变**：见规格 §5 的 8 条硬约束。**不允许顺带的"小修小补"**——发现缺陷写进报告，由控制者落进规格。
- **包名按领域命名；类型名不改**（`CloudDownloadManager` → 迁入 `cloud` 包后**保留原名**，不做 `cloud.DownloadManager` 式改名——P5）。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues**（前者根 module、后者 10 个子 module，**互补缺一不可**）。
- 提交时**只 `git add` 本任务改动的文件**；message 用**多重 `-m`**；**不加署名行**。
- **不要改动计划/规格文件**：经验与口径修正写进报告，由控制者落进计划。
- 每次 Bash 调用在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`。
- **工作分支：每片一个分支，从最新 master 切出**，该片 PR 合并后再开下一片。
- **`go build ./...` 只覆盖根 module**；**必须**另跑 **`make build-all`**（10 个子 module）。
- **archcheck 登记**：新顶层包必须**同时**写入 `Levels` 与 `Managed`；子包另需 `ParentDomain`。本计划两个目标包都是顶层包。
- **注释里的路径引用会随搬迁变陈旧**：`test/` 下有两处注释引用 `pkg/server/downloader/http_downloader.go:167`。核对 ① 要求 `test/` 零改动——**本次对 `test/` 的改动仅限这两行注释路径**，必须在报告中如实披露并贴出 `git diff test/` 全文（证明非行为改动）。

## 四条机械核对（每片必跑）

```bash
git diff --stat db99de06 -- test/          # ① 应为空（本计划 S4-A 例外：仅注释路径，须附 diff）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)   # ② 必须无 "-" 行
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)   # ③ 必须为空
go test ./internal/archcheck/               # ④
```

## 验证命令（每片必跑）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
go test -race -count=1 ./pkg/cloud/ ./pkg/downloader/
make test-e2e
```

---

## 任务 S4-A：`pkg/server/downloader` → `pkg/downloader`（顶层）

**规格：** §3（归属判定）、§4（S4-A 行）。

- [ ] `git mv pkg/server/downloader pkg/downloader`（含 `http_downloader_test.go`、`registry_test.go`、`ssrf_test.go`）。
- [ ] 包文档/注释核对：`pkg/downloader/*.go` 中若有「pkg/server」措辞，改为顶层包口径。
- [ ] 改导入路径（以 `go build ./...` + `make build-all` 报错为准）：`pkg/server/cloud_download.go`、`pkg/server/cloud_download_handler.go`；`test/e2e_cli_cloud_test.go`、`test/e2e_cli_harness_test.go`（**仅注释路径**）。
- [ ] `internal/archcheck/layers.go`：`Managed` += `pkg/downloader`；`Levels` += `pkg/downloader: 0`（G0，只依赖 `pkg/plugin`）；不是子包 ⇒ **不写** `ParentDomain`。
- [ ] 验证：全局验证命令 + 四条机械核对（核对 ① 附 `git diff test/` 全文）。
- [ ] 提交（分支 `refactor/extract-downloader`）：spec + plan + 本任务文件。

**DoD：** ① 四条核对全过（① 的例外已披露）；② `make lint`/`lint-all` 0 issues；③ `go build ./...`/`make build-all` 过；④ `go test ./pkg/... ./internal/...` 过；⑤ `-race ./pkg/downloader/` 绿；⑥ e2e 过。

---

## 任务 S4-B：`pkg/cloud` 领域核心

**规格：** §2（三个硬约束）、§3、§5。

- [ ] 新建 `pkg/cloud/manager.go` ← `pkg/server/cloud_download.go`（`git mv` 后改 `package cloud`）：
      - 删掉对 `pkg/storage/capacity` 的导入，新增窄接口 `StorageManager`（`TryReserveCloud`/`ReleaseCloud`/`Usage`/`MaxBytes`）与 `ErrStorageFull` 哨兵（若 `Manager` 内需要判断容量不足；实测 `capacity.ErrStorageFull` 的使用在 handler 侧，见下）；
      - `NewCloudDownloadManager` 的 `sm *capacity.StorageManager` → `sm StorageManager`；内部 `m.storage.TryReserve(x, capacity.CategoryCloud)` → `m.storage.TryReserveCloud(x)`、`Release` → `ReleaseCloud`；
      - 新增 `func (m *Manager) Metrics() *CloudMetrics`（导出唯一被跨包访问的私有成员）；
      - **类型名与其余函数体逐字不变**。
- [ ] `pkg/server/cloud_service.go`（新，仿 `files_service.go`）：`cloudStorageManager` 适配器把 `*capacity.StorageManager` 适配为 `cloud.StorageManager`，类别固定 `capacity.CategoryCloud`；`CloudTask` 相关错误映射（`capacity.ErrStorageFull`）留在装配层。
- [ ] `pkg/server`：`h.cloudMgr *cloud.Manager`；`handlers.go` 装配处 `cloud.NewCloudDownloadManager(...)` + `cloudStorageManager{sm}`；`Close` 调 `h.cloudMgr.Close()`；`metrics.go` 改 `cm.Metrics()`。
- [ ] `pkg/server` 的其余引用（实测：`cloud_download_handler.go` 1 处、`handlers.go` 5 处、9 个测试文件约 109 处）加 `cloud.` 限定（以 `go build`/`go vet` 报错为准）。
- [ ] **测试迁移**：`pkg/server/cloud_download_test.go` → `pkg/cloud/manager_test.go`（48/53 用例用 helper，只有 4 行读 `h`）：
      - 在 `pkg/cloud` 建等价的测试基座 `newCloudTestManager(t, root, sm StorageManager, cfg *Config) (*Manager, *cloudTestEnv)`；
      - `cloudTestEnv` 提供测试需要的 `checksumStoreFor` / `quotaBucketFor` / `quotaFor`（对应原 `h.*` 的 4 处使用）；
      - **测试内**可导入 `pkg/storage/capacity` 构造真实 `StorageManager`（R2 只约束生产代码），配一个测试内小适配器；
      - `testLogger` → 本地 helper 或 `pkg/testutil.DiscardLogger()`。
      - 用 `newTestServerWithAllRoutes`/`newOwnerEnv`/`newAssemblyTestHandlers` 的用例（约 39 个）**留在 `pkg/server`**，改名迁入对应装配层测试文件（核对 ② 允许改名、禁丢失）。
- [ ] 验证：全局验证命令 + 四条机械核对；`pkg/cloud` 覆盖率零覆盖函数 = 0。
- [ ] 提交（分支 `refactor/extract-cloud-core`）。

**DoD：** 同 S4-A，且额外：⑦ 16 条 `/api/cloud/*` 路由逐条一致（核对 ③）；⑧ 任务/分组持久化与 TTL/恢复语义由既有用例钉住并通过；⑨ `pkg/cloud` 生产代码**不导入** `pkg/storage/capacity`（`go list` 可验）；⑩ `pkg/cloud` 在 `-race` 下通过。

---

## 后续（不在本计划范围）

- **S4-C（可选，另立计划）**：两个 handler + `archive.go` → `pkg/cloud`，按 `files` 的「能力接口 + Option」模式（能力清单草案：`StorageManager` / `ArchiveUsage` / `QuotaBuckets` / `Auditor` / `TenantResolver` / `ChecksumLedgers` / `ConfigProvider` / `Logger` 取用函数）。**动手前先出能力接口清单并评估「是否为拆分而拆分」**（file-service 的 `pkg/files/chunked` 回炉即此教训）。
- **`pkg/storage/capacity` 是否升为顶级包**：file-service 规格 §3.3 记的长期选项；与本计划无关。
