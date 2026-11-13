<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient `quota` 命令 — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-sclient-quota.md` 已于 2026-11-13 归档。
> 命令用法以 `docs/cli.md`（§quota）为准（现行事实源），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
`/api/stats` 已返回 `Quota{usage,max_bytes,watermark}`，但 CLI 没有直接看配额的命令
（用户只能读 `stats` 大 JSON）。目标是加一条轻命令。

## 定案与取舍（仍有效）
1. **服务端零改动**：`GetStats` 方法不变，旧服务端无 quota 段 → 零值（零回归）。
2. **不扩展 `OutputFormatter` 接口**：该接口在 `cmd/sclient` 内，加方法要同时改两个实现与
   测试 mock（13 方法 × N 实现）。命令内按根 `--json` flag 分支输出，沿用 `sync.go` 的
   `printSyncTaskResult` 先例——**少量输出场景不值得动公共接口**。
3. **空/不限语义显式化**：`max_bytes <= 0` 显示「不限」、水位显示「0%（不限）」，
   不让「0」被误读成「用满了」。
4. **不在 CLI 侧推导卷维度**：quota 是服务端语义；命令只透传服务端视图。
   （后续演进：服务端改为**跨卷联邦视图**——集中树水位 + 逐卷明细——CLI 随 `--json`
   原样输出，不需要客户端拼装。）

## 落地指针
`cmd/sclient/quota.go`（`NewCmdQuota`，注册于 `root.go`）；`pkg/client` 的 `GetStats` 复用不变。
