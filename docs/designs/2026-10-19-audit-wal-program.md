<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 总体方案：统一审计 + 通用等待 + 中断继续（WAL）

> 状态：**设计（待评审后实施）**。本文件是**总纲**；包级细化见
> [`2026-10-19-audit-service.md`](./2026-10-19-audit-service.md)（`pkg/audit`）与
> [`2026-10-19-waitq.md`](./2026-10-19-waitq.md)（`pkg/waitq`）。

---

## 1. 终态目标（用户裁定）

| 目标 | 判定 |
|---|---|
| **审计能力唯一提供方** | 只有 `pkg/audit` 提供；其他包只装配/使用（禁自造） |
| **一切审计皆有作用域** | 无 scope 的审计不可能（编译期/门禁/运行期三重） |
| **活跃记录在内存 + 按需轮转** | 内存超限 / 内容过时 / **读取时** / 显式 / 结束 / 退出 ⇒ 立即轮转 |
| **能力由结构体提供** | 零全局变量 ⇒ 测试隔离、不污染其他功能测试 |
| **等待能力通用可扩展** | `pkg/waitq` 提供抽象；使用方实现 `Resource`（磁盘/内存/CPU/带宽…） |
| **生产可用后补测试与自动恢复** | 各业务**审计场景用例** + **崩溃恢复用例** + 回归锁定 |
| **全系统中断继续（WAL）** | 任务不丢失、崩溃后可续跑（`ListScopes/Resume/MarkDone/Lease`） |
| **包提供全部能力，使用方按场景选择** | 合规形态/span/采样/WAL/归档/加密存储/配额治理均为**可选能力**；使用方声明 `CapabilitySet` |
| **审计落盘纳入配额**（防磁盘打满） | 本地热段**预留额度**（不与用户竞争）+ 卷内归档走**卷配额** |
| **审计可归档到外部存储卷** | 长期留存复用**卷/外部卷/加密卷**；本地只留热段（占用有上界） |
| **保留审计/系统用户管理审计数据** | 保留 owner 复用配额/加密/外部卷/版本/回收站/统一恢复；**5 条安全红线** |

---

## 2. 三个能力包与依赖

```
                    ┌──────────────────────────────────────────────┐
                    │ pkg/audit（唯一审计 + WAL 基座）              │
                    │  Scope · Entry · Journal · Index · Service    │
                    │  Save/Resume/ListScopes/MarkDone · Rotate     │
                    └───────────────┬──────────────────────────────┘
                                    │ 提供「作用域 + 持久化 + 重放」原语
                                    ▼
                    ┌──────────────────────────────────────────────┐
                    │ pkg/recovery（统一恢复协调器，M3）            │
                    │  注册中心 · 优先级/依赖编排 · 块级校验续传     │
                    └──────────────────────────────────────────────┘

                    ┌──────────────────────────────────────────────┐
                    │ pkg/waitq（通用等待/排队，独立）              │
                    │  Queue · Resource · Lease · 优先级 · 背压     │
                    └──────────────────────────────────────────────┘
```

| 包 | 职责 | 依赖 |
|---|---|---|
| `pkg/audit` | 审计条目/作用域/索引/轮转/持久化/重放 | 无（叶子包） |
| `pkg/waitq` | 通用加权等待队列（任意资源） | 无（叶子包） |
| `pkg/recovery` | 统一恢复协调器（收敛 5 处独立恢复） | `pkg/audit`（WAL 原语） |

**关键**：**审计与中断继续共用一条 append-only 流** ⇒ 恢复过程本身也是审计证据（合规闭环），且**不重复造持久化**。

---

## 3. 阶段划分与顺序

| 阶段 | 内容 | 依赖 | 交付判据 |
|---|---|---|---|
| **M1 审计收敛** | `pkg/audit` 增统一能力（Entry/Service/Journal/Index/Notifier/Policy）；`pkg/server.RecordAudit` 薄委托；`AuditRing`/`AuditStore` 下沉；**补 8 类覆盖缺口**；`auditMiddleware` 传输层兜底；结构化字段；丢弃/失败计数 + 可选 fail-closed；**轮转四触发** | — | 旧用例全绿 + 新业务审计用例 + R35 |
| **W1 通用等待** | 新建 `pkg/waitq`；`quota`/`staging`/`cloud 内存` 适配；**配额获取 7 条 → 1 条**；惊群/背压/优先级/指标 | — | 迁移等价用例 + R36 |
| **M2 WAL 化** | `Save` 落盘（`KindState`+`Seq`）；`Resume/ListScopes/MarkDone/Lease`；段布局（合规=全局段 / 任务=按 scope 段）；`Archiver` 接线 | M1 | 崩溃重放/幂等/已完成跳过用例 |
| **M3 统一恢复协调器** | `pkg/recovery`：注册中心（装配层启动编排）；收敛 cloud/sync/chunked 三处 `recover*`；**接线 baidupcs 断点**；**上传/下载块级校验**（`meta.VerifyReadSeeker`）；崩溃恢复 e2e | M2 | 崩溃恢复 e2e + R37 |
| **S3 面与文档** | Web UI（审计新字段/筛选 + 等待可观测）；docs（`api.md`/`config.md`/设计交叉引用）；**真浏览器 e2e** | M1/W1 | R10 + UI e2e |
| **M4 归档到外部/加密卷**（后续阶段） | sealed 段 → 写内核 → 保留用户卷内路径 → 热段回收；`Query` 跨热/冷段；归档失败**不丢热段** | M2 | 归档往返用例（含加密卷/外部卷） |

**顺序理由**：M1 先（统一日志形态是 M2 的载体）；W1 独立可与 M1 并行（同分支顺序做，避免多 PR）；M2 在 M1 后（`Save` 落盘复用统一 Journal）；M3 在 M2 后（协调器消费 `ListScopes/Resume`）；S3 最后（面）；**M4 为后续阶段**（用户裁定「后续规划支持归档到外部存储卷」，依赖 M2 的段与摘要）。

> 原「S1 配额收敛」**并入 W1**（`waitq` + 适配器即收敛），不再单列。

---

## 4. 能力矩阵（总表）

### 4.1 审计

| 能力 | 现状 | 目标 |
|---|---|---|
| 合规事件（谁/什么/结果） | 仅 `pkg/server` | `pkg/audit.Service.Emit`（全包可用） |
| 强制作用域 | 部分 | 全部（无 scope 不可审计） |
| 内存活跃索引 | 有（无 scope） | `Index`（scope/actor/action/时间/结果） |
| 轮转：内存/过时/读取/显式/结束/退出/大小 | 仅「大小」 | 7 触发全 |
| 落盘策略 | 恒 fsync 或尽力 | 按 `Kind` 可配（合规逐行 fsync / 诊断批量） |
| 覆盖完备（变更路由） | 8 类缺口 | `auditMiddleware` 兜底 + 缺口补齐 |
| 结构化字段 | 部分 | volume/request_id/source_ip/size/dur |
| 可观测（丢弃/失败）+ fail-closed | 无 | 计数 + 选项 |
| 归档到卷 | 预留未接线 | 接线 |
| WAL（Save 落盘/Seq/Resume/ListScopes/MarkDone/Lease） | 无 | 全 |
| 无全局变量 / 可注入 Clock | 混用 | 全结构体 |
| **能力选择性启用**（CapabilitySet） | 无 | 使用方按场景声明（合规形态=可选） |
| **落盘策略族**（Always/Batch/None/Memory） | 硬编码 | 包提供，使用方选择（scope > Kind > 全局） |
| **存储配额治理**（本地预留 + 卷配额） | 无 | §5.4（注册为 `waitq.Resource`） |
| **归档到外部/加密卷** | `Archiver` 预留 | §5.5（走写内核 ⇒ 复用配额/加密/版本/多卷） |
| **保留用户（系统用户）** | 无 | §5.6（复用外部卷/加密卷；5 条安全红线） |

### 4.2 等待

| 能力 | 现状（4 套） | 目标（1 套） |
|---|---|---|
| 加权 + 阻塞 + 超时 | ✓ | ✓ |
| 不可能满足 ⇒ 立即失败 | 仅 quota | ✓ 全部 |
| 丢唤醒防护 | 门禁纪律 | **库内不变式** |
| 定向唤醒（无惊群） | ✗ | ✓ |
| 背压（队列上限） | ✗ | ✓ |
| 优先级 + 老化 | ✗ | ✓ |
| 多资源原子 | 特化 | ✓ |
| 可观测（指标/Stats） | 仅测试计数 | ✓ |
| 使用方扩展（内存/CPU/带宽） | ✗ | ✓ `Resource` |
| 获取路径收敛 | 7 条 | **1 条** |

### 4.3 中断继续

| 能力 | 现状 | 目标 |
|---|---|---|
| 统一注册中心 | 无（各扫各目录） | `pkg/recovery` 注册表 |
| 崩溃后可续跑 | 3 处各自实现 + 2 处缺 | 统一（`ListScopes/Resume`） |
| 上传断点（baidupcs） | **未接线** | 接线 + 块级校验续传 |
| 下载续传块级校验 | 仅整文件哈希 | 逐块校验 + 异常块重下 |
| 幂等重放 | 无 | `Seq` 幂等 |
| 多节点单写者 | 无 | `Lease` |
| 已完成跳过 | 部分 | `MarkDone` 终态 |

---

## 5. 边界场景（汇总索引）

| 域 | 场景 | 详见 |
|---|---|---|
| 审计 | E1–E15（无 scope / 轮转并发 / fsync 失败 / 内存逐出 vs 未落盘 / 半行坏行 / 过时实现 / 时钟回拨 / scopeID 安全 / 多节点 / 敏感内容 / 抖动 / 阻塞业务 / 禁用态 / 双格式迁移 / 总容量上限） | audit-service.md §7 |
| 等待 | W1–W16（丢唤醒 / 惊群 / 忙等 / 死锁 / 取消 / 超时 / 队列满 / 部分成功回滚 / 加锁顺序 / 重入 / 饿死 / 容量变更 / 下溢 / 停机 / 指标基数 / 时钟 / 内存有界） | waitq.md §6 |
| 恢复 | 崩溃重放 / 幂等 / 已完成跳过 / 残留 staging+resume / 异常块修正 / 多节点租约 | 本文件 §6 + `docs/archive/architecture-task-recovery.md` |

---

## 6. 测试计划（用户要求⑤：生产可用后必补 + 锁定防回归）

### 6.1 各业务功能「审计场景」用例

| 业务 | 断言 |
|---|---|
| 文件 | upload（**新增**）/ overwrite / rename / copy / move / delete / 软删（含 denied/error） |
| 目录 | mkdir / rmdir（含配额不足 denied） |
| 归档/转存 | archive（含 target volume） |
| 回收站 | restore / empty（单卷 + `all=1`） |
| 卷 | user volume 建/删、move/rebalance/copy/import、quota set |
| 密钥 | secrets 建/删（**断言不含密钥明文**） |
| presign | presign + complete（含被拒） |
| 版本 | restore / delete |
| 云任务 | cancel / delete / archive |
| 同步 | syncexec 拉/推（**新增覆盖**） |
| 传输层 | 任意变更路由 ⇒ ≥1 条事件（含 request_id/source_ip） |
| 等待 | 配额不足 ⇒ 等待并成功（**不再立即 507**）；超时 ⇒ 507；队列满 ⇒ 429 |
| 恢复 | 崩溃 → 重启 → 续跑完成（cloud/sync/chunked/baidupcs/下载） |

### 6.2 锁定与隔离

- 每条用例断言 **action/actor/object/volume/result**（防回归漂移）；
- **隔离性**：门禁扫描 `pkg/audit`/`pkg/waitq` **禁包级可变状态** ⇒ 保证「不影响其他功能测试」；
- **禁新增 `time.Sleep`**（R14 棘轮）⇒ 用注入 Clock / `Stats()` 确定性同步；
- `t.Parallel()` 默认 + serial 预算棘轮。

---

## 7. 门禁（新增，沿用「结构门禁 > 运行时竞态用例」纪律）

| 门禁 | 断言 | 理由 |
|---|---|---|
| **R35-a** | `RecordAudit`/`Emit` 调用点必须在登记表（棘轮只减不增） | 防「又到处实现」 |
| **R35-b** | 所有变更型路由必须经 `auditMiddleware`（或登记豁免+理由） | 覆盖完备性不依赖人 |
| **R35-c** | `pkg/server` 不得定义审计条目类型/落盘实现 | 唯一提供方 |
| **R36-a** | 非测试源码禁直接 `TryReserve`/`WaitReserve`/`semaphore.Weighted`（白名单=waitq+适配器） | 路径收敛 |
| **R36-b** | 非测试源码禁新增 `sync.Cond`（白名单=`pkg/waitq`） | 防第 5 套实现 |
| **R36-c** | `Resource` 适配器必须登记（资源名/label/单位/超时） | 能力矩阵可控 |
| **R37** | 长任务必须 `Open` 作用域（可续跑） | 中断继续前提 |
| R34（保留） | 释放入口在根等待锁内（**冗余保险**） | I1 生效后可退役 |

---

## 8. 配置与运维（新增键）

| 键 | 默认 | 说明 |
|---|---|---|
| `audit.max_memory_bytes` / `audit.max_buffer_entries` | 8 MiB / 2048 | T1 内存超限轮转 |
| `audit.max_age` | `5m` | T2 内容过时轮转 |
| `audit.rotate_on_read` / `audit.min_rotate_interval` | `true` / `30s` | T3 读取触发 + 防抖 |
| `audit.max_total_bytes` | `0`（不限） | E15 防吃满磁盘 |
| `audit.fail_closed` | `false` | E3 合规可选 |
| `audit.sync_mode`（按 Kind 覆盖） | `event=always, span=batch, resource=batch` | §5.2 落盘策略 |
| `waitq.<resource>.timeout` / `.max_waiters` / `.priority` | 30s / 1000 / normal | 按资源登记（沿用 `quota_wait_timeout` 语义） |
| `audit.local_reserve_bytes` | 256 MiB | 本地热段**预留额度**（不与用户数据竞争） |
| `audit.max_total_bytes` | `0`（不限，部署应显式设） | 本地热段硬上限 ⇒ 超限即轮转/归档 |
| `audit.sync.<kind>` / `audit.sync.scope.<id>` | `event/done/state=always`、`span/resource=batch` | 落盘策略族选择（scope > Kind > 全局） |
| `audit.retained_owner` | `__audit__`（待裁定 O6） | 审计数据保留用户（冷段归档归属） |
| `audit.archive_volume` / `audit.archive_path` / `audit.local_keep_segments` | 空 / 空 / 4 | M4：归档目标卷与热段保留份数 |

---

## 9. 不做的事（YAGNI，避免过度设计）

- 不引入外部 WAL 库（如 etcd/raft）：单机 append-only + fsync 已满足「任务不丢失」；
- 不做审计的**分布式共识**（多节点各自本地段 + `NodeID` 标注 + `Lease` 足够）；
- 不做审计内容加密（不含密钥，明文 JSON 便于合规检索；目录权限 0700 保证本地安全）；
- 不为 `resource` 采样行开逐行 fsync（`store.go` 已论证 I/O 放大）；
- 不重写 `pkg/files` 的锁体系（本次只统一「审计 + 等待 + 恢复」三块能力）。

---

## 10. 开放问题（需你裁定）

| # | 项 | 建议 |
|---|---|---|
| ~~Q1~~ | ~~`audit.fail_closed` 默认~~ | **已裁定**：默认 best-effort（零回归），合规部署显式开启 |
| ~~Q2~~ | ~~段布局~~ | **已裁定**：**两者并存**（合规=全局段、任务/WAL=按 scope 段） |
| Q3 | 是否保留 R34 | 建议保留为冗余保险，稳定一版后退役 |
| Q4 | `ratelimit` 并发槽是否迁入 `waitq` | 建议先不迁（语义非阻塞），仅统一指标 |
| ~~Q5~~ | ~~阶段顺序~~ | **已裁定**：单分支单 PR，M1 → W1 → M2 → M3 → S3 |
| Q6 | 优先级是否配置化 | 建议先内部常量，需要再配置化 |
| Q7 | 保留 owner 命名（`__audit__` vs `_system`） | 见 audit-service.md O6 |
| Q8 | 本地热段预留额度 / 归档阈值默认值 | 见 audit-service.md O7/O8（256 MiB / 70% 或 N 段） |
| Q9 | 本地热段是否也加密（现为明文 + 0700） | 建议先明文（与现状一致）；若要求则归档后统一加密卷（M4） |

---

## 11. 实施清单（评审通过后按序执行）

1. **M1**：`pkg/audit` 统一能力（Entry/Service/Journal/Index/Notifier/Policy）+ `pkg/server` 薄委托 + 下沉 ring/store + 覆盖缺口 + `auditMiddleware` + 轮转四触发 + 指标 + 门禁 R35；
2. **W1**：`pkg/waitq` + `quota`/`staging`/`cloud 内存` 适配 + 配额路径收敛 + 门禁 R36；
3. **M2**：`Save` 落盘 / `Seq` / `Resume` / `ListScopes` / `MarkDone` / `Lease` / `Archiver` + 恢复用例；
4. **M3**：`pkg/recovery` 协调器 + 收敛 5 处 + baidupcs 断点接线 + 块级校验 + 崩溃 e2e + 门禁 R37；
5. **S3**：Web UI（审计字段/筛选 + 等待可观测）+ docs + 真浏览器 e2e；
6. **M4（后续阶段）**：归档到外部/加密卷 + `Query` 跨热/冷段 + 保留用户路径；
7. 全量验证：`go test ./...`（含 serial/sleep 棘轮）+ `make lint` + `make check-ci` + e2e + 零丢失验证。
