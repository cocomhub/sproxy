<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# StateStore（可插拔状态后端：local / mongo）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-statestore.md` 已于 2026-11-13 归档。
> 配置字段以 `docs/config.md`（`state_store.*`）、装配形态以 `docs/architecture.md` 为准
> （现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
单机 JSON 文件 store（凭据 Ring、卷状态等）无法支撑多节点：既没有共享读写，也没有
CAS / Watch 这类一致性原语。目标是把「状态」抽成可插拔后端（`local` 默认 + `mongo` 集群），
并让**缺省路径逐字零回归**。

## 定案与取舍（仍有效）
1. **绝不允许静默回落 local**：配置了未注册/未实现/连不上的后端 ⇒ 启动失败（fail-closed）。
   理由：回落会造成「以为多节点一致、实际各写各的」这一最坏情况——比启动失败危险得多。
   `raft` 只留注册位不实现，配置即报「未实现」。
2. **后端隔离**：Mongo 实现放独立 module（`pkg/state/ext/mongo`），只有装配层引
   `mongo-driver` ⇒ 核心 `pkg/state` 保持零外部依赖（同 `pkg/tunnel/xfer/ext/*` 模式）。
3. **不做数据迁移工具**：`local → mongo` 由运维脚本/外部完成；本期只保证**读旧格式兼容**。
   装配层把既有 JSON 文件**原样保留**为「StateStore 之上的首版导入」——`NewLocalStateStore`
   只扫 `<root>/state/`，既有 `meta/` 路径由各 Store 适配器在**读路径**回退 ⇒ 不要求一步搬文件
   （零回归的关键：不搬迁、不双写、不改既有文件布局）。
4. **key 规范是三段式** `<owner>/<type>/<name>`，每段经 `validateSegment`（拒 `..`、绝对路径、
   空字节、Windows 非法字符）；`name` 段内部允许 `/`（checksum 的 rel），落盘时映射为目录层级。
   段校验是**路径安全边界**，不能因为「值来自内部」而省。
5. **CAS 是核心原语**：语义等价于「读当前 → 比对 → 原子替换」；Mongo 侧靠版本号字段 +
   单文档 findAndModify（或事务内读-比-改）。**并发 CAS 正确性是验收红线**，不是可选优化。
6. **Append / Watch 也要有界**：追加批量带上限、超限滚动到新 seq 文档（否则单文档无限增长）；
   Watch 首选 Change Streams，**不支持时必须降级为 `_id` 范围轮询并日志告警**（可观测铁律，
   禁静默降级）。
7. **装配期探活**（`Ping`，仿 Vault 启动探活）：连不上要在启动时就报，而不是等第一次写。

## 外部契约 / 踩坑
- `state_store.type` 取值与语义（`local` / `mongo` / `raft`）是运维可见契约；新增类型必须
  走注册表而不是加分支。
- Mongo 文档形状（`_id = key`，`{v, rev}`）与集合名（默认 `sproxy_state`）一旦落盘即为兼容面。
- 集群模式下「主写副本读」的转换依赖本设计（见 `cluster-state-migration` 方向），
  即：`cluster.enabled` 或 `type != local` 时状态切 StateStore。

## 落地指针
`pkg/state/{state.go,local.go,append_local.go,registry.go,key_test.go}`（接口 / local 实现 /
注册表 / key 规范）、`pkg/state/ext/mongo/{store.go,leader.go,register.go}`（独立 module）。
