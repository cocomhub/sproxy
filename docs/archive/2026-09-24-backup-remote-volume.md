<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 备份到远端卷（roadmap 11.5-⑧）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-backup-remote-volume.md` 已于 2026-11-13 归档。
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**；HTTP 契约以 `docs/api.md` 为准，
> 实现以代码与 git 历史为准。

## 背景
联邦卷的 FS 早已具备写面（`WithWriter(sync.FS)` 注入，装配层经远端 client 触达远端 hub 卷），
但**没有备份编排**。目标：通用备份引擎（源任意 `sync.FS` → 目标任意 `sync.FS`），
以联邦写面作为远端目标；全量/增量 + 每文件可选校验 + 报告。

## 定案与取舍（仍有效）
1. **泛化于 `sync.FS`，不为备份新造接口**：「换目的地只换 FS」——本地→本地即可完整单测，
   远端只是把目标换成联邦写面。这是本设计最重要的一条：备份引擎不得依赖卷/后端的任何具体类型。
2. **增量靠 manifest 比对（size + mtime）**，manifest 单文件**原子写**（tmp + Rename）；
   manifest 损坏 ⇒ Warn + 全量重扫，**绝不中断备份**（备份的正确性优先于省流量）。
3. **失败语义显式化**：单文件失败重试（指数退避）后记 `Failed` 并继续（错误隔离）；
   结果 **HTTP 200 + Report**（`failed`/`errors` 显式列出，不静默），CLI 侧以非 0 退出码表达「部分失败」。
   目标无写面（只读）⇒ **fail-fast**「备份目标只读，需装配远端写面」，不做「假装成功」。
4. **ctx 取消 ⇒ 返回已拷贝部分 + `Truncated=true`**（而不是丢弃报告或返回裸错误）。
5. **备份是内容级语义**：按逻辑文件备份，与硬链/去重物理层无关 —— 不要用「物理省空间」推导备份行为。

## 关键陷阱（务必保留）
**manifest 落在 user 桶时是用户 API 可读写的路径**，可被覆写/伪造 ⇒ 增量比对会**静默跳过**该文件
（表现为「备份成功但内容陈旧/缺失」）。因此装配层应把 manifest 移到**独立功能桶**，或至少把它
当不可信输入（这也是 `Options.ManifestRel` / `ManifestFS` 存在的原因）。
同类教训：备份链路上的任何「元数据文件」都要先问「谁能写它」。

## 与设计稿的差异（避免误读）
设计里设想的 sclient 入口 `backup <target> [--incremental]` **未接线**；当前 `sclient backup`
是「把卷导出为 tar 备份到本地」的另一功能（见 `docs/cli.md`）——两者不可混为一谈。
远端备份当前的可调用面是 HTTP `POST /api/backup`。

## 依赖与外部契约
- 远端写面 = 卷后端的联邦写面（外部契约）；配额记账经**逐文件写前预留**（owner user 桶 Scope + 目标卷容量池）。
- 备份条目上限/报告字段/错误码属对外契约（见 `docs/api.md`）。

## 落地指针
`pkg/backup`（引擎 `Run` / `Options` / `Report`）、`pkg/server/backup_api.go`（`POST /api/backup` 装配）。
