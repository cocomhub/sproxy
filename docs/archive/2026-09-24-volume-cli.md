<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 卷操作 CLI（`sclient volume copy` / `move` / `rebalance`）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-volume-cli.md` 已于 2026-11-13 归档。
> 命令用法以 `docs/cli.md`（§volume）为准（现行事实源），本文件只保留
> **设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图
服务端已有 `POST /api/volumes/{copy,move,rebalance}`，但 `sclient volume` 只有
create/list/delete ⇒ 卷间操作无法脚本化。目标是把它们接进 CLI。

## 定案与取舍（仍有效）
1. **客户端先行，服务端只做核对/补缺**：设计稿提到的 `mirrorVolume` / `rebalanceVolumeHandler`
   与当时的代码现状**对不上** ⇒ 实施第一步先全局定位真实 handler（`pkg/server` / `pkg/files`），
   找不到才补最小 handler。这是本设计**已核对过的方法论**：先定位再动手，勿按设计稿名字盲写。
2. **`move` 目标冲突必须 409，绝不在 CLI 层静默覆盖**：卷唯一性 / 覆盖写语义（stay-home 口径）
   不得被 CLI 绕过——静默覆盖会造成跨卷双份 + owner 双计（配额账本被污染）。
3. **复用既有上下文**：`--vol`（卷上下文，持久 flag）与 `cd` 当前目录语义；`copy`/`move` 的
   `--from-volume` 缺省取 `--vol`，`--to-volume` 必填。
4. **`rebalance` 是异步语义**：CLI 只提交并打印「任务已提交」，不假装同步完成（进度看
   `/api/volumes/*` 任务接口与 `/metrics`）。
5. **零回归**：只加子命令与客户端调用，不改服务端既有端点行为。

## 落地指针
`cmd/sclient/volume_ops.go`（`newCmdVolumeCopy` / move / rebalance）、`cmd/sclient/volume.go`
（子命令注册）、`pkg/client/volume.go` 的 `CopyVolume` / `MoveVolume` / `RebalanceVolume`；
测试 `cmd/sclient/volume_ops_test.go`。
