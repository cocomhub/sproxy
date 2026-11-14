<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# WebUI 节点拓扑 + 延迟/RTT — 归档

> **归档说明**：原文 `docs/designs/2026-09-24-webui-topology.md` 已于 2026-11-14 归档。
> 指标见 `docs/api.md` §Hub 节点质量指标；API 字段与 WebUI 见 `docs/api.md` §GET /api/hub/nodes。
> 本文件只保留**设计意图 / 取舍 / 外部契约 / 仍有效的判断**。

## 背景 / 目标
`writeHubNodeMetrics` 已有 per-node quality 分档 + `retransmits`/`errors`/`connected_seconds`，
但**缺** per-hop 实时 RTT/延迟值，WebUI 也无拓扑可视化（仅表格/文本）。目标：RTT 入 `/metrics`
（`sproxy_hub_node_rtt_ms`）+ `/api/hub/nodes` 增 `rtt_ms` 字段 + WebUI 节点拓扑图。

## 定案与取舍（仍有效）
1. **RTT 是纯增量观测，不改帧协议**：复用 mux 心跳既有 Ping/Pong（30s/90s），Pong 到达时
   `LastRTTNanos = now - pingSentAt`；**无 ping 在途时跳过**（防冒充/样本污染）——心跳机制本身不变（零回归）。
2. **RTT 缺失显式标记未知（禁假健康）**：gauge 与 API 字段用 **`-1`** 表示无样本（不写 0 误导）；
   WebUI 边用虚线 + `N/A`。
3. **API 带字段省一次抓取**：`/api/hub/nodes` 增 `rtt_ms`（与 `/metrics` **同一数据源**），WebUI 拓扑直接消费，
   不回调 `/metrics`；JSON 增字段对旧客户端向后兼容。
4. **WebUI 改动全量走硬规则**：`node --test`（纯函数）+ Playwright e2e + Makefile `web-test` 登记（R10）。
5. **大节点数降级不卡渲染**：节点数过大时降级为提示（本期标注上限）。

## 外部契约 / 踩坑
- 拓扑数据源 = `GET /api/hub/nodes`（轮询 5s）；边色分档 `<100ms` 绿 / `<500ms` 黄 / `≥500ms` 红，
  未知虚线 + `N/A`——阈值/分档是面板可见契约。
- `/metrics` 新增 gauge 不影响既有 series 解析（追加族）。

## 落地指针
- `pkg/tunnel/mux/mux.go`（`Metrics.LastRTTNanos` / `RTTNanosMax`）、`pkg/tunnel/mux/frame_handler.go`
  （收 Pong 更新）。
- `pkg/server/metrics/metrics.go`：`WriteHubNodeLinkGauge`（`sproxy_hub_node_rtt_ms` 等）；
  `pkg/server/hub_handler.go`：`/api/hub/nodes` 的 `rtt_ms` 字段（-1=无采样）。
- `web/static/app-render.js`：`topologyEdgeColor` / `topologySvg`。
- 测试：`pkg/tunnel/mux/rtt_test.go`、`pkg/server/hub_node_metrics_test.go`、
  `web/static/app-render.test.js`、`web/e2e/webui_topology_e2e_test.go`。
