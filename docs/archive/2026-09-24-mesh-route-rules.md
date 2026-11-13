<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# mesh 域名/网段分流（11.1-③）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-mesh-route-rules.md` 已于 2026-11-13 归档。
> CLI 用法以 `docs/cli.md`（`--route` 分流规则）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
出口策略原先只有 `--exit-group` 按序 failover 与 `--exit-auto` 能力候选；不同目标走不同出口
（如内网 A 出口、公网 B 出口）无法表达。目标：`--route <domain|cidr>=<exit-group>` 分流，
socks/udp/http-proxy/mesh connect 统一出口选择。

## 定案与取舍（仍有效）
1. **未命中回落默认出口**（`--exit` / `--exit-group` / `--exit-auto` / 本地直连）——**零回归**
   （默认空规则 ⇒ 走原路径）。
2. **域名规则用字符串后缀匹配、不依赖 DNS**（避免解析副作用）；CIDR 用 `netip.Prefix` 前缀匹配。
   边界明示：cidr 规则对纯域名 host 不命中（回落默认）。
3. **首命中即止**（声明序，文档化）；`--route` 与 `--exit-only` **语义冲突 ⇒ 拒绝**
   （恒经出口时分流无意义）；与 `--exit*` 可共存（route 优先级更高，未命中才用默认）。
4. **fail-closed 解析**：非法格式/CIDR/空组 ⇒ 命令启动报错并列出具体规则文本，**不静默忽略**。
5. **出口选择收敛在单点**（`AutoDial`）：socks/http-proxy 天然收敛、一处改造全部生效；udp map 是
   固定单出口，命中 remote host 时替换其出口组。

## 落地指针
`cmd/sclient/internal/meshconn/meshconn.go`（`RouteRule` / `RouteKind` / `ParseRoutes` /
`parseRouteRule` / `SelectRoute`）；flag 与用法见 `docs/cli.md` §`--route`。
