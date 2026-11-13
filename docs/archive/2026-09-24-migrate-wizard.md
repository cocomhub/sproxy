<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 迁移向导（11.7-⑩）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-migrate-wizard.md` 已于 2026-11-13 归档。
> CLI 用法以 `docs/cli.md`（§migrate）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
跨机搬迁原先只能手动 download/upload，多卷/联邦配置需手编 YAML。目标：`sclient migrate` 向导——
导出（清单+文件）→ 导入（目标机校验还原）→ 镜像/联邦配置生成。

## 定案与取舍（仍有效）
1. **manifest 原子写 + schema/checksum 自检**；缺失/损坏/schema 不符 ⇒ **拒绝导入**
   （fail-closed，防静默错迁）。
2. **绝不假成功（关键红线）**：checksum 校验失败 ⇒ 该文件标 FAILED，**绝不上报成功**；目标已存在且
   checksum 相同 ⇒ SKIPPED（幂等）；同名不同 checksum ⇒ CONFLICT 报错（非零退出）。
3. **默认快速失败**：单文件失败即中止后续；`--ignore-errors` 才跳过继续（失败清单含原因）。
4. **源/目标预检**：先探活（stats/version）再开跑，避免跑一半才发现目标不可达。
5. **服务端零改动**：纯新增命令 + `pkg/migrate`，复用既有 list/download/upload/分块/续传；
   大数据量靠分块/续传、manifest 断点重导（已 SKIPPED 直接跳过）收敛。

## 落地指针
`pkg/migrate`（`Manifest` / `Exporter` / `Importer` / `MirrorConfig`）、`cmd/sclient/migrate.go`
（`export` / `import` / `mirror-config` 子命令）。
