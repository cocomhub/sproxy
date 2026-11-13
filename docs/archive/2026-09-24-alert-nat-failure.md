<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# NAT 穿透失败告警（11.1-①）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-alert-nat-failure.md` 已于 2026-11-13 归档。
> 配置项以 `docs/config.md`（`alerts.rules[]` 的 `source=nat_failure`）为准，
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景
hub/relay/webrtc（STUN/TURN）拨号失败原先只有日志，未接告警：出口断链、打洞失败等网络故障运维无感。
目标：补 `nat_failure` source + 失败/恢复事件接口。

## 定案与取舍（仍有效）
1. **告警是旁路副作用，绝不吞错**：包装拨号闭包，失败 → 记录告警后**原样返回错误**——fail-closed
   的拨号传播语义必须不变（出口失败不静默回退本地）。
2. **per-peer 去抖 + 自动恢复**：告警 key = `nat_failure\x00<peer>`；同 peer 后续拨号**成功**即发
   恢复通知（recover 对非 firing 是 no-op，可安全重复调用）。
3. **无规则零输出**：仅当规则显式引用 `nat_failure` 才生效；`engine == nil` ⇒ 包装 no-op（零开销、
   零回归），既有 dial 路径不触碰。
4. **恢复依赖「后续成功拨号」**：故障期间无新拨号则保持 firing——这是**有意**（避免误报恢复）。

## 落地指针
`pkg/server/alerts/alerts.go`（`SourceNATFailure` / `OnNATFailure` / `OnNATRecovered`）、
`cmd/sproxy/cloud_exit.go`（`withNATAlert`）、`cmd/sproxy/mesh_node.go`（出口/mesh 拨号挂点）；
配置 `alerts.rules[].source: nat_failure`。
