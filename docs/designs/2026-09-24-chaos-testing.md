# 混沌测试（11.10-⑧）

## 背景/目标
- 现状：`test/e2e_test.go` 有 `TestChaos_*`（crash 恢复类），覆盖了进程 kill 后恢复的粗粒度场景；但**无网络分区、无延迟注入、无 hub 断链/重连中途故障**的确定性混沌。
- 目标：新增 HA 场景故障注入框架：① 确定性进程级混沌（kill -9/重启恢复）；② 网络分区（`tc netem`/iptables 或纯应用层 proxy 断连）；③ 延迟注入（应用层 proxy sleep）；不依赖外部工具时用应用层代理路径兜底（纯 Go，CI 可跑）。

## 组件与接口
- `test/chaos/`（新包，构建真实二进制 + 子进程启动，沿用 `test/e2e_test.go` 的 `startSPROXY` 模式）：
  - `ChaosNode`：包装 sproxy 子进程（`Start`/`Stop`/`Kill9`/`WaitRestart`），进程身份校验（PID 变化断言——重启后必须换 PID）。
  - `NetChaos`：应用层 TCP proxy（`net.Listen` 127.0.0.1 转发到目标，可 `Pause`/`Resume`/`Delay(d)`）——127.0.0.1 回环绑定铁律；proxy 透明于应用（TLS 层断开复现网络分区）。
  - 场景：
    1. `Kill9Restart`：上传中 kill -9 → 重启 → 检查 checksum 台账/分块会话（in-flight 临时文件清理语义不变）。
    2. `PartitionReconnect`：hub 节点间 TCP 断连（proxy Pause 3s）→ 恢复 → 断言 mux 重连（心跳超时 90s 内）+ 指标（retransmits/errors 增长，`/metrics` 断言）。
    3. `LatencyInjection`：proxy 注入 200ms 延迟 → 下载仍完成（读超时 30s 内）+ 断言无错误注入面回归。
  - `ChaosTester` 辅助：`t.Cleanup` 全清理；端口全部 `127.0.0.1:0`。
- 测试枚举挂 `make test-chaos`（独立 target，不并入默认 `make test`，控制 CI 时长；但 CI 必检项可选挂一个 smoke 场景）。

## 数据流
测试 → `go build` 真实二进制（复用现有构建辅助）→ `ChaosNode.Start`（临时 config，`--config` 隔离用户本机配置）→ 注入故障（kill/proxy pause/delay）→ 观测恢复（轮询 `/healthz` + 业务断言）→ `t.Cleanup` 收敛。

## 错误处理
- 子进程异常退出：测试直接失败（fail-closed，不吞）；恢复轮询设超时（默认 15s，`-race` 下 3 倍=45s——遵循既有测试规范）；proxy 端口冲突 → `:0` 动态端口规避；tc/iptables 不存在 → **不跳过也不依赖**（设计上纯应用层实现，无外部工具依赖）。

## 测试+变异点
- 三个场景各有独立用例；断言点：PID 变化、checksum 台账一致、`/metrics` 计数增长、业务请求在恢复后成功。
- 变异验证：① `Kill9Restart` 断言 PID 变化——把重启逻辑改成复用进程 → 红；② `PartitionReconnect` 断言重连——把 proxy Resume 后不恢复转发 → 红；③ `LatencyInjection` 断言成功——把 proxy 永久挂起 → 红（超时路径）。

## 片划分
- 片1：`ChaosNode` + `NetChaos` 框架 + `Kill9Restart`（纯 test/ + Makefile target）。
- 片2：`PartitionReconnect` + `LatencyInjection` + CI job（test-chaos 挂 ci.yml 独立 job，超时上限设 15m）。

## 风险与零回归
- 全部新包、新 target，不动既有 `test/e2e_test.go`；`make test` 不包含 chaos（时长可控）；127.0.0.1 绑定铁律、`--config` 隔离、`-race` 超时 3 倍约定全部沿用；CI job 超时上限防 Benchmark 式 I/O 塌陷（沿用 `gh run rerun --failed` 处置）。
