<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 卷备份/导出（export / import，tar 流 + manifest）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-volume-export.md` 已于 2026-11-13 归档。
> 端点以 `docs/api.md`（§卷运维 · `GET /api/volumes/export`）、命令以 `docs/cli.md`（§backup）
> 为准（现行事实源），本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
卷跨实例迁移/备份此前没有统一出口：要么手抄文件，要么依赖底层 FS。目标是 `GET/POST
/api/volumes/export|import` 流式 tar（含每文件校验清单），CLI 侧 `sclient backup` 落地。

## 定案与取舍（仍有效）
1. **流式，不整卷入内存**：边读边写 tar，响应流可中断（ctx 取消即止）——卷可能极大，
   「先打包到临时文件」会把磁盘问题换成内存问题。
2. **manifest 是自证清单**：tar 尾部附 `manifest.json`（条目相对路径 + SHA-256 + size +
   mtime + 台账 checksum 交叉校验）⇒ 导入方可逐条审核，不必信任来源介质。
3. **导入必须复用既有写路径**（`ValidateFilePath` + 配额 `Reserve` + 写后 checksum 登记），
   **不 bypass 台账**——否则导入出来的对象在配额与 checksum 账本上是隐形的。
4. **绝不写卷外**：条目路径穿越由 `ValidateFilePath` 拒绝 ⇒ 该条目**跳过 + 报告**，
   不是中止（也不静默继续）。manifest 校验不一致的条目同样跳过并计入报告。
5. **默认宽松 + `strict` 可选**：导出途中卷内文件被改/删时默认「记错误继续」（能救回多少算多少），
   `strict=true` 才中止（需要一致快照的场景）。
6. **导入半途失败保留已写入条目**（可重跑）；同名覆盖需要显式 `--overwrite`，否则跳过已有
   （幂等重试而不是「第二次全废」）。
7. **权限与读写面同源**：导出 = 读卷权限，导入 = 写卷权限 + 配额；默认不挂任何定时行为。

## 落地指针
`pkg/server/volume_export.go`（`exportVolumeHandler` / `importVolumeHandler`）、
`pkg/client/export.go`（`ExportVolume`）、`cmd/sclient/backup.go`（`backup` / `backup restore`）；
测试 `pkg/server/volume_export_test.go`。
