# 配额模型 v3（物理占用 · 每卷独立 · 目录级配额）——设计与实施规格

> 状态：**规格定稿，实施中**（S0 已落地）。用户裁定日期 2026-10-14。
> 本文是**唯一权威依据**：实施中出现分歧，以本文为准；修改本文需同步用户裁定。

## 1. 目标与硬约束

| # | 约束 | 说明 |
|---|---|---|
| G1 | **绝对禁止磁盘打满 / 绝对不超真实配额** | 由「严格上界预占 + 运维余量」保证，而非事后对账 |
| G2 | **只按物理占用记账** | 逻辑大小无人关心；账本 = 卷内实际落盘字节 |
| G3 | **每卷独立配额；用户在每个卷的配额独立** | 键 = (volume, owner) |
| G4 | **目录级最大配额**（最有价值能力） | 任意深度子树上限 |
| G5 | **一切按用户独立目录组织** | 各用户独立 trash/secrets/staging/cloud/backup/audit ⇒ 天然隔离，**无「系统数据」概念** |
| G6 | **不跨 owner 共享 blob** | 归属完备（每个 blob 恰属一个 owner） |
| G7 | **直接切换、无需兼容** | 旧 `owner_quotas`（全局/逻辑）废弃 |
| G8 | **严格上界预占 + 不足排队重试** | 真正放得下的文件最终一定成功 |
| G9 | **启动异步回填 + 就绪前拒绝写** | 回填完成前写返回 503（读/删放行） |
| G10 | **对账支持只对账某个用户** | 亦支持单卷 / 单目录 |

## 2. 结构与不变式

配额树（复用现有 `quota.Pool`/`Scope`，每卷一个 Pool）：

```
卷 Pool(maxBytes = 卷容量, reserve = 运维余量)
└── owner Scope(max = 该用户在本卷的配额)
    └── 目录 Scope(max = 目录级配额)   ← 任意深度
```

| # | 不变式 | 强制点 |
|---|---|---|
| I1 | `Σ 子节点上限 ≤ 父节点可用上限` | 配置路径 `EnsureChild`（超额**原子失败**） |
| I2 | `节点 committed + reserved ≤ 节点可用上限` | `reserveUp` **沿父链**校验（一次调用同时校验 目录→用户→卷） |
| I3 | `可用上限 = maxBytes − 运维余量` | `usableLocked()`；运维余量仅卷根设置 |
| I4 | **`预占 ≥ 实际物理占用`** | 卷策略 `ChargeForCreate` 返回严格上界；e2e 硬断言 |
| I5 | `卷物理总占用 ≤ 卷容量` | 配额字节 ≤ 可用上限；非配额字节（staging/孤儿/索引）由**运维余量**兜住 |
| I6 | 归属完备（不跨 owner 共享） | dedup 台账 per-tenant；跨 owner 共享禁止 |

## 3. 记账口径与卷策略接口（三者**必须同源**）

```go
// VolumeFootprint 由卷策略实现（明文/加密本地/可信/secretdata/外部/封装）。
type VolumeFootprint interface {
    ChargeForCreate(kind Kind, logical int64) int64   // 严格上界（预检/预占）
    FootprintOf(kind Kind, rel string) int64          // 实测（结算/释放）
    TrueUsage() (int64, bool)                         // 真值报告（对账）
}
```

**同源规则**：三者由同一 `physBytesOf(...)` 派生；违反 ⇒ 对账会把正确账本改错。

| 卷策略 | 内容（变换后） | 辅助对象 | 上界公式 |
|---|---|---|---|
| 明文本地 | `n` | `meta/<rel>.meta`（可选） | `n + meta上界` |
| 加密本地（at-rest） | `EncryptedPhysicalSize(n)`（**精确**） | meta（亦密文） | 上表 + meta |
| 可信卷（trusted.Wrap） | 底层内容 | meta sidecar | 内容 + meta 上界 |
| secretdata | Σ 计划分块（密文） | meta blob（含 pad）+ XOR parity | **计划前移后精确** |
| secretdata（多副本 `targets`） | 上值 | — | **× (1+副本数)** |
| 外部卷（s3/baidupcs/pikpak/cluster） | 对象字节 | — | 对象字节（+本地 staging 另计） |
| 封装卷（enclosure） | 源卷足迹 | 封装自身 meta | 源足迹 + meta |

## 4. 预占用机制（关键：让上界**可靠**）

### 4.1 已知逻辑大小（绝大多数）
- **能精确算 ⇒ 用精确值，不用系数**：加密本地（已验证公式）；secretdata（**计划前移**后精确）；
  可信/明文（内容 + meta 上界）；多副本（×N）。
- **分块计划前移**（secretdata 专属）：`PlanChunks(size, seed) []int64` 在**写前**确定块边界；
  `seed` 随机生成并存入**加密 meta**（观察者不可预测边界 ⇒ **安全属性不退化**）；
  由此 `Σ planned(plaintext + 每块头/nonce/tag) + meta_blob(pad 上界) + parity(上界)` **精确可算**。
  附带收益：重试/续传**复用同一计划**（已写分块不重复计费）、分块可并行各自预占。
- **系数只作兜底**（未来/第三方卷型）：必须 ≥ 实际（I4 由矩阵硬断言）。

### 4.2 未知大小（少数）
- 云下载 `TotalSize == -1`（`pkg/cloud/manager.go` 已有该语义）、流式归档导出：
  复用 `reservingReader`（`pkg/sync/staging_gate.go`）**增量预留 + Read 背压**；完成后结算 + 记录。
- 远端给了长度 ⇒ 走 4.1 预占。

## 5. 排队重试（不足时等待，而非立即拒绝）

- `Scope.WaitReserve(ctx, n, timeout)`：不足 ⇒ 排队（**释放广播**唤醒 / ctx 取消 / 超时）；
  `timeout<=0` ⇒ `DefaultWaitTimeout = 30s`（用户裁定）；超时返回**包装 `ErrStorageFull`** 的错误。
- **丢唤醒防护**：释放侧**持锁广播到根节点队列**；等待侧**持锁 double-check**（`waitRound`）。
- **区分两类不足**（文案与等待策略不同）：
  - **卷物理上限不足**：等他人释放可能可行 ⇒ 排队有意义；
  - **用户配额不足**：等自己释放 / 管理员提额才可能 ⇒ 排队 + 超时后 507「请释放空间或申请提额」。
- 形态与既有 `staging_tracker.go` 一致（`sync.Cond` + double-check + ctx/超时打断；打断用
  `time.AfterFunc` + `context.AfterFunc`，无常驻 goroutine）。

## 6. 记录化释放（消灭单位歧义与测量竞态）

- 对象上存 `charged_bytes`：`FileMeta.Extra`（现成字段，旧读者忽略 ✓）；外部卷无 sidecar ⇒ **独立 ledger 记录**。
- 释放 / 覆盖 / 改名：**读记录**（不做删除前测量）；覆盖写按 `prev` 差分。
- **预留泄漏回收**：预留记录 + 超时回收（崩溃/放弃留下的预占不得永久占位；与 staging 的 5s 超时同型）。

## 7. 迁移与运维

- **启动异步回填**：按**用户目录**遍历，`FootprintOf` 求和写入配额树；**就绪前拒绝写**（读/删放行，
  503 + 明确文案）；进度入 `/metrics` + 日志；支持**单卷/单用户/单目录**回填（同一代码路径）。
- **人工对账**：API + CLI（`--volume` / `--owner` / `--dir`）；对账公式与写入口径**同源**。
- **运维余量**：默认 **卷容量 1%**（可配），仅卷根；用于 staging/孤儿 blob/索引等**非配额物理字节**。

## 8. 可见性（三级）

- 卷：总容量 / 运维余量 / 可用上限 / 已分配 / 未分配 / 已用（真值）
- 用户 × 卷：配额 / 已用 / 剩余
- 目录：配额 / 已用 / 剩余
- Web UI 展示（**改动 UI 必须配 Playwright 真浏览器 e2e**，项目硬规则）+ `docs/config.md`/`docs/api.md`
  新语义（旧 `owner_quotas` 标注废弃）；「上界预占期间已用短暂偏高」在 UI 标注为「预留中」。

## 9. 门禁

| # | 规则 |
|---|---|
| R28 | 非测试源码禁止裸调账本原语（`ReleaseUsage`/`TryReserve`/`ReleaseCommitted`/`Adjust`/`Commit`），必须经 ledger helper；覆盖 `pkg/files`、`pkg/server`、`pkg/quota`、`pkg/volume/capacity`；存量白名单 TSV + **只降不升** |
| R29 | 禁止逻辑尺寸入账（`Charge` 值类型 / helper 唯一入口） |
| R30 | 新增卷策略必须登记进对账矩阵（否则 CI 红） |

## 10. 分期与验收

| 期 | 内容 | 验收 |
|---|---|---|
| **S0** ✅ | 配额树三层 + **分配账本**（`TryAllocate`/`Unallocated`/`EnsureChild`）+ 探针 | 探针全绿（`-race`） |
| **S1** | 记录化释放（`charged_bytes` + 外部 ledger + 预留回收） | 记录读写测试 + 变异 |
| **S2** | 卷策略足迹（6 类）+ **计划前移**（secretdata）+ 加密精确公式 | **对账矩阵**（6 卷型 × 操作序列）全绿 |
| **S3** | 写路径简化（去每写卷池双账本，改查配额树）+ 对账同源 | 矩阵 + 对账幂等（两次对账结果一致） |
| **S4** | 回填 + 就绪门 + 对账 API/CLI + 运维余量接线 | 回填中断/重启可续；就绪前写 503；对账单用户 |
| **S5** | 三级可见性 + Web UI + docs | Playwright e2e + 文档审校 |
| **S6** | R28/R29/R30 门禁 | 门禁红/绿双向验证 |

**对账矩阵（S2 起，先红）**：6 卷型 ×
`write → overwrite → copy → rename → soft-delete → restore → empty-trash → version save/restore/delete →
mkdir → mkdirAll 多层 → 幂等 mkdir → 目录改名 → rmdir → 非空 rmdir → hard delete`，
**每步**断言：I1–I6（含 `实际物理 ≤ 预占`、三层配额不超、卷容量不超）。

## 11. 明确不做（解耦，独立排期）

per-user 的 `secrets`/`audit` **目录化**（当前 audit 为服务端全局环形/存储、secrets 为卷）。

## 12. 已知风险与对策

| 风险 | 对策 |
|---|---|
| 口径一次性切换（无兼容）⇒ 历史用量跳变 | 回填 + 就绪门 + 启动告警；不做双模型 |
| 大卷回填耗时（O(对象数)） | 异步 + 进度 + 就绪门；支持单用户/单目录回填 |
| 上界预占期间「已用」短暂偏高 | UI 标注「预留中」；写后立即结算回落 |
| 计划前移削弱分块随机性（安全） | `seed` 存**加密 meta**；测试断言 seed 不出现在明文侧 |
| 非配额物理字节（staging/孤儿/索引）打满磁盘 | **运维余量**（默认 1%）+ staging 独立 tracker + 孤儿 GC |
| 预占泄漏（崩溃/放弃） | 预留记录 + 超时回收；启动回填一并清理 |
