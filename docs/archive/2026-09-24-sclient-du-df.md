<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient `du` / `df`（目录占用 + 卷水位）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-du-df.md` 已于 2026-11-13 归档。
> 命令与端点以 `docs/cli.md`（§du / §df）与 `docs/api.md`（§GET /api/du）为准（现行事实源），
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
`/api/stats` 只有**全局瞬时快照**，CLI 无「按目录递归占用」与「卷/磁盘水位」两类日常运维视图。

## 定案与取舍（仍有效）
1. **两个命令走两条不同数据路径**：`du` 需要新端点（服务端算）、`df` **不新增端点**
   （直接消费 `/api/stats` 既有 `Quota{usage,max_bytes,watermark}` + `storage_*` + `disk_*`）。
   不为已有数据再造一个端点。
2. **`du` 的暴露面刻意收窄**：只返回聚合数字（dirs/files/size），**不含文件名/内容**；
   定位经 `locateForRead` 收口（ACL），未命中 → 404，**不泄露卷存在性**。
3. **遍历语义必须与 `stats` 同源**：抽共享 bucket 判定 helper（跳过 cloud/chunk/version/meta/
   魔法目录/`checksums.json`/`LAYOUT_VERSION`），只统计 user 桶子树——两份实现漂移会让
   `du` 与 `stats` 长期给出不同数字（设计上以变异点防漂移）。
4. **零回归**：`/api/stats` 字段与语义一字不动；既有命令行为不变；新端点默认无影响。
5. 输出人类可读大小（本地 helper，不引入单位库依赖）；`--json` 走既有持久 flag，
   与其它命令一致。

## 落地指针
`pkg/server/du.go`（`duHandler` / `duResponse`，`GET /api/du?path=&volume=`）、
`cmd/sclient/du.go`（`NewCmdDU`、`NewCmdDF`）、`pkg/client` 的 `Du` 方法。
