<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 混沌测试（HA 场景故障注入，11.10-⑧）— 归档

> **归档说明**：原文 `docs/designs/2026-09-24-chaos-testing.md` 已于 2026-11-13 归档。
> 场景/工具/入口以 `docs/testing/chaos.md` 为准（现行事实源），本文件只保留**设计意图 /
> 取舍 / 外部契约 / 仍有效的判断**。

## 背景与意图

`test/e2e_test.go` 原有 `TestChaos_*` 只覆盖「进程 kill 后恢复」的粗粒度场景；缺**网络分区 /
延迟注入 / 断链重连中途故障**的确定性混沌。目标：新增 HA 故障注入框架，且**不依赖外部工具**
（无 `tc`/`iptables` 也能在 CI 跑）。

## 定案与取舍（仍有效）

1. **应用层代理兜底网络故障**：`NetChaos` 用 `net.Listen` 的 TCP 转发 + Pause/Resume 复现分区，
   比 `tc netem` 更可控（确定性、无 root、跨平台）；代价是只能模拟「连接层断/卡」，不能模拟丢包率。
2. **PID 变化作为「真重启」判据**：断言重启后 PID 不同，才能防「复用进程」式假通过。
3. **不并入 `make test`**：混沌天然慢且依赖子进程，独立 target + e2e build tag 门控，避免常规
   测试时长被拖；CI 单独 job（超时上限防 I/O 塌陷式挂死，处置沿用 `gh run rerun --failed`）。
4. **子进程失败即测试失败**（fail-closed 不吞）；恢复轮询设超时（`-race` 下 3 倍）。
5. **`--config` 隔离**是硬要求：否则本机 `~/.config/sproxy` 会污染子进程配置。

## 落地指针

`test/chaos/node.go`（`ChaosNode`）、`test/chaos/net_chaos.go`（`NetChaos`）、
`test/chaos/chaos_test.go`（场景）；入口 `make test-chaos`；CI job `chaos`（`.github/workflows/ci.yml`）。

## 实测差异（以代码为准）

- 设计写 3 个场景（`Kill9Restart`/`PartitionReconnect`/`LatencyInjection`），**实际只落地前两个**；
  且第二个场景名最终叫 `TestChaos_NetPartition`（代理 `Pause`/`Resume` 复现分区），比设计描述的
  「hub 节点间断连 + mux 重连 + `/metrics` 断言」更轻（不建 hub 拓扑、不断言重传指标）。
- `NetChaos.Delay` 能力已实现但**无对应用例**（延迟注入场景未落地）。
- 设计提的「CI job 超时上限 15m」未在 ci.yml 显式设置；当前靠 job 步内 `make test-chaos` 自身结束。
