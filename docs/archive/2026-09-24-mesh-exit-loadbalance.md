<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 多出口负载均衡（11.1-④）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-mesh-exit-loadbalance.md` 已于 2026-11-13 归档。
> CLI 用法以 `docs/cli.md`（`--exit-group-mode` / `--exit-group-weight`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
出口节点组原先只有按序 failover（首节点永远吃满流量，故障才切下一个），多出口带宽闲置。
目标：补 `failover | round-robin | weighted` 三模式。

## 定案与取舍（仍有效）
1. **默认 failover 逐字节兼容**：`PickExitGroup` 恒 0 = 原按序行为；**旧构造器保留并委托**
   新模式构造器（现有调用方零改动，编译期保证）。
2. **模式只影响「起点选择」，不改变失败恢复路径**：每连接用 atomic counter 选起点，从起点
   **循环 failover**（选中节点失败 → 试组内其余）——切换模式不牺牲可用性。
3. **确定性扇区轮转（非随机加权）**：长连接分布等效，换来决定性可测；如需随机加权属后续片
   （接口不变）。
4. **fail-closed 校验**：未知 mode / 非法 weight 语法 ⇒ CLI 启动报错；weighted 缺权重 ⇒ 等权回落 +
   Warn（**不静默**）。
5. **并发安全**：counter 用 atomic（与既有 raceDial 语义一致）。

## 落地指针
`pkg/tunnel/mesh/exit_route.go`（`ExitGroupMode` / `PickExitGroup` / `NormalizeExitGroupMode` /
`NewExitGroupDialWithMode`）、`cmd/sclient/internal/meshconn`（`--exit-group-mode` /
`--exit-group-weight` flag 与 `AutoDial` 接线）。
