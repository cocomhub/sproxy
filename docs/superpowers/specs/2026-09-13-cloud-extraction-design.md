# `pkg/cloud` 云下载域抽取 设计

> 本规格解决 server 域抽取工作的 **S4**：把云下载域（任务管理器 + 可插拔下载器机制 [+ HTTP 面]）从装配包 `pkg/server` 抽出。
> 判据与清单见 `docs/superpowers/specs/2026-09-13-server-domain-extraction-design.md` §2（D2/P6 类）。

**目标（按已确认的推荐）**：做 **S4-A**（`pkg/downloader` 顶层化）与 **S4-B**（`pkg/cloud` 领域核心）；**S4-C**（HTTP 面接缝化）列为可选、另立计划。

**结论先行**：S4 **无 D1 类正确性驱动**（仓内无任何非装配层包括入 cloud 代码），驱动是**体积与领域内聚**。技术可行性**高**：管理器已自我领域化（自定窄函数类型与配置类型、**跨包私有访问仅 1 处**）。

---

## 1. 实测证据

### 1.1 规模与耦合

| 单元 | 生产行 | 测试行 | 用例 | 对 `Handlers` 的耦合（`\bh\.` 精确匹配） |
|---|---|---|---|---|
| `cloud_download.go`（Manager/Task/Group/Config/Metrics） | **2267** | 2115 | 53 | **6**：`tenantFor`×2、`checksumStoreFor`×2、`quotaFor`、`listTenantIDs` |
| `downloader/`（4 生产文件） | 1053 | 1514 | — | 0（独立子包） |
| `cloud_download_handler.go` | 691 | 932 | 26 | 10 种：`cloudMgr`×33、`logger`×10、`storageMgr`×7、`RecordAudit`×4… |
| `cloud_archive_handler.go` | 670 | 662 | 14 | 12 种：`logger`×19、`storageMgr`×14、`tenantMu`×6、`archiveUsage`×6… |
| `archive.go`（tar 机制） | 416 | — | — | 0（仅被上面两个 handler 调用） |
| 路由 / `cloudCfg` 装配 / `Close` | ~60 | — | — | 装配层本分 |

`/api/cloud/*` 共 **16 条唯一路由**（tasks/groups 的 CRUD + cancel/resume/archive）。

### 1.2 依赖（决定目标包能放哪）

| 文件 | 仓内依赖 |
|---|---|
| `cloud_download.go` | `checksum`、`cloudfilename`、`quota`、`storage`、**`storage/capacity`（子包）**、`server/downloader` |
| `cloud_download_handler.go` | `cloudfilename`、`quota`、**`storage/capacity`**、`server/downloader` |
| `cloud_archive_handler.go` | `quota`、`storage`、**`storage/capacity`** |
| `downloader/` | **只有 `pkg/plugin`** |

### 1.3 已存在的对称与干出

- `pkg/cloudfilename`（云端文件名解析）、`pkg/provider`（提供者抽象）**已是顶层包**；
- `pkg/client/cloud.go` + `chain_cloud_download*.go` —— **客户端侧已就位**（服务端/客户端对称）；
- `CloudDownloadConfig` 与 `CloudMetrics` **已定义在 `cloud_download.go`**（不在 `config.go`）。

---

## 2. 三个硬约束与处置

### 约束 1：R2 阻断 `pkg/storage/capacity`

三个文件都 import `pkg/storage/capacity`（子包，`ParentDomain=pkg/storage`）。按 R2，`pkg/cloud` **不得**直接导入它。

**处置**：声明消费方窄接口（与 `files.StorageManager` 同构先例），类别常量由装配层适配器固定：

```go
// pkg/cloud/manager.go
type StorageManager interface {
	TryReserveCloud(size int64) error
	ReleaseCloud(size int64)
	Usage() int64
	MaxBytes() int64
}
```

实测 cloud 对 capacity 只用 `TryReserve`/`Release`/`Usage`/`MaxBytes` 四个方法 + `CategoryCloud` 常量，接口面很窄。

> **被否决的替代方案**：把 `pkg/storage/capacity` 提升为顶级包 `pkg/capacity`（file-service 规格 §3.3 记的长期选项）。**本片不做**——它会连带动 `registry` 与 `files` 的既有窄接口设计（`files` 刻意用窄接口而非直连），收益（少 10 行适配器）远小于代价。

### 约束 2：唯一的跨包私有访问

`pkg/server/metrics.go` 读 `h.cloudMgr.metrics`（**未导出字段**，仅因同包才可访问）。移出后须导出访问器 `func (m *Manager) Metrics() *CloudMetrics`。

**积极信号**：实测跨包私有访问**仅此一处**（`.tasks`/`.config`/`.logger`/`.storage`/`.semaphore` 从包外访问均为 0）⇒ 管理器已是「对外只有方法」的领域对象。

### 约束 3：测试基座在装配层

| 测试文件 | 用例 | 基座 | 可搬迁性 |
|---|---|---|---|
| `cloud_download_test.go` | 53 | 仅 `newCloudTestManager` | ✅ 域级用例（48/53 用 helper；只有 4 行读 `h`） |
| `cloud_download_handler_test.go` | 26 | `newAssemblyTestHandlers` + helper | 部分 |
| `cloud_owner_test.go` | 12 | 同上 | 部分 |
| `cloud_quota_writer_test.go` | 6 | helper + `newOwnerEnv` | 部分 |
| `cloud_archive_handler_test.go` | 14 | `newTestServerWithAllRoutes` + `newOwnerEnv` | ❌ 留装配层 |
| `cloud_archive_download_test.go` | 12 | `newTestServerWithAllRoutes` | ❌ 留装配层 |
| `cloud_newlayout_test.go` | 1 | `newOwnerEnv` | ❌ 留装配层 |

**R2 只约束生产代码**（archcheck 解析 `go list` 的非测试导入集）⇒ 迁入 `pkg/cloud` 的测试**仍可**导入 `pkg/storage/capacity` 构造真实 `StorageManager`（需要一个测试内的小适配器）。

---

## 3. 归属判定（逐单元）

| 单元 | 判定 | 依据 | 目标 |
|---|---|---|---|
| `cloud_download.go` | **P6② 真子领域** ✅ | 自有领域概念（Task/Group 状态机）、自有生命周期（并发信号量、TTL 清理、重启恢复、`<tenant>/cloud/` 持久化）、自有窄接口与配置 | **`pkg/cloud`** |
| `downloader/` | **P6① 可复用扩展工具集合** ✅ | 插件注册表（`plugin.Plugin[T]`）+ 实现；**只依赖 `pkg/plugin`** | **`pkg/downloader`（顶层）** |
| `cloud_download_handler.go` / `cloud_archive_handler.go` | 领域 HTTP 面（D2） | 与领域同族，但需能力接缝 | `pkg/cloud`（**C 期**） |
| `archive.go` | **P6✗ 机制** | 仅被 cloud 两个 handler 消费（`commonArchiveName`/`validateArchiveFiles`/`addFileToTar`）；与 `chunked` 之于 `files` 同类 | 随 cloud（**C 期**） |
| 路由注册 / `cloudCfg` 装配 / `Close` | 装配层 | — | 留 `pkg/server` |

**包名决策**：`pkg/cloud`（领域名，P3）；**类型名全保留**（P5）。文件 `cloud_download.go` → `pkg/cloud/manager.go`（包内再叫 `cloud_download` 是冗余；内容是 Manager + Task/Group + Config + Metrics）。

---

## 4. 分期

| 片 | 内容 | 生产行变化 | 风险 |
|---|---|---|---|
| **S4-A** | `pkg/server/downloader` → **`pkg/downloader`**（顶层，纯搬迁）；archcheck 登记 | `pkg/server` −1053 | 极低（1 条依赖边、0 耦合） |
| **S4-B** | `cloud_download.go` → **`pkg/cloud`**；加 `cloud.StorageManager` 窄接口 + `Manager.Metrics()`；`h.cloudMgr *cloud.Manager`；**两个 handler 暂留装配层**；域级测试迁 `pkg/cloud` | `pkg/server` −2267 | 中低（耦合 6、私有访问 1、测试基座需重建） |
| **S4-C（可选，另立计划）** | 两个 handler + `archive.go` → `pkg/cloud`，按 `files` 的「能力接口 + Option」模式；路由与 HTTP 集成测试留装配层 | `pkg/server` −1777 | 中高（接缝成本占 90%；须先出能力接口清单） |

**收益量化（实测）**：`pkg/server` 生产行 17,124 →（A）16,071 →（B）**13,804（−19%）** →（C）12,027（−30%）。

---

## 5. 行为不变的硬约束（实施与审查清单）

1. 任务/分组的持久化路径与文件名不变（`<tenant>/cloud/tasks/<id>/…`、groups 文件）。
2. 任务状态机与状态字面值不变；`TaskStatus`/`GroupStatus` 常量值不变。
3. 配额语义不变：创建期 `TryReserve(CategoryCloud)`、完成期差分结算、失败/取消/删除归还。
4. TTL 清理与重启恢复的触发时机与扫描范围不变。
5. `Close()` 的停止顺序与幂等性不变。
6. 配置默认值（`applyCloudConfigDefaults`）不变。
7. 日志级别与关键字段名不变（`task_id`/`owner`/`error`…）。
8. HTTP 契约为零改动：16 条路由逐条一致，状态码/响应体不变。

## 6. 风险与处置

| 风险 | 评级 | 处置 |
|---|---|---|
| R2 窄接口漏项 | 低 | 已实测穷举；以 `go build` + 用例为准；适配器与 `filesStorageManager` 同构 |
| 持久化路径语义被搬迁改变 | 低 | 纯搬迁 + `cloud_newlayout_test.go`/`cloud_owner_test.go` 钉住 |
| TTL 清理 / 恢复扫描 goroutine 生命周期 | 中 | `Close()` 逐字保留；`-race` + e2e |
| 用例名守恒 | 中 | 基座相关用例留装配层、改名迁入（files 先例已证明合规） |
| 一次性大重构 | 中 | 分片 + 每片独立 PR + 四条机械核对 + 自动合并 |

## 7. 不做（记录理由）

- **S4-C 不与 A/B 合并**：它的接缝成本占 90%，收益只剩内聚；先让 A/B 交付可评估的中间态。
- **不动 `pkg/client` 的 cloud 侧**：客户端/服务端已对称，无收益。
- **不动 `pkg/provider` / `pkg/cloudfilename`**：已是顶层包。
- **不建 `pkg/cloud/downloader` 子包**：会成为子包 → R2 反把 cloud 自己限制住；`downloader` 是 P6① 工具集合，取顶层。
- **不提升 `pkg/storage/capacity` 为顶级包**：见 §2 约束 1 的被否决替代方案。
