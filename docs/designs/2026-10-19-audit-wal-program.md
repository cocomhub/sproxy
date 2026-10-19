<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 总体方案：统一审计 + 通用等待 + 中断继续（WAL）

> 状态：**设计 v2（经 3 路独立 fresh-context 对抗审核并逐条证伪后修订；待你确认后实施）**。
> 本文件是**总纲**；包级细化见 [`2026-10-19-audit-service.md`](./2026-10-19-audit-service.md)（`pkg/audit`，E1–E26）
> 与 [`2026-10-19-waitq.md`](./2026-10-19-waitq.md)（`pkg/waitq`，W1–W18）。

---

## 1. 终态目标（用户裁定）

| 目标 | 判定 |
|---|---|
| **审计能力唯一提供方** | 只有 `pkg/audit` 提供；其他包只装配/使用（禁自造）；**包内亦只有一套实现**（旧 API 降级为 facade） |
| **一切审计皆有作用域** | `Emit` 定义在 `*Scope` 上 ⇒ **编译期强制**；后台/周期作业**必须自开合成 scope** |
| **活跃记录在内存 + 按需轮转** | 内存超限 / 内容过时 / **读取时** / 显式 / 结束 / 退出 / 段大小 / 达硬上限（T1–T8） |
| **能力由结构体提供** | 零全局可变状态（门禁 + 白名单）⇒ 测试隔离 |
| **等待能力通用可扩展** | `pkg/waitq` 提供抽象；使用方实现 `Resource`（磁盘/内存/CPU/带宽/连接数） |
| **包提供全部能力，使用方按场景选择** | `CapabilitySet` + 落盘策略族（`SyncAlways/SyncBatch/SyncNone/Memory`） |
| **审计落盘纳入配额（防磁盘打满）** | 热段**有界保留**（`local_reserve_bytes` 自身上界 + `max_total_bytes` **非零安全默认**）；冷段走卷配额 |
| **审计可归档到外部存储卷** | 长期留存复用**卷/外部卷/加密卷**；本地只留热段（**M4**） |
| **保留审计/系统 owner 管理审计数据** | **保留系统 owner `__audit__`**（**非**管理员 owner、**非**审计账号；前提：**注册入口保留该名**）+ **11 条安全红线**（含读面两维权限、TTL 隔离、权限收紧、审计读也要审计、归档卷不绑管理员）；多管理员靠**角色/能力位**扩展（§5.8） |
| **（用户追问）tunnel 鉴权模型** | **tunnel 只是传输层**：内层**强制鉴权**（与主 mux 同一 middleware）；三类显式路由（public / node / user），**取消「裸注册」类别**；tunnel key = 节点身份（`Role=node`）而非用户身份 ⇒ **所有操作都是用户行为、可被管控**（§2.5） |
| **（用户追问）审计空间 = 新用户类型** | `accesskey.RoleSystem="system"` 的**真实租户**（配额/卷/加密/回收站/版本/镜像/巡检**全部复用**）+ **类型级禁止登录**（一处生效，非逐面豁免）+ **启动期保证存在（fail-fast）** |
| **生产可用后补测试与自动恢复** | 各业务审计场景用例 + 崩溃恢复用例（真实 kill）+ 回归锁定 |
| **全系统中断继续（WAL）** | 任务不丢失、崩溃后可续跑；**幂等是使用方契约**（`ReplayApplier`）+ `MarkDone` = 提交记录 |
| **审计读面权限（已裁定）** | **两维模型**：用户维度（owner 读自己 + **脱敏**）/ 系统维度（**仅管理员**）/ 跨维条目（owner 脱敏可见）；**服务端强制过滤**（客户端不可放宽）；`export` 与 `list` 同一门 |
| **单机模型（已裁定）** | 段在本地 ⇒ **跨节点恢复不在本期**（不做无 fencing 的 `Lease`） |

---

## 2. 三个能力包与依赖（**审核修订：分层**）

```
                    ┌──────────────────────────────────────────────┐
                    │ pkg/audit（**叶子核心**：审计 + WAL 基座）     │
                    │  Scope · Entry · Journal · Index · Notifier    │
                    │  Service · Policy · CapabilitySet             │
                    │  （不 import 装配层、不 import waitq）          │
                    └───────────────┬──────────────────────────────┘
                                    │ 提供「作用域 + 持久化 + 重放」原语
                                    ▼
                    ┌──────────────────────────────────────────────┐
                    │ pkg/recovery（统一恢复协调器，M3）             │
                    │  注册中心 · 优先级/依赖编排 · 块级校验续传      │
                    └──────────────────────────────────────────────┘

                    ┌──────────────────────────────────────────────┐
                    │ pkg/waitq（通用等待/排队，独立叶子包）          │
                    └──────────────────────────────────────────────┘

        外置适配器（不在叶子包内 ⇒ 不违反 R4 分层门禁）：
        ├─ 配额适配器：热段上界 / 冷段额度（注册 waitq.Resource）
        ├─ 归档适配器：sealed 段 → 写内核 → 保留用户卷内路径（M4）
        └─ 读面 ACL：/api/audit(+export) 必须管理员角色
```

| 包 | 职责 | 依赖 |
|---|---|---|
| `pkg/audit` | 审计条目/作用域/索引/轮转/持久化/重放 | 无（**叶子**） |
| `pkg/waitq` | 通用加权等待队列（任意资源） | 无（**叶子**） |
| `pkg/recovery` | 统一恢复协调器（收敛 5 处独立恢复） | `pkg/audit` |
| 适配器（装配层/`pkg/audit/adapters/*`） | 配额 / 归档 / ACL / 通知渠道 | `pkg/audit` + `pkg/waitq` + 写内核 |

**关键**：**审计与中断继续共用一条 append-only 流** ⇒ 恢复过程本身也是审计证据；**但 TTL/清理只作用于合规全局段，绝不触碰 WAL 段**（E25）。

### 2.5 tunnel 鉴权模型（架构级决策，用户 2026-10-19 追问）

**问题**：tunnel 场景下应（A）用**专用 middleware 直接把内层请求当作「该 tunnel 用户」鉴权通过**，还是（B）tunnel 下**可能不同用户 ⇒ 必须强制鉴权**？

**现状取证**：`localMux`（隧道内层）共 **105 条裸注册** ✗，注释自陈「**隧道加密即认证**」「与…同模式」（`routes.go`）；只有主 mux 有 `authMiddleware`；`Principal` 已有 4 类型 `user/reader/node/admin`（`pkg/accesskey`）；隧道客户端**透传 ctx**（`tunnel/handler_client.go:141`）。

| 方案 | 优点 | 缺点（本质） |
|---|---|---|
| **A 专用 middleware 视为「该 tunnel 用户」通过** | 改动小（105 处不动） | ① 隧道流量全部归属**单一伪用户** ⇒ 配额/审计/ACL **无法按用户管控** ✗；② 制造**特权旁路**（tunnel key 泄露 = 该伪用户全部权限）✗；③ 与「所有操作都是用户行为」理念冲突 ✗；④ **豁免面 = 105 处 + 每个新路由** ⇒ 正是用户点名的「**各种豁免容易疏漏**」✗ |
| **B tunnel 只是传输层，内层强制鉴权（推荐 ✓）** | ① **一套鉴权**（与主 mux 同一 middleware ⇒ 收敛 ✓）；② 每请求归属**具体用户** ⇒ 配额/审计/ACL 全正确 ✓；③ **fail-closed**：新增路由默认受保护 ⇒ **零豁免面** ✓；④ tunnel key = **传输/节点级**身份（`Role=node` ✓ **已是既有用户类型**）而非用户身份 ⇒ 职责清晰 ✓ | ① 内层请求必须携带用户凭据（CLI `--access-key`/SproxySig 路径**已存在** ✓）；② 需把 105 处裸注册迁到统一 middleware（工作量，但**正是收敛目标** ✓） |

**推荐：B（强制鉴权）** —— 与用户理念「**所有操作都是用户行为、可以被管控**」一致 ✓；且与 §5.6 的「审计租户 = 用户类型 `system`」同一思想（**用类型统一判定，不用豁免**）✓。

**落地形态（三类显式路由，**取消「裸注册」类别**）**：
1. **public**（显式白名单）：`register`/`login`/`nonce`/健康检查 —— 仅此一类免鉴权 ✓；
2. **node/mesh**：`requireRole(node)`（`Role=node` 已有 ✓）；
3. **user**：`requireRole(user|reader|admin)` ✓；
⇒ 主 mux 与 localMux **共用同一套中间件与同一分类**（差异仅传输层：直连 vs tunnel）✓。

**门禁 R38**：非测试源码中 `localMux.Handle*` 必须经统一中间件（或登记为 public + 理由）⇒ **裸注册只减不增**（基线 105，收敛到 0）✓。

**对 M1-11 的影响**：审计读面在隧道下同样按 **ctx 身份**做 owner 过滤 ⇒ 消除 revB 指出的「直调用例无 Principal」矛盾 ✓（隧道不再是「无身份的旁路」✓）。

---

## 3. 阶段划分与顺序（**审核修订：依赖显式化 + 阶段改名**）

| 阶段 | 内容 | 依赖 | 交付判据 |
|---|---|---|---|
| **M1 审计收敛** | `pkg/audit` 统一能力（Entry/Scope/Service/Journal/Index/Notifier/Clock/Policy/CapabilitySet + null-object）；`RecordAudit` 薄委托；**`AuditStore/AuditRing` 迁移（已裁定：②彻底 — 类型迁入 `pkg/audit`）**；**补覆盖缺口全表**；`auditMiddleware`；结构化字段 + 计数 + fail-closed；**轮转 T1–T8**；**M1-9 保留 owner 前提 + 拒绝面**；**M1-11 读面权限（两维模型）**；**M1-12 权限收紧**；**M1-13 TTL 与 WAL 隔离**；**M1-14 审计空间 = `Role=system` 租户 + 启动期预建（fail-fast）**；**M1-15 tunnel 内层强制鉴权（105 裸注册收敛，R38）** | — | 旧语义逐条保留用例通过（倒序/limit/export 升序/轮转文件名/信封）+ 新业务审计用例 + R35 |
| **W1 通用等待** | 新建 `pkg/waitq`；`quota`（**等待侧 + 释放侧同提交**）/`staging`/`cloud 内存` 适配；**获取路径收敛（两层 ~40 处）**；惊群/背压/优先级/指标 | — | 迁移等价用例 + W17 回归 + R36 |
| **M1-8 审计配额治理**（依赖 W1） | 热段**有界保留**（自身上界）+ `max_total_bytes` 非零默认 + 达上限轮转/归档/有界丢弃 | W1 | 配额/有界保留用例 |
| **M2 WAL 化** | `Save` 落盘（`KindState`+`Seq`）；`Resume(ReplayApplier)`/`ListScopes`/`MarkDone`（**提交记录语义**）；两级序；段布局（合规=全局段 / 任务=按 scope 段）；`Journal.Rewrite` 接口化 | M1 | 崩溃重放/幂等契约/提交语义用例 |
| **M3 统一恢复协调器** | `pkg/recovery`（**考虑降为 `pkg/audit/recovery` 子包**，避免为「注册中心+编排」新增顶层包）：注册中心；收敛 **3 处既有 `recover*`**（cloud/sync/chunked）+ **2 处新增**（baidupcs 断点接线、下载块级校验）；**首版保留 legacy 兜底** | M2 | 崩溃恢复 e2e（真实 kill）+ R37 |
| **S4 面与文档**（原名 S3，**改名避免与 S3 网关混淆**） | Web UI（审计新字段/筛选 + 等待可观测）；docs（`api.md`/`config.md`/`gates.md`）；**真浏览器 e2e** | M1/W1 | R10 + UI e2e |
| **M4 归档到外部/加密卷**（后续） | sealed 段 → 外置归档适配器 → 写内核 → 保留用户路径 → 热段回收；跨热/冷段 `Query`；归档↔轮转护栏 | M2 | 归档往返（含加密/外部卷）+ 失败不丢热段 |

**顺序理由**：M1 先（统一日志形态是 M2 的载体）；W1 独立；**M1-8 必须在 W1 之后**（依赖 `waitq`，原 v1 顺序不自洽 ✗）；M2 复用统一 Journal；M3 消费 `ListScopes/Resume`；S4 最后；M4 后续。

---

## 4. 能力矩阵（总表）

### 4.1 审计

| 能力 | 现状 | 目标 |
|---|---|---|
| 合规事件（谁/什么/结果） | 仅 `pkg/server` | `(*Scope).Emit`（全包可用） |
| 强制作用域 | 部分 | **编译期**（`Emit` 在 `*Scope` 上） |
| 内存活跃索引 | 有（无 scope） | `Index`（倒序 + limit + 过滤语义锁定） |
| 轮转 T1–T8 | 仅「大小」 | 全 8 触发 |
| 落盘策略族 | 硬编码/尽力 | 4 策略，**默认 `SyncBatch`**（避免 fsync 回归） |
| 覆盖完备（变更路由） | 缺口含 share/sync/DAV/notify 等 | `auditMiddleware` 兜底（**不计达标**）+ 缺口补齐 |
| 结构化字段 | 部分 | volume/request_id/source_ip/size/dur |
| 有界保留 + 丢弃/失败/panic 计数 | 无 | §5.4.1（审计）+ `Metrics` |
| **读面权限** | **无（P0）** | **两维模型**（§5.7）：用户维度（脱敏）/ 系统维度（仅管理员）/ 服务端强制过滤 |
| **保留 owner 名保护** | **可被占用** | 注册入口保留 `__` 前缀 |
| **权限** | 0755/0644 | 0700/0600 |
| 归档到卷 | `Archiver` 预留 | M4 外置适配器 |
| WAL（Save/Seq/Resume/ListScopes/MarkDone） | 无 | 幂等契约 + 提交语义 |
| 无全局可变状态 / 可注入 Clock | 混用 | 全结构体 + 门禁白名单 |

### 4.2 等待

| 能力 | 现状（4 套） | 目标（1 套） |
|---|---|---|
| 加权 + 阻塞 + 超时 | ✓ | ✓ |
| 不可能满足 ⇒ 立即失败 | 仅 quota | ✓ |
| 丢唤醒防护 | 门禁纪律（R34 登记式） | **库 + 释放侧 `Notify` + R34 升级断言** |
| 定向唤醒（无惊群） | ✗ | ✓ |
| 背压（队列上限） | ✗ | ✓ |
| 优先级 + 老化 | ✗ | ✓ |
| 重入检测 | ✗ | ✓ `Request.Holder` |
| 多资源原子（**两层预留**） | 特化 | ✓ `Group` |
| 可观测（指标/Stats） | 仅测试计数 | ✓ |
| 使用方扩展（内存/CPU/带宽） | ✗ | ✓ `Resource` |
| 获取路径收敛 | **两层 ~40 处** | 经唯一 `Acquire`（**已裁定：两层一并收敛**；探测式登记豁免） |

### 4.3 中断继续

| 能力 | 现状 | 目标 |
|---|---|---|
| 统一注册中心 | 无（各扫各目录） | `pkg/recovery` 注册表 |
| 崩溃后可续跑 | 3 处各自实现 + 2 处缺 | 统一（`ListScopes/Resume` + legacy 兜底） |
| 幂等重放 | 无 | **`ReplayApplier` 契约**（红灯用例锁定） |
| 提交语义 | 无 | `MarkDone` = 副作用后的提交记录 |
| 上传断点（baidupcs） | 未接线 | 接线 + 块级校验续传 |
| 下载续传块级校验 | 仅整文件哈希 | 逐块校验 + 异常块重下 |
| 多节点 | 无 | **本期单机**（跨节点另立专题） |

---

## 5. 边界场景（汇总索引）

| 域 | 编号 | 详见 |
|---|---|---|
| 审计 | **E1–E26**（含 E21 panic 隔离 · E22 轮转递归 · E23 归档↔轮转 · E24 seal→open 耐久 · E25 TTL vs WAL · E26 读面无 ACL） | audit-service.md §7 |
| 等待 | **W1–W18**（含 W17 迁移期双队列 · W18 两层预留） | waitq.md §6 |
| 恢复 | 崩溃重放 / 幂等 / 提交语义 / 残留 staging+resume / 异常块修正 | 本文件 §6 + `docs/archive/architecture-task-recovery.md` |

---

## 6. 测试计划（用户要求⑤）

### 6.1 各业务功能「审计场景」用例

文件（upload **新增**/overwrite/rename/copy/move/delete/软删，含 denied/error）· 目录（mkdir/rmdir）· 归档 · 回收站（单卷 + `all=1`）· 卷（user volume 建删、move/rebalance/copy/import、quota set）· 密钥（**不含明文**）· **presign(+complete)** · **分享创建/撤销** · **sync HTTP（任务/冲突）** · **DAV（PUT/DELETE/MOVE/COPY/MKCOL/PROPPATCH/LOCK）** · 版本 · 云任务（含 cloud groups）· **syncexec 拉/推** · 传输层（≥1 事件含 request_id/source_ip）· **保留 owner 越权矩阵（403）** · **读面 ACL（reader ⇒ 403）**。

### 6.2 可靠性/恢复用例（审核补充）

真实 kill 崩溃重放（`test/e2e` + 撕裂行注入）· 幂等契约红灯（非幂等 apply ⇒ 失败）· 提交语义（`MarkDone` 前后 kill）· 轮转不丢（并发 + T1/T3）· 内存有界 + 丢弃计数 == 超出数 · **不阻断业务的量化判据**（500ms 慢 Journal ⇒ 业务 P99 不劣化）· panic 隔离 · 禁用态隔离 · 测试隔离（无全局可变状态 + 门禁白名单）· **等待：配额不足 ⇒ 等待并成功；超时 ⇒ 507；队列满 ⇒ 429；W17 不丢唤醒回归**。

### 6.3 纪律

`t.Parallel()` 默认 · **禁新增 `time.Sleep`**（R14）· serial 预算棘轮 · 每包必须有测试 · **压测判据**（fsync 策略：HDD/SSD 吞吐与尾延迟）。

---

## 7. 门禁

| 门禁 | 断言 |
|---|---|
| R35-a | `Emit`/`RecordAudit` 调用点登记（基线 **115 处/20 文件** + `recordFileAudit` 29 处/3 文件），棘轮只减不增 |
| R35-b | 变更型路由必须经 `auditMiddleware`（豁免登记）；**transport 兜底不计达标** |
| R35-c | `pkg/server` 不得定义审计条目类型/落盘实现 |
| **R35-d** | `pkg/audit` 不得同时导出两套 span/kv API（除 facade） |
| **R35-e** | `/api/audit` `/api/audit/export` 必须：① 管理员门；② **服务端强制 owner 过滤**（非管理员不得放宽）；③ `export` 与 `list` 同一门与同一过滤（豁免登记） |
| R36-a | 禁直接 `TryReserve`/`WaitReserve`/`TryReserveChunked`/`semaphore.Weighted`（白名单 + **基线全量登记**，§8.2 of waitq.md） |
| R36-b | 禁新增 `sync.Cond`（基线：`quota.go:46`、`staging_tracker.go:35`；**`kad.go:246` 显式排除**） |
| R36-c | `Resource` 适配器登记（名/label/单位/超时/MaxWaiters/优先级） |
| R37 | 长任务必须 `Open` 作用域 |
| **R38** | **非测试源码中 `localMux.Handle*` 必须经统一鉴权中间件**（或登记 public + 理由）⇒ 裸注册**只减不增**（基线 105 ⇒ 目标 0） |
| **R34（升级）** | 释放入口须在 `withWaitLock` 内**且**调 `Notify`/`Adjust` |
| **门禁登记表** | 新增 `docs/gates.md`（R 号 / 文件 / 断言 / 基线）+ 测试断言「R 号双向出现」 |

---

## 8. 配置与运维（**审核修订：去重 + 安全默认**）

| 键 | 默认 | 说明 |
|---|---|---|
| `audit.max_memory_bytes` / `audit.max_buffer_entries` | 8 MiB / 2048 | T1 内存超限轮转（`max_buffer_entries` 与既有 `audit.buffer_size` 的关系：**`buffer_size` 为兼容别名**，一个版本后废弃） |
| `audit.max_age` | `5m` | T2 内容过时 |
| `audit.no_rotate_on_read`（**bool 默认 false = 默认轮转**） | `false` | T3 读取触发；`audit.min_rotate_interval` = `30s` 防抖 |
| `audit.max_total_bytes` | **`local_reserve_bytes × 4`（非零安全默认）** | T8 热段硬上限（**单一键，单行**） |
| `audit.local_reserve_bytes` | 256 MiB | **自身上界语义**（非物理预留，§5.4.2） |
| `audit.fail_closed` | `false` | 合规可选 |
| `audit.sync.<kind>` / `audit.sync.scope.<id>`（**单一形态**，删 `sync_mode`） | `event=batch`、`span=batch`、`state/done=always`、`resource=batch` | 落盘策略（scope > Kind > 全局） |
| `audit.batch_max_lines` / `audit.batch_max_interval` | `256` / `200ms` | SyncBatch 的 group commit 阈值（**审核：必须入配置表**，否则性能判据不可执行） |
| `audit.owner` | `audit`（**租户名**，`Role=system`） | 审计空间租户；**不允许**改为普通用户 owner（红线 11） |
| `audit.archive_volume` / `archive_path` / `local_keep_segments` | 空 / 空 / 4 | M4 |
| `waitq.<resource>.{timeout,max_waiters,priority}` | 30s / 1000 / normal | 按资源登记 |

---

## 9. 不做的事（YAGNI）

- 不引入外部 WAL 库（单机 append-only + 策略化 fsync 已满足）；
- **不做分布式共识 / 不做无 fencing 的 `Lease`**（段在本地 ⇒ 跨节点恢复另立专题）；
- 不做审计内容加密（明文 + 0700/0600；归档走加密卷）；
- 不为采样行开逐行 fsync；
- **不把「从不排队」的限额（审计热段）建模为 `waitq.Resource`**（语义错配）；
- 不重写 `pkg/files` 锁体系；
- **不新建第 4 套 append-only 实现**：`pkg/state.LocalAppendStore`（`pkg/state/append_local.go:17`，注释即写「审计/事件」）**当前零生产调用** ⇒ 需在 M1 写明取舍：**采用 `pkg/audit.Journal` 并废弃/改造 `LocalAppendStore`**（或反之），不得两套共存（否则与「唯一提供方/唯一实现」矛盾）。

---

## 10. 开放问题

| # | 项 | 建议 |
|---|---|---|
| Q1/Q2/Q5 | ~~fail_closed / 段布局 / 阶段顺序~~ | **已裁定**（v1 问答） |
| Q6 | 优先级配置化 | 先内部常量 |
| Q7 | 保留 owner 命名 | `__audit__`（前提：注册入口保留 `__`） |
| Q8 | `local_reserve_bytes` / 归档阈值 | 256 MiB / 70% 或 N 段 |
| Q9 | 热段是否加密 | 先明文 + 0700/0600 |
| **Q10** | **`max_total_bytes` 默认值** | 建议 `local_reserve_bytes × 4`（非零安全默认） |
| ~~Q11~~ | ~~单机模型~~ | **已裁定：本期单机**（跨节点恢复另立专题） |
| ~~Q12~~ | ~~读面角色~~ | **已裁定：两维权限模型**（audit-service §5.7）+ **权限位 `admin` 或 `audit.read`**（多管理员/多角色可扩展：未来可加「审计员」而不授 admin）；M1-11 先落管理员门 + 过滤骨架，用户维度视图低优先级后续 |
| ~~Q13~~ | ~~层 B 是否本期收敛~~ | **已裁定：两层一并收敛**（`Group` 原子，不留半入账） |
| ~~Q14~~ | ~~`AuditStore` 迁移方案~~ | **已裁定：② 彻底**（类型迁入 `pkg/audit`；`Journal.Prune`/`Index.Evict` + 白盒测试迁移） |

---

## 11. 实施清单（评审通过后按序执行）

1. **M1**：`pkg/audit` 统一能力 + 薄委托 + **M1-3 二选一迁移** + 覆盖缺口全表 + `auditMiddleware` + 结构化字段/计数 + 轮转 T1–T8 + **M1-11 读面 ACL** + **M1-12 权限收紧** + **M1-13 TTL 隔离** + 门禁 R35（含 d/e）；
2. **W1**：`pkg/waitq` + 适配器（**等待侧+释放侧同提交**）+ 路径收敛（两层）+ 门禁 R36（含 R34 升级）；
3. **M1-8**：审计热段配额治理（有界保留）；
4. **M2**：WAL（`ReplayApplier` 契约 + 提交语义 + 两级序 + `Journal.Rewrite`）；
5. **M3**：`pkg/recovery` 协调器 + 收敛 5 处 + baidupcs 断点 + 块级校验 + 真实 kill e2e + R37；
6. **S4**：Web UI（审计字段/筛选 + 等待可观测）+ docs（含 `gates.md`）+ 真浏览器 e2e；
7. **M4**（后续）：归档到外部/加密卷 + 跨热/冷段 `Query`；
8. 全量验证：`go test ./...`（serial/sleep 棘轮）+ `make lint` + `make check-ci` + e2e + **压测** + 零丢失验证。

---

## 12. 修订记录（v2，经 3 路独立审核并逐条证伪）

| 审核发现 | 处置 |
|---|---|
| **revC P0**：审计读面无 ACL（`reader` 可读全量） | 终态 + M1-11 + E26 + R35-e + 用例（`reader ⇒ 403`） |
| revC P1：`__audit__` 可被注册占用 | §1 目标 + audit-service §5.6 前提 + M1-9 |
| revC P1：`max_total_bytes` 默认 0 | §8 非零安全默认（Q10） |
| revC P1：`SyncAlways` 默认 ⇒ 回归 | 默认 `SyncBatch`（§8） |
| revC P1：`pkg/audit` 分层矛盾 | §2 叶子核心 + 外置适配器 |
| revC P1：`Entry` 缺 `Level/Dim`；配置键重复；`S3` 阶段名歧义；E 编号（v1 写 E1–E15） | 均订正（audit-service §4.1/§8/§12；本文件 §3/§5/§8） |
| revB P1：WAL 幂等写成库保证；`MarkDone` 顺序未定义 | §4.3 + audit-service §4.2/§6 |
| revB P1：「用例原地通过」不成立（非导出字段） | M1-3 二选一（Q14 建议①） |
| revB P1：waitq I1 不可达；热段「预留」过度承诺；多节点矛盾 | waitq.md I1/W17；audit-service §5.4.2；§1 单机（Q11） |
| revA P1-1：per-scope Seq vs 全局段 | 两级序（audit-service §4.1） |
| revA P1-2：覆盖清单不全 + 兜底假绿 | 补 share/sync/DAV/notify/cloud-groups；transport 不计达标 |
| revA P1-3：包内两套 API | facade + R35-d |
| revA P1-4：0700 与现状不符 + `persist_dir` 不存在 | M1-12 + 现状改述 |
| revA P1-5/revB P2：E3×E4×T1 矛盾 | audit-service §5.4.1 有界保留 |
| revA P1-6：fsync 全站串行 | 单写者 + 压测判据 |
| revA P1-7：阶段依赖不自洽 | §3 显式：**M1-8 依赖 W1** |
| revA P2-4/revB P2：TTL 会删 WAL 行 | E25 + M1-13 |
| revA/revB/revC：计数与路径清单（~130/~35；7 条路径） | 订正：**115 处/20 文件**（+29/3）；**两层 ~40 处** |
| revC C6：门禁失控风险 | 新增 `docs/gates.md` + R 号双向校验 |

### 14. 修订记录（v4，第二轮独立审核）

| 审核发现 | 处置 |
|---|---|
| **三路均报 P1：`Entry.Owner` 无写侧来源** + `Principal.Owner` 用户可控 | audit-service §5.7 新增「Owner 写侧来源」5 条规则（单一来源 = AK 租户键；**严禁** `Principal.Owner`）+ M1-2 + E28 + 用例 |
| **revC P1：§5.7 门语义自相矛盾** | 改**单轴**（权限轴 = `Owner`；`Dim2` 降为 UI 归类） |
| **revB P1：保留名封堵单点 + 前提 2 自毁** | §5.6 改为 4 个身份/配置来源封堵 + 硬约束（**不得**改 `ValidSegmentName`/`IsReservedBucketName`） |
| **revC P1：§5.8 前提错误（只有 1 个受支持 admin）** | §5.8 现状订正 + 多 admin/`audit.read` 授予机制列为 M1-11 **前置** |
| **revA P1：段族矛盾 / 节点 seq 未续号 / Prune vs Rewrite** | §5.4.1 段族隔离 + E27；§4.1 节点 seq 持久化 + 载入排序；§4.2 统一 `Rewrite`+`Index.Prune` |
| 计数订正（三路均质疑） | `RecordAudit` **108/19** · `recordFileAudit` **29/4** · 配额侧共 **48 处**（层 B 8） |
| revC C1：单 PR 不现实 | **保持单 PR（用户裁定）**，但**提交按 P1–P8 批次边界**：P1 包骨架+facade+M1-12/13+R35-d → P2 M1-3 迁移+缺口+middleware+R35-a/c → P3 M1-9/M1-11/R35-e → P4 W1 → P5 M1-8 → P6 M2 → P7 M3+R37 → P8 S4 |
| revC C4：性能阈值缺失 | §8 补 `batch_max_lines`/`batch_max_interval`；验收阈值 = 「注入 500ms 慢 Journal 时业务 handler P99 增量 < X ms」+ HDD/SSD 吞吐基线 |
| revC C5：既有 append-only 原语 | §9 增 `pkg/state.LocalAppendStore` 取舍（不得两套共存） |
| revC C2：门禁口径 | R35-b 标口径待定（localMux 处置）；R36-b 口径 = **根 module `go list ./...`**（不下探嵌套 module ⇒ `kad.go` 自动不入扫描面） |

### 13. 裁定记录（v3，2026-10-19 用户问答）

| 项 | 裁定 |
|---|---|
| `AuditStore`/`AuditRing` 下沉 | **② 彻底**（类型迁入 `pkg/audit`）⇒ M1-3 含 `Journal.Prune`/`Index.Evict` + 白盒测试迁移 |
| 中断继续集群语义 | **本期单机** |
| 获取路径收敛范围 | **两层一并收敛**（quota + capacity） |
| 审计读面权限 | **两维模型**（用户维度脱敏可读自己 / 系统维度仅管理员 / 服务端强制）⇒ audit-service §5.7 |
| 审计存储归属（追问） | **保留系统 owner**（非管理员 owner / 非审计账号）；多管理员靠角色/能力位而非所有权 ⇒ 可平滑扩展 ⇒ audit-service §5.8 |
