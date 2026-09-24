# WebUI 节点拓扑 + 延迟/RTT（11.1-⑦）

## 背景 / 目标
- 现状：`pkg/server/metrics.go` `writeHubNodeMetrics` 已有 per-node quality 分档（0=healthy 1=degraded 2=stale）+ `retransmits`/`errors`/`connected_seconds`，经 `/metrics` 输出；`/api/hub/nodes` 带 quality。**缺**：per-hop 实时 RTT/延迟值（metrics 只有累计重传，无当前 RTT）+ WebUI 无拓扑可视化（仅表格/文本）。
- 目标：RTT/延迟入 `/metrics`（`sproxy_hub_node_rtt_ms` gauge）+ WebUI 节点拓扑图（app.js 渲染节点-边图 + Playwright e2e）。

## 组件与接口
- `pkg/tunnel/mux`（metrics.go 同构）：mux 帧心跳已有 Ping/Pong（30s/90s），增加 **RTT 采样器**：`mux.Metrics` 增 `LastRTTNanos atomic.Int64` / `RTTNanosMax atomic.Int64`（Pong 到达时 `now - pingSentAt` 更新；无 ping 在途时跳过）。
- `pkg/server/metrics.go`：`writeHubNodeMetrics` 增 `sproxy_hub_node_rtt_ms{node}` gauge（`n.Mux.Metrics().LastRTTNanos/1e6`）+ 可选 `sproxy_hub_node_rtt_ms_max`。
- `GET /api/hub/nodes`：现有响应已带 quality；增 `rtt_ms` 字段（同一数据源，WebUI 拓扑直接消费，避免再抓 `/metrics`）。
- WebUI：`web/static/` app.js 拓扑视图（SVG/Canvas 渲染：hub 中心节点 + 叶子节点 + 边按 RTT 着色；点击节点看 retransmits/errors/quality）；数据源 `GET /api/hub/nodes`（轮询 5s）。新 JS 登记进 Makefile `web-test`（R10 门禁）。

## 数据流
1. mux 心跳层：每 30s 发 Ping 记录 `pingSentAt`；收 Pong → `LastRTTNanos = now - pingSentAt`。
2. `/metrics` 与 `/api/hub/nodes` 读 `n.Mux.Metrics().LastRTTNanos` → 输出 RTT。
3. WebUI 拉 `/api/hub/nodes`（含 rtt_ms）→ 拓扑渲染：边色 = RTT 分档（<100ms 绿 / <500ms 黄 / ≥500ms 红）；节点 = quality 分档色。
4. 轮询刷新；RTT 缺失（新节点/未采样）→ 边虚线 + "N/A"（不假报）。

## 错误处理
- RTT 无样本（Pong 未回）→ 输出 0 会误导：gauge 用 -1 或省略（写 `-1` 显式标记未知，文档说明）。
- `/api/hub/nodes` 拉取失败 → WebUI 拓扑显示重试横幅，不白屏（保留既有表格兜底）。
- 节点数大（>200）→ 拓扑图降级为分页/聚合（本期标注上限，超限显示提示不卡死渲染）。

## 测试 + 变异点
- `TestMuxRTT_UpdatedOnPong`：收 Pong → LastRTT 更新（变异：不更新 → 红）。
- `TestMetrics_RTTGaugeOutput`：节点带 mux → `/metrics` 含 `sproxy_hub_node_rtt_ms` 且值=期望毫秒（变异：gauge 缺失/单位错误 → 红）。
- `TestHubNodesAPI_RTTField`：`/api/hub/nodes` 响应含 rtt_ms（变异：字段缺失 → 红）。
- 纯函数：拓扑边色分档函数 `topologyEdgeColor(rtt int64) string`（node --test 单测，变异：分档边界错 → 红）。
- Playwright e2e：mock `/api/hub/nodes` 返回 3 节点 → 页面渲染 3 节点 + 边颜色正确（变异：渲染缺失节点 → e2e 红）。

## 片划分
- P1：mux RTT 采样 + `/metrics` gauge + `/api/hub/nodes` rtt_ms 字段（纯服务端，含单测）。
- P2：WebUI 拓扑渲染（app.js + node --test + Playwright e2e + R10 登记）。
- P3：边色/RTT 阈值可配置 + 大节点数降级 + 真浏览器手工复核（playwright-cli）。

## 风险与零回归
- 心跳 Ping/Pong 既有机制不变：RTT 是纯增量观测，不改变帧协议（零回归）。
- `/metrics` 新增 gauge 不影响既有 series 解析；`/api/hub/nodes` 增字段对旧客户端向后兼容（JSON 增字段不破坏）。
- WebUI 改动全量走既有硬规则：node --test + Playwright e2e + `make web-test`。
- RTT 缺失显式 -1/虚线，禁静默当 0（避免假健康）。
