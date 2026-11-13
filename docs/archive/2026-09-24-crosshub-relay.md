<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 跨 hub 数据面中继（11.1-⑥）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-crosshub-relay.md` 已于 2026-11-13 归档。
> 现行事实源：`AGENTS.md` 能力矩阵与 `docs/architecture.md` §S14；
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
联邦**发现面**已完整（跨 hub 服务发现、按 mesh 定位目标节点注册在哪个对端 hub）。缺的是**转发执行体**：
本 hub 路由表未命中目标节点时，把 `RelayStreamRequest`（CONNECT 语义）经上游 hub 转发。

## 定案与取舍（仍有效）
1. **能力边界**：`FederationForwarder` 只负责「**向一个**联邦对端转发一条拨号」+ 防环头；
   HTTP 解析、多对端故障转移、回写**留在装配层** `RelayStreamHandler`（防领域包反向依赖装配层）。
2. **防环 fail-closed**：`X-Relay-Hop` / `X-Relay-Path` 记录 hubID 链；`DefaultRelayMaxHops=4` 是
   硬上限，超限/回源 → 508；环检测命中 → **直接拒绝**（宁可拒绝也不无限转发）。
3. **mesh 严格隔离**：转发目标必须 `n.Mesh == mesh`（复用发现面语义，防跨 mesh 泄漏）。
4. **零回归**：路由表命中路径**先行判断**，联邦转发仅在未命中后触发；未装配联邦时未命中即 404
   （行为同现状）。
5. **故障转移按序尝试**：多对端首个失败 → 下一个，全部失败才报错（与 mesh 多候选回退一致）。

## 与设计稿的差异（避免误读）
设计稿拟在 `pkg/tunnel/hub` 增 `ForwardRequest`；**实际落在 server 作用域**：`pkg/server/federation`
（`FederationForwarder` / `NewFederationForwarder` / `StatusError` / `DefaultRelayMaxHops`）。

## 落地指针
`pkg/server/federation/forward.go`（`Forward` / `PeersForNode` / `MaxHops` / `Close`、
`RelayForwardHopHeader` / `RelayForwardPathHeader`）；装配层故障转移在 `pkg/server/relay_stream.go`。
