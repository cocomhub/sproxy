# 租户解析下沉 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法跟踪进度。

**目标：** 把租户「懒创建 + 缓存 + 失败关闭」的公共骨架与 `owner` 规范化下沉到 `pkg/storage`，消除 `pkg/server` 与 `pkg/volume/registry` 的同构副本，并让 `*storage.TenantCache` 结构上直接满足 `files.TenantResolver`。

**架构：** 先在基础包新增能力（T1，缓存不动，回归面最小），再让两处消费方切到新缓存（T2，动关闭顺序与装配接线）。全程**行为逐字不变**。

**技术栈：** Go 1.26；纯标准库（`log/slog`、`sync`、`os`）；不新增依赖。

**规格：** `docs/superpowers/specs/2026-09-13-tenant-resolver-design.md`

---

## 全局约束

- **Go 1.26**；**纯标准库**；不新增任何依赖。
- **行为逐字不变**：见规格 §6 的 7 条硬约束。**不允许顺带的"小修小补"**——发现缺陷记入报告，由控制者落进规格。
- **测试纯标准库**（`t.Fatalf`/`t.Errorf`）；**只绑 `127.0.0.1`**（禁 `0.0.0.0`/`localhost`）。
- 源码带 **SPDX 头**；注释用简体中文；注释必须与实测一致。
- **lint 必须跑 `make lint` 与 `make lint-all` 两者，均 0 issues**（前者覆盖根 module，后者只覆盖 10 个子 module，**互补缺一不可**）。
- 提交时**只 `git add` 本任务改动的文件**（禁止 `git add -A`/`.`）；message 用**多重 `-m`**；**不加任何署名行**。
- **不要改动计划/规格文件**：实施中的经验、口径修正写进报告，由控制者落进计划。
- 每次 Bash 调用在同一命令内 `export PATH="$PATH:$(go env GOPATH)/bin"`（否则 pre-commit 的 addlicense 失败或 lint 静默跳过）。
- **工作分支：每片一个分支，从最新 master 切出**，该片 PR 合并后再开下一片。
- **`go build ./...` 只覆盖根 module**；跨 module 必须另跑 **`make build-all`**。两者都过才算"编译器确认"。
- **调用点清单是 grep 估计值，编译器才是权威**：以 `go build ./...` 报错逐条加减，报告里写明实际数量与偏离原因。
- **archcheck 登记**：本计划**不新增包**（`pkg/storage` 已有），故无需新增 `Levels`/`Managed`/`ParentDomain` 条目；但要确认 `go test ./internal/archcheck/` 仍绿（尤其是 `pkg/storage` 新增 `log/slog` 不影响规则）。

## 四条机械核对（每片必跑）

```bash
# ① e2e 零改动（必须输出为空）
git diff --stat db99de06 -- test/
# ② 用例名零丢失（必须没有 "-" 行；新增允许）
diff <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' db99de06 -- '*_test.go' | sort) \
     <(git grep -hE '^func (Test|Fuzz|Benchmark|Example)' -- '*_test.go' | sort)
# ③ 路由表逐条一致（必须输出为空）
diff <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' db99de06 -- '*.go' ':(exclude)*_test.go' | sort) \
     <(git grep -hE 'HandleFunc\("[A-Z]+ [^"]*"' -- '*.go' ':(exclude)*_test.go' | sort)
# ④ 分层/子包门禁
go test ./internal/archcheck/
```

## 验证命令（每片必跑）

```bash
export PATH="$PATH:$(go env GOPATH)/bin"
gofmt -l pkg/ && go build ./... && make build-all
make lint && make lint-all
go test -count=1 ./pkg/... ./internal/...
go test -race -count=1 ./pkg/files/ ./pkg/volume/...
make test-e2e
```

---

## 任务 T1：`pkg/storage` 租户解析骨架下沉（缓存不动）

**交付物：** `pkg/storage/tenant_resolve.go` + `tenant_resolve_test.go`（`OpenTenant` 与 `TenantCache` 的**定义与自测**，**不切换消费方**）；`pkg/server`、`pkg/files` 的 `owner` 规范化改引用。

**规格：** §3.1、§3.2（仅创建部分）、§6。

- [ ] 新建 `pkg/storage/tenant_resolve.go`：`AnonymousOwner`、`NormalizeOwner`、`TenantOption`（`WithMetaBucket`/`WithLogger`/`WithLogAttrs`）、`OpenTenant`、`ListOwners`，以及 `TenantCache`/`NewTenantCache`/`TenantFor`/`Close`（**本任务只新增并自测，不切换任何消费方**——`Handlers.tenantRoots` 与 `Set.tenants` 保持原样，留给 T2）。
      **实现来源**：`OpenTenant` 的骨架逐字取自 `pkg/server/handlers.go:266-316`（`h.tenantFor`），把 `h.globalRoot` 换成 `parent`、`h.logger` 换成选项 logger；`ListOwners` 逐字取自 `h.listTenantIDs`（`handlers.go:327-360`）。
- [ ] 新建 `pkg/storage/tenant_resolve_test.go`（纯标准库，`t.TempDir()`）。最小覆盖：
      - `OpenTenant` 成功：目录被创建、返回租户可用；
      - `WithMetaBucket()`：`meta` 桶存在；不加则该桶**不存在**；
      - 非法段名（如 `".."`、Windows 保留字）⇒ `(nil, err)` 且**不创建**目录；
      - `parent == nil` ⇒ `(nil, err)`（绝不回落）；
      - **句柄不泄漏**：成功打开后 `Close` 掉返回的根与父根，`os.RemoveAll` 能成功（Windows 上句柄未关会失败）——这是查失败路径关闭逻辑的可执行探针；
      - `NormalizeOwner("") == AnonymousOwner`；`NormalizeOwner("alice") == "alice"`；
      - `ListOwners`：跳过 `__` 前缀目录与非法段名目录、按名排序、`parent == nil` 返回 nil；
      - `TenantCache`：命中复用同一实例、非法 owner/`parent == nil` 返回 nil、并发 `TenantFor` 只建一个实例（`-race`）、`Close` 幂等且**不关 parent**、`Close` 后可重建、nil 接收者安全。
- [ ] `pkg/server`：`normalizeOwner` 改委托 `storage.NormalizeOwner`；`anonymousOwner` 改引用 `storage.AnonymousOwner`（保留本包常量别名或直接替换，以编译报错量最小为准）。
- [ ] `pkg/server`：`h.tenantFor` 的**函数体**改为「缓存查找（保持 `h.tenantMu`/`h.tenantRoots`）+ `storage.OpenTenant(h.globalRoot, owner, storage.WithMetaBucket(), storage.WithLogger(h.logger))`」；`h.listTenantIDs` 体改为 `storage.ListOwners(h.globalRoot)`。
- [ ] `pkg/files`：`anonymousOwner`/`normalizeOwner` 改委托/引用 `storage`（同 `pkg/server` 口径）。
- [ ] `pkg/server/helper_impl_drift_test.go`：把 `normalizeOwner` 的 parity 守卫替换为**委托守卫** `TestNormalizeOwner_DelegatesToStorage`（断言 `pkg/files/service.go` 与 `pkg/server/handlers.go` 两侧的 `normalizeOwner` 体都含 `storage.NormalizeOwner(`），并同步该文件顶部的守卫清单注释与 `normalizeProbeRe` 常量移除。
- [ ] 验证：全局验证命令 + 四条机械核对。
- [ ] 提交（分支 `refactor/tenant-resolve-sink`）：spec + plan + 本任务文件。

**DoD：** ① 四条机械核对全过；② `make lint`/`lint-all` 0 issues；③ `go build ./...`/`make build-all` 过；④ `go test ./pkg/... ./internal/...` 过；⑤ e2e 过；⑥ 新增测试覆盖 `OpenTenant` 的每条失败路径。

---

## 任务 T2：`storage.TenantCache` 与两处消费方切换

**交付物：** `storage.TenantCache`；`registry.Set` 每卷缓存；`Handlers` 改持缓存；`files.New(h.tenants)`；文档收口。

**规格：** §3.1（缓存部分）、§3.2、§3.3、§5（T2 行）。

- [ ] 在 `pkg/storage/tenant_resolve.go` 中**切换消费方**（`TenantCache` 本身已在 T1 落地并自测）：语义见规格 §3.1 与 §6 第 6 条（**不关 parent**）。
- [ ] （T1 已覆盖 `TenantCache` 的单元用例，本任务只补消费方集成点，无需重复。）
- [ ] `pkg/volume/registry/set.go`：`tenants map[string]*storage.Tenant` + `tenantMu` → `caches map[string]*storage.TenantCache`；`Set.Tenant(volName, owner, log)` 保留签名，首次为某卷创建 cache 时以 `log` 作 `WithLogger`（后续忽略，与现状「首次创建才记日志」一致，见规格 §7）；`Set.Close` 遍历 `caches` 关闭。**删除** `Set.Tenant` 内的创建骨架。
- [ ] `pkg/server/handlers.go`：字段 `tenantRoots map[string]*storage.Tenant` → `tenants *storage.TenantCache`；`tenantFor(owner)` 变成一行 `return h.tenants.TenantFor(owner)`；`listTenantIDs` 一行转发；`Close` 中「遍历 tenantRoots 关闭」改为 `h.tenants.Close()`（**顺序保持：先租户、后 `volSet.Close()`**）；`RegisterRoutes` 装配处 `h.tenants = storage.NewTenantCache(h.globalRoot, storage.WithMetaBucket(), storage.WithLogger(h.logger))`（在 `h.globalRoot` 赋值之后）。
      **注意**：`h.tenantMu` 仍保护 `checksumStores`/`uploadStores`/`quotaScopes`/`quotaBuckets`/`archiveUsage`，**不得删除**。
- [ ] `pkg/server/files_service.go`：`filesRuntime` 删除 `TenantFor` 方法；`fileService()` 改为 `files.New(h.tenants, ...)`（`WithTenant` 不再需要——`*storage.TenantCache` 直接满足 `files.TenantResolver`）。
- [ ] `pkg/server/helper_impl_drift_test.go`：归一化规则同步（若 `files.New(h.tenants` 形式变化影响规则，按编译与用例结果调整）。
- [ ] 文档：`pkg/files/service.go` 包文档、`pkg/storage` 相关注释、`docs/superpowers/specs/2026-09-12-file-service-extraction-design.md` 的接缝描述（如引用 `filesRuntime.TenantFor` 则更新）。
- [ ] 验证：全局验证命令 + 四条机械核对；`registry` 用例名守恒；`pkg/files` 覆盖率零覆盖函数仍为 0。
- [ ] 提交（分支 `refactor/tenant-cache-wiring`）。

**DoD：** 同 T1，且额外：⑦ 多卷 + 默认卷关闭路径由 e2e 或既有集成测试覆盖并通过；⑧ `close` 后 `parent` 仍可用有显式用例；⑨ `pkg/storage` 仍零 `pkg/*` 内部依赖（`go list` 可验）。
