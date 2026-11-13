<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient `trash`（回收站 list / restore / empty）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-trash.md` 已于 2026-11-13 归档。
> 命令与端点以 `docs/cli.md`（§trash）与 `docs/api.md`（§回收站 trash）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
服务端早有软删（`POST /delete?soft=true`）与回收站端点（list / restore / empty），
但 CLI 无入口 ⇒ 用户只能手工调 HTTP。目标是补 `sclient trash` 三子命令。

## 定案与取舍（仍有效）
1. **服务端零改动**：端点已存在且稳定，本任务纯新增 SDK 三方法与 CLI 命令。
2. **SDK 统一走既有 `doRequest`**（自动带 SproxySig 签名/隧道），不引入新鉴权路径。
3. **`restore` 的入参是 `trash_rel` 而不是原路径**（含 `.__deleted__<nano>` 后缀）：
   help 与 list 输出显式提示「恢复时请使用 trash_rel 列」；**不做原名模糊匹配**——
   同名多条时歧义不可消解（YAGNI）。
4. **`OutputFormatter` 只增不改**：新增方法由编译期强制 Text/JSON 双实现；仓库内若有
   mock 实现会一起编译失败 ⇒ 动手前先 `grep OutputFormatter` 找全部实现。
5. **边界**：回收站端点 owner 维度；trash 桶落**本地租户根** ⇒ 外部卷 `?soft=true`
   fail-closed（400）。多卷演进后命令为 `--volume` / `--all`，其中 `empty` **默认只清默认卷**
   （安全默认，绝不静默扩大删除范围）；`--all` 为显式动作。
6. **不扩 `delete` 命令**（软删入口不在本任务范围），文档标注「回收站依赖服务端软删配置/
   既有调用方」。

## 落地指针
`cmd/sclient/trash.go`（`NewCmdTrash` + list/restore/empty）、`pkg/client/trash.go`
（`ListTrash` / `RestoreTrash` / `EmptyTrash`）、`pkg/server/trash.go`（既有端点），
`cmd/sclient/output.go` 的 `PrintTrashList`。
