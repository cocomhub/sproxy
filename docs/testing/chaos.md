<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 混沌测试（HA 故障注入）

> roadmap 11.10-⑧。设计稿：[`docs/archive/2026-09-24-chaos-testing.md`](../archive/2026-09-24-chaos-testing.md)（已归档）。

## 入口

```bash
make test-chaos      # go test -race -tags=e2e -run 'TestChaos_' ./test/chaos/...
```

- **不并入 `make test`**（时长可控）：`test/chaos` 全部文件带 `//go:build e2e` 门控，只有显式
  target 才编译运行。
- CI 由 `ci.yml` 的 **`chaos` job**（`Chaos tests`）执行 `make test-chaos`，同 job 还串行跑
  「限时协议面 fuzz」（两者同属「非常规、可慢、限定环境」类别，不拖长常规 test job）。

## 工具（`test/chaos/`）

| 文件 | 提供 |
|------|------|
| `node.go` | `ChaosNode`：包装 sproxy 子进程——`Start` / `Stop` / `Kill9` / `Restart` / `WaitExit` / `Pid` / `URL`（构建真实二进制 + 临时 config 启动，沿用 `test/e2e_test.go` 的 `startSPROXY` 模式） |
| `net_chaos.go` | `NetChaos`：**应用层** TCP proxy（`127.0.0.1` 监听转发到目标），`Pause` / `Resume` / `Delay`——透明于应用，暂停即复现网络分区 |
| `chaos_test.go` | 场景用例 + 签名上传/下载夹具 |

**无外部工具依赖**：分区/延迟都用应用层代理实现，不依赖 `tc netem` / `iptables`（CI 可跑、不因
工具缺失而跳过）。

## 场景

1. **`TestChaos_Kill9Restart`**（进程级混沌）：上传 → `kill -9`（不走优雅关闭）→ 重启（复用同一
   storage）→ 断言 **PID 变化**（证明真的重启而非复用进程）→ 业务恢复：文件仍可下载（checksum
   台账持久化生效）。
2. **`TestChaos_NetPartition`**（网络分区）：经 `NetChaos` 代理访问节点 → `Pause` 后请求失败/超时
   （有界 3s）→ `Resume` 后轮询恢复成功。

## 约定（写新场景时沿用）

- **127.0.0.1 回环绑定**（`0.0.0.0`/`localhost` 禁止）；端口用 `:0` 动态分配避免冲突。
- **`--config` 指向临时配置**隔离本机 `~/.config/sproxy`（否则测试被本机配置污染）。
- `t.Cleanup` 收敛子进程与代理；子进程异常退出即测试失败（fail-closed，不吞）。
- `-race` 下超时按正常值 3 倍留余量。

## 何时用

改动落在这几类**跨进程/跨链路不变量**时才值得加混沌场景：

- 进程生命周期与恢复（强杀后重启、临时文件/in-flight 会话清理、台账持久化）；
- mux 重连与心跳超时、hub 断链中途故障；
- 恢复期间的可观测性（`/metrics` 计数、`/healthz` 判据）。

**未落地**：设计里的 `LatencyInjection` 场景（注入 200ms 延迟断言下载仍完成）当前**只有
`NetChaos.Delay` 能力、无用例**；需要时按上表新增 `TestChaos_*` 即可（`make test-chaos` 的
`-run 'TestChaos_'` 会自动纳入）。
