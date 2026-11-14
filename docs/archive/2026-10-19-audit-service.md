<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 审计服务（`pkg/audit`：作用域 · WAL · 有界保留 · 归档冷读）— 归档

> **归档说明**：原文 `docs/designs/2026-10-19-audit-service.md` 已于 2026-11-14 归档。
> 能力见 `AGENTS.md`（能力矩阵 `pkg/audit`）、`docs/api.md` §审计（`/api/audit*`）、`docs/architecture.md`、
> `docs/testing/audit-scenarios.md`、`docs/gates.md`（R35 族）。本文件只保留**设计原则 / 不变量 / 架构 /
> 条目模型 / 裁定 / 落地指针**。

## 1. 设计原则（多为**编译期/结构**强制，非约定）
1. **唯一提供方**：审计能力只由 `pkg/audit` 提供；其他包只装配 + 使用。
2. **一切审计皆有作用域**：`Open(scopeID)`；**`Emit` 定义在 `*Scope` 上**（`func (s *Scope) Emit(...)`）
   ⇒ 用法上**无法绕开 scope**（真编译期强制；不再宣称 `Entry.Scope` 字段能做到）。
3. **活跃记录在内存 + 轮转**：内存超配置 / 内容过时 / 读取时 / 显式 / scope 结束 / 进程退出 ⇒ 立即轮转。
4. **结构体能力、零全局可变状态**：`*Service` 由装配层注入；`Options` 在 `New()` 时**一次性 resolve 成不可变
   effective 配置**；测试自建 service + MemoryJournal ⇒ 隔离。
5. **通用抽象 + null-object**：`Journal`/`Index`/`Notifier`/`Clock`/`Policy` 均为接口，各配
   `Null*` 空对象 ⇒ `Emit` **永不分支**。
6. **包提供全部能力，使用方选择性启用**（`CapabilitySet`：合规形态/span/采样/WAL/归档/索引/通知/加密存储/
   配额治理逐项可选）；测试与门禁按**少量规范剖面**（`compliance-only`/`telemetry-only`/`full`/`disabled`）。
7. **分层**：`pkg/audit` 核心为**叶子包**（不 import 装配层、不 import `waitq`）；**配额、归档、读面 ACL
   均为外置适配器**（装配层或 `pkg/audit/adapters/*`）⇒ 不违反 R4。
8. **不阻断业务**（默认 best-effort）+ **可选 fail-closed**；任何失败**可观测**（计数 + 告警）。
9. **有界保留**：内存与磁盘**都**必须有上界；「不丢」限定为「在配置上限内不丢」，超限**显式丢弃 + 计数 + 告警**。
10. **幂等是使用方契约**：`Resume` 的 apply 函数**必须幂等**（`ReplayApplier` 接口契约 + 红灯用例锁定；库不提供幂等）。
11. **单机模型**：段在**各节点本地** ⇒ **跨节点恢复不在本期范围**（`Lease` 仅在「共享段存储」方案下成立，本期不引入）。
12. **读面 ACL（P0）**：审计读面**必须**管理员角色（`audit.read`）。

## 2. 架构与条目模型
- `Service`（结构体，注入式）：`Scope`（`Open`/`Close`/`MarkDone`；**`Emit` 在 `*Scope` 上**）、
  `Span`（`Begin/End/Fail`）、`Save/Get`（作用域 kv，**落盘 = WAL**）、`Resume/ListScopes`（`ReplayApplier` 契约）、
  `Rotate(reason)`；下挂 `Journal`（append-only）/`Index`（内存活跃索引，有界）/`Notifier`/`Clock`（`Now` + `Since`，可注入 ⇒ 过时/老化可测）。
- **`Kind`**：`event`（合规）/`span`（过程）/`state`（WAL Save）/`done`（终态，恢复时跳过）/`resource`（采样）/`rotate`（审计自身可审计）。
- **两级序**：`Scope` + `Seq`（**scope 内**单调，Resume 的权威顺序）。
- **WAL 与「中断继续」**：`state` 记录落盘 + `done` 终态；`MarkDone` 顺序是 P1（提交语义）；与统一恢复协调器
  （`pkg/recovery`）协作——**审计与中断继续共用一条 append-only 流**（恢复过程本身也是审计证据），
  但 **TTL/清理只作用于合规全局段，绝不触碰 WAL 段**。

## 3. 关键裁定
- **审计空间 = 新用户类型 `system` 的真实租户**（`audit.owner` 默认 `audit`，`Role=system`；启动期预建 fail-fast）：
  与普通用户隔离、可断言归属；**不允许**改为普通用户 owner。
- **审计权限模型（单轴 `Owner`）**：用户/系统仅作 UI 归类；门禁按 `admin` 或 `audit.read` 能力位。
- **存储归属**：保留系统 owner（多管理员扩展性优于管理员 owner）。

## 4. 落地指针
- `pkg/audit/`（`core.go`/`service.go`/`journal.go`/`index.go`/`wal.go`/`archive.go`/`cold.go`/`perm.go`）、
  `pkg/server/audit.go`（装配 + `auditMiddleware` + 读面 ACL + `/api/audit*`）、`cmd/sproxy`（wire）、
  门禁 R35 族（`internal/archcheck/audit_*_gate_test.go` + `mutating_route_audit.tsv`）。
