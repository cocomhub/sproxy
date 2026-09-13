# `pkg/server` 其余域抽取 设计

> 本规格解决 file-service 抽取工作留下的第 2 项遗留：`pkg/server` 其余职责（cloud / auth / config / share / volumes API / hub / stats / sync / archive / credentials）是否、以及按什么顺序再做「领域包抽取」。
> file-service 规格 §1 已明文把这些列为**非目标**；本规格给出**下一步的判据与排序**，并明确哪些**不做**。
> 实施见 `docs/superpowers/plans/2026-09-13-server-domain-extraction.md`（S1 / S2）。

**结论先行**：**驱动优先**。全仓实测只有**一条**硬驱动（分层倒置 `pkg/syncexec → pkg/server/syncmgr`）与**一条**应修（`CredentialStore` 违反 P2）。其余候选**无第二消费者**，按 P1「不要为了拆分而拆分」**留在装配层并记录理由**；最大的一块（cloud，4681 行）另立规格。

---

## 1. 判据（沿用 file-service 规格 P1–P6，加一条可执行的）

| 编号 | 判据 | 可执行证据 |
|---|---|---|
| **D1** | **分层倒置**：有非装配层的仓内包导入 `pkg/server/**` | `go list ./...` 反查 import，逐条列出 |
| **D2** | **P2 可复用抽象埋在装配层**：该抽象已有同域**顶层包**（接口/类型在顶层，实现却在 `pkg/server`） | 同域顶层包存在 + 消费者在别处 |
| **D3** | **只被装配层消费的 HTTP 面 / 中间件 / 管理 API** | 无第二消费者 ⇒ **留**（P1） |
| **P6** | 子包门槛：**① 可复用工具集合** 或 **② 真子领域**；「某功能的处理器 + 它的存储」**不属任何一类** | 该单元的类型集与生命周期 |

## 2. 实测清单（`go list` + 行数实测）

| 候选 | 非测试行 | 反向依赖 | 同域顶层 peer | 判定 |
|---|---|---|---|---|
| `pkg/server/syncmgr` | 1263（测试 1971） | **`pkg/syncexec`（全仓唯一倒置）** | `pkg/sync`、`pkg/syncexec` | **D1 ⇒ S1 必修** |
| `credentialstore.go` | 111 | 无 | `pkg/accesskey`（`CredentialStorer` 接口就在那） | **D2 ⇒ S2 应修** |
| `share.go`（`ShareStore`） | 506 | 无 | 无 `pkg/share`；`pkg/client` 有 share 客户端 | D2/P6② 候选 ⇒ **S3 可选** |
| `downloader/` | 1053（测试 1514） | 无 | `pkg/plugin`（可插拔下载器注册表） | **D3 + P6✗**：cloud 域的机制，随 cloud 走 |
| cloud（`cloud_download*.go` + `cloud_archive_handler.go`） | 2267+691+670 | 无 | `pkg/cloudfilename`、`pkg/provider` | D2 候选 ⇒ **S4 另立规格**（含 downloader 4681 行、16 条路由） |
| auth/credentials 处理器 | 2521 | 无 | `pkg/accesskey`/`otp`/`sproxysig` | D3 留 |
| hub / signal / relay / federation | 1583 | 无 | `pkg/tunnel/hub`、`pkg/tunnel/mesh` | D3 留（把 HTTP 面塞进 L1 的 `pkg/tunnel` 会倒置层级） |
| version / volumes_api / archive | 448 / 375 / 416 | 无 | registry 已抽 | D3 留（file-service 规格 §1 已明文） |
| stats / metrics / audit / config / cors / gzip / requestlog / ratelimit | ~1200 | 无 | `pkg/telemetry` | D3 留（这就是「server」本身） |

**D1 的实测命令与结果**：

```bash
go list -f '{{.ImportPath}}|{{join .Imports " "}}' ./... | \
  awk -F'|' '{n=split($2,a," "); for(i=1;i<=n;i++) if (a[i] ~ /cocomhub\/sproxy\/pkg\/server/) print $1" -> "a[i]}' | \
  grep -v '^github.com/cocomhub/sproxy/pkg/server'
# 输出仅一行：
# github.com/cocomhub/sproxy/pkg/syncexec -> github.com/cocomhub/sproxy/pkg/server/syncmgr
```

## 3. S1：`pkg/server/syncmgr` → `pkg/syncmgr`

### 3.1 为什么是必修

`pkg/syncexec` 是**领域包**（`syncmgr.Executor` 的实现，基于 `pkg/sync` 引擎），它导入**装配层** `pkg/server/syncmgr`。这条边：

- **能编译通过**，故 Go 编译器不管；
- **现有门禁一条都不响**——`pkg/syncexec` 不在 `Managed` 里，R1/R3 都不作用于它；`pkg/server/syncmgr` 不在 `ParentDomain` 里，R2 也不响。

这正是 file-service 规格 §8 里说的「编译器管不到的两类违规」的**第三种**：**领域包反向依赖装配层**。

### 3.2 目标

```go
// 顶层包：pkg/syncmgr（1263 行非测试 + 1971 行测试，纯搬迁）
// - 实测零仓内依赖：go list ./pkg/server/syncmgr 的 cocomhub 导入 = 0
// - 搬迁后：pkg/syncexec → pkg/syncmgr（L0）；pkg/server → pkg/syncmgr（装配注入）；cmd/sproxy → pkg/syncmgr
```

**包名决策：`pkg/syncmgr`（保留现名）**，而非 `pkg/sync/manager`：

- `pkg/sync` 已 import `pkg/client`；让它成为父域会把 `pkg/syncmgr` 拖成**子包**，从而落入 R2（只有 `pkg/sync` 子树与装配层可导入）——而 `pkg/syncexec` 与 `cmd/sproxy` 都要导入它，会在 R2 上再开例外，得不偿失。
- P5「包名现在改，类型名留后」：本片是**搬迁**，改包名会把「逐字不变」的可核对性打薄；`syncmgr` 本身也是领域名（同步**管理器**），不是实现形态名。

### 3.3 改动面（实测）

| 文件 | 改动 |
|---|---|
| `pkg/server/syncmgr/*.go` → `pkg/syncmgr/*.go` | 纯搬迁（含测试，包名 `syncmgr` 不变） |
| `pkg/server/handlers.go`、`sync_handler.go`（+ 各自 `_test.go`） | 导入路径 |
| `pkg/syncexec/executor.go`（+ 2 个 `_test.go`） | 导入路径（**本片的存在理由**） |
| `cmd/sproxy/root.go` | 导入路径（子 module） |
| `internal/archcheck/layers.go` | 登记 `pkg/syncmgr` |

## 4. 门禁增强（本规格的可执行产物）

### R4（新增）：领域包不得导入装配层

```go
const assemblyRoot = modulePrefix + "pkg/server"

// TestNoDomainImportsAssembly：pkg/** 下非装配层的包不得导入 pkg/server/**。
func TestNoDomainImportsAssembly(t *testing.T) { ... }
```

**先写 R4 → 测试红（`pkg/syncexec` 违规）→ 搬迁 → 绿**。这是本片最干净的可执行验收。

### R5（新增）：`Levels` 全表化

今天 `Levels` 只登记 9 个包（5 个 `Managed` + 4 个基础），R1 只在这 9 个之间生效。把**全部 24 个顶层 `pkg/*` 包**登记为“依赖深度”分组后，R1 能普遍拦下编译器管不到的低层→高层倒置。

实测可用的分组（**由当前 DAG 的深度导出，且已逐条验证零违规**）：

| 组 | 包 |
|---|---|
| **G0 基础库**（零 `pkg/*` 内部依赖） | `accesskey` `certmgr` `checksum` `cli` `cloudfilename` `iostream` `otp` `pathguard` `plugin` `provider` `quota` `sproxysig` `storage` `store` `telemetry` `testutil` `volume` |
| **G1 领域包** | `files` `socks5` `tunnel` |
| **G2 装配层** | `client` `server` |
| **G3 装配之上的消费者** | `sync` |
| **G4** | `syncexec` |

> 与现状表的一致性已逐条核对：`pkg/storage/capacity`(2) ← checksum/storage/quota；`pkg/volume/registry`(2) ← quota/storage/volume；`pkg/files`(G1) 只导入 G0 ⇒ 不导入两个子包（R2 亦如此要求）；`pkg/server`(G2) 可导入同层的 `capacity`/`registry`（R1 允许同层）。
>
> **注意**：现状表里 `pkg/files` 记为 3、`pkg/storage` 记为 1；全表化时按本表统一口径（`files`=G1、`storage`=G0）。这只是编号变化，**不改变任何约束的松紧**（已验证零违规）。

## 5. S2：`credentialstore.go` → `pkg/accesskey`

`CredentialStore` 是 `accesskey.CredentialStorer` 的**实现**（`<tenant>/meta/credentials.json`，原子写 + `saveMu` 串行化），接口在 `pkg/accesskey`、实现在 `pkg/server` —— 典型的 P2 违反（可复用抽象埋在装配层）。111 行 + 测试纯搬迁，`pkg/server` 只留装配。

## 6. 不做（记录理由）

| 不做 | 理由 |
|---|---|
| auth / credentials 的 HTTP 面 | D3：`/api/credentials` 是服务端管理 API，只有装配层消费；`pkg/accesskey` 已是其领域抽象，处理器属装配 |
| hub / signal / relay / federation 的 HTTP 面 | D3 + 层级：它们是 hub 域的 **HTTP 面**；`pkg/tunnel/hub` 在 L1，把 HTTP 面塞进去会倒置层级（`pkg/tunnel` 已 import `accesskey`，再依赖 `net/http` 服务端栈不合适） |
| version / volumes_api / archive | D3：file-service 规格 §1 已明文列为非目标；archive 是 cloud 域的**机制**（P6✗），随 S4 走 |
| stats / metrics / audit / config / cors / gzip / requestlog / ratelimit | D3：这些**就是**「server」——中间件与管理 API |
| `pkg/server/downloader` 单独成包 | P6✗：它是 cloud 域的下载机制（处理器+其存储的同类形态），单独成包只会制造跨包注入面；随 S4 一起进 `pkg/cloud` |
| S3（`pkg/share`） | D2/P6② 成立（`ShareStore` 有 token/密码/过期/`Stop()` 生命周期），但**无第二消费者**，收益仅是缩小 `pkg/server` ⇒ 列为可选，需另行确认 |
| S4（`pkg/cloud`） | 4681 行、16 条路由、任务生命周期 + 持久化 + 恢复扫描 ⇒ **另立规格与计划**（按 file-service 的 A/B/C/D 阶段法），不与 S1/S2 混在一个计划 |

## 7. 风险与取舍

| 风险 | 处置 |
|---|---|
| 搬迁面含子 module（`cmd/sproxy`） | 根 `go build ./...` **不覆盖**子 module ⇒ 必须跑 `make build-all`；`make lint-all` 同理 |
| 测试基座跨包耦合 | `pkg/server/syncmgr` 的测试**全部随包搬走**（其测试不依赖 `pkg/server` 测试基座——实测 `go list` 的 cocomhub 导入为空）；若搬迁后发现依赖，则该用例留在 `pkg/server` 并改名迁入 `sync_handler_test.go`（不违反核对 ②：用例名守恒） |
| R5 全表化误伤后续演进 | 表是**顶层包冻结契约**：新增顶层包必须登记（R3 已强制 `Managed`），跨组新边若确有正当理由，改表并写明理由即可 |
| 用例名核对 | `pkg/server/syncmgr/*_test.go` 的用例名在搬迁后仍存在（改名迁移允许，丢失不允许） |

## 8. S4-C 评估结论：**不做**（附触发器）


S4-C = 把 `cloud_download_handler.go` + `cloud_archive_handler.go` + `archive.go`（1361 + 416 行）迁入 `pkg/cloud`，按 `pkg/files` 的「能力接口 + Option」模式接缝。

**评估用两条实测证据**：

1. **无第二消费者**。`pkg/files` 抽取的驱动是 Y-C（集群读）要挂载文件服务——`2026-09-11-y-cluster-read-design.md` 中 **cloud 提及 0 次**；cloud 路由处理器的引用点全部在 `pkg/server` 自身（`handlers.go` 注册 + 两个 handler 互调）。
2. **注入面全由处理器驱动**。S4-C 需新增 5 个能力接口，其消费者**只有这些 handler**，`pkg/cloud` 现有的管理器一个都不需要：

| 能力接口 | 对应现状 | 消费者 |
|---|---|---|
| `Auditor`（`Record`） | `h.RecordAudit` ×4 | 仅 cloud handler |
| `ArchiveUsage` | `h.recordArchiveUsage` / `h.deleteCloudArchive` / `h.archiveUsage` map + `h.tenantMu` 直改（×12） | 仅 cloud handler |
| `QuotaScopes` | `h.quotaBucketFor` ×4 | 仅 cloud handler |
| `ArchiveLimits` | `h.cloudArchiveMaxBytes` ×3 + `h.cfgPtr` ×1 | 仅 cloud handler |
| `VolumeRouter` | `h.volSet`/`h.volumeTenant`/`h.volumeFileExists`/`h.locateOwnerFile`/`h.defaultVolumeAllows`/`h.archiveFileRootFor`（`archive.go` ×10） | 仅 cloud/archive handler |

**结论**：这与 **`pkg/files/chunked` 回炉时的实测形态同类**（注入面 100% 由处理器驱动、存储侧 0 需求），属于判据 **D3**（只被装配层消费的 HTTP 面 → 留装配层）。同时 `pkg/cloud` 当前是**零领域注入面的干净核心**（只依赖 G0 包），S4-C 会让它变成「带 5 个能力接口的服务域」——**净增复杂度**换 10% 的行数转移。

**触发器（满足任一条再做）**：
- 出现第二个消费者（例如集群/远程侧要挂载云端下载路由，或 C 期的 remote 面复用云端任务体）；
- 或 `pkg/server` 的 cloud HTTP 面继续膨胀到超过领域核心（当前 1361 vs 2267）。

届时**能力接口清单已备好**（上表），可直接按 `files` 的 Option 模式实施，无需重新勘察。

## 9. S3（`pkg/share`）评估结论：**不做**（附触发器）

`ShareStore`（`pkg/server/share.go` 的 24–256 行，约 230 行）实测：**完全自包含**（其方法内 `h.` 引用数 = **0**），依赖仅 stdlib + `internal/shortid`，消费者只有 `pkg/server`（`handlers.go` 装配 + 4 个 handler）。

**不做**的理由：
- **D2 不成立**：D2 的判据是「**可复用**抽象埋在装配层」。对照先例：`checksum.ChecksumStore` 被抽出是因为它有**跨域消费者**（files + cloud + capacity + version）；`ShareStore` 只有 1 个消费者，属「服务端 share API 面的存储」（判据 **D3**）。
- **P1**：抽取是纯搬迁、无正确性驱动、也不减少任何注入面（handler 留在装配层，`h.shareStore` 换成 `share.Store` 而已）⇒ 「为了拆分而拆分」。
- 与 S4-B 的区别：S4-B 的 `cloud_download.go` 是 2267 行的**领域核心**（任务/分组状态机 + 持久化 + 恢复 + TTL），体量与内聚都到了值得独立门户的程度；`ShareStore` 230 行且无同族体量。

**触发器（满足任一条再做）**：出现第二个消费者（客户端之外的服务端模式、集群/federation 共享 share 记录）；或 share 域扩展到「独立领域概念 + 独立生命周期」且同族体量 > 500 行。
