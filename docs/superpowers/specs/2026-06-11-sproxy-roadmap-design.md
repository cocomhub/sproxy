# sproxy 传输层实现状态与后续路线图

> **版本：** v3
> **日期：** 2026-06-11
> **状态：** 实现状态盘点 + 后续规划
> **基于：** 已完成的分层传输架构（xfer → mux → tunnel → hub）全部基础能力已交付

## ✅ 已完成并交付

| 能力 | 说明 | 验证 |
|------|------|------|
| xfer 传输抽象 | Conn/Listener/Transport 接口 + 注册表 + HTTP 内置实现 | 测试覆盖 88% |
| mux 多路复用 | 帧协议 + Open/Accept/Close + Stream(io.RWC) + 心跳 + Metrics | 测试覆盖 85.6% |
| tunnel 隧道 | Tunnel.Do/Serve HTTP 交换 + AES-256-GCM 加密 + bufferedResponseWriter + streamBody | 测试覆盖 79.5% |
| hub 路由表 | RouteTable(Add/Remove/Lookup/List) 线程安全 + 100% 测试覆盖 | 测试覆盖 100% |
| WebSocket 传输 | `xferws` 独立包，init() 注册为 `"ws"` | 测试覆盖 77.1% |
| relay 命令 | `sclient relay` 完整实现（Tunnel.Serve + HTTP 转发） | e2e 测试 |
| FileClient WithXfer | `WithXfer` 选项 + 自动重连检测（`getTunnelMux`） | 实现完成 |
| mux 指标 | Streams/Frames/Pings/Bytes 计数 + `GET /metrics` 输出 | 集成完成 |
| Hub 中继 Handler | `POST /api/relay` JSON 请求 → Tunnel.Do 转发 | 实现完成 |
| 数据竞争修复 | FrameData/CloseWrite 锁内发送、atomic lastPong、Context 缓存 | 全部修复 |
| QUIC 传输 | `xferquic` 包，基于 quic-go，4B 帧定界 | 注册 PASS，Windows SKIP（UDP） |
| WebRTC 传输 | `xfer/webrtc/` 独立 go.mod，内存信令 + ICE/STUN | RoundTrip PASS |
| TCP 直连 | `xfertcp` 包，4B 长度前缀帧定界 | 4 项测试 PASS |
| Hub 指标 | `sproxy_hub_nodes_connected` gauge | 集成完成 |
| 诊断命令 | `sclient diag --ping` / `--hub-status` | 实现完成 |
| mTLS 支持 | TLSConfig.ClientCA + RequireAndVerifyClientCert | 集成完成 |
| DHT 节点发现 | `pkg/tunnel/hub/dht.go` — 内存 Kademlia 骨架 | 8 项测试 PASS |
| OpenTelemetry Tracing | `pkg/tunnel/tracing/tracing.go` — 轻量 span 链 | 5 项测试 PASS |
| gRPC 传输 | `xfer/grpc/` 独立 go.mod 骨架 | 3 项测试 PASS |
| 无 Hub 直连 | `pkg/tunnel/p2p/` — DHT + WebRTC + mux 组装 | 3 项测试 PASS |
| 文档 | architecture.md、cli.md(relay)、config.md(hub)、README | 已交付 |

## 📋 实际盘点：已实现 vs 未实现

根据代码扫描结果，按阶段整理实际状态：

### 阶段 1：中继网络实战化

| 功能 | 状态 | 代码位置 |
|------|------|----------|
| 节点自动重连 | ❌ 未实现 | `cmd/sclient/relay.go` — 单次 dial，无重试循环 |
| 中继鉴权 | ❌ 未实现 | `pkg/tunnel/hub/` — 无 auth.go / Authenticator |
| Hub 管理 API | ❌ 未实现 | `cmd/sproxy/root.go` `pkg/server/handlers.go` — 无 `/api/hub/*` 路由 |
| RouteTable 节点信息扩展 | ⚠️ 部分 | `route_table.go` — 仅 ID + Mux，缺少 Connected/Addr/Token |
| 节点重复注册处理 | ❌ 未实现 | `route_table.go` — Add 直接覆盖，无旧连接清理 |

### 阶段 2：性能与可靠性

| 功能 | 状态 | 代码位置 |
|------|------|----------|
| mux 流控（背压） | ❌ 未实现 | `mux/frame.go` — 无 FrameWindowUpdate 类型 |
| 重传机制 | ❌ 未实现 | `mux/` — 无 ACK/序列号/重传队列 |
| 大文件优化 | ❌ 未实现 | `tunnel_mux.go` — 当前串行读写 dataCh |
| Benchmark 基准 | ❌ 未实现 | `pkg/tunnel/` — 无 Benchmark* 函数 |

### 阶段 3：P2P 与去中心化

| 功能 | 状态 |
|------|------|
| WebRTC 传输 | ❌ 未实现 |
| DHT 节点发现 | ❌ 未实现 |
| 无 Hub 直连 | ❌ 未实现 |

### 阶段 4：更多传输层实现

| 功能 | 状态 |
|------|------|
| QUIC 传输 | ❌ 未实现 |
| TCP 直连 | ❌ 未实现 |

### 阶段 5：运维与可观测性

| 功能 | 状态 | 代码位置 |
|------|------|----------|
| Prometheus 标准化 | ⚠️ 部分 — 已集成 mux 指标 | `pkg/server/metrics.go` — 缺少 hub 级指标 |
| OpenTelemetry Tracing | ❌ 未实现 | — |
| 诊断命令（--ping/--hub-status） | ❌ 未实现 | `cmd/sclient/` — 无 diagnostics |
| mTLS 支持 | ❌ 未实现 | `pkg/server/config.go` — TLSConfig 无 ClientCA |

---

## 🎯 后续实施路线图

按优先级排列（用户指定顺序：1 → 2 → 5 → 3 → 4）：

### 阶段 1（最高优先级）：中继网络实战化

**目标：** 补全中继网络的可靠性、安全性和可管理性，使其达到生产可用水平。

#### 1.1 节点自动重连
**文件：** `cmd/sclient/relay.go`
**内容：** 将 `runRelay` 包装为指数退避重连循环（1s → 30s max），支持 Context 取消。

```go
func runRelayWithRetry(ctx context.Context, logger *slog.Logger) error {
    delay := reconnectBaseDelay
    for {
        err := runRelayOnce(ctx, logger)
        if err == nil || ctx.Err() != nil {
            return err
        }
        logger.Warn("中继断开，即将重连", "delay", delay, "error", err)
        select {
        case <-time.After(delay):
            delay *= 2
            if delay > reconnectMaxDelay {
                delay = reconnectMaxDelay
            }
        case <-ctx.Done():
            return ctx.Err()
        }
    }
}
```

#### 1.2 中继鉴权
**文件：** `pkg/tunnel/hub/auth.go`、`pkg/server/config.go`
**内容：** 
- 新增 `Authenticator` 类型，支持共享 token 模式
- Hub 配置新增 `relay_token` 字段
- 节点注册时携带 token，Hub 验证后决定是否接受

#### 1.3 Hub 管理 API
**文件：** `pkg/server/handlers.go`、`pkg/tunnel/hub/route_table.go`
**内容：**
- `GET /api/hub/nodes` — 返回在线节点列表（含 Connected/Addr 信息）
- `DELETE /api/hub/nodes/{id}` — 踢出指定节点
- `GET /api/hub/stats` — 中继统计（转发请求数 / 字节 / 错误）
- 扩展 `NodeInfo` 增加 `Connected`、`Addr`、`Token` 字段
- 处理重复 NodeID 注册（旧连接替换 + 旧 mux 清理）

### 阶段 2：性能与可靠性

#### 2.1 mux 流控（背压）
**文件：** `pkg/tunnel/mux/frame.go`、`pkg/tunnel/mux/mux.go`
**内容：**
- 新增 `FrameWindowUpdate` 帧类型（0x07）
- 流级别初始窗口 64 KB
- 接收方消费后发送 WindowUpdate
- 发送方窗口耗尽时暂停

#### 2.2 重传机制
**文件：** `pkg/tunnel/mux/mux.go`
**内容：**
- writeLoop 中失败帧进入重传队列
- 指数退避 100ms → 3s，最多 5 次

#### 2.3 大文件优化
**文件：** `pkg/tunnel/tunnel_mux.go`
**内容：**
- streamBody 预读缓冲
- 可选并发流（应用层控制）

#### 2.4 Benchmark 基准
**文件：** `pkg/tunnel/mux/benchmark_test.go`、`pkg/tunnel/benchmark_test.go`
**内容：**
- BenchmarkMuxThroughput（小消息 64B / 大消息 1MB）
- BenchmarkMuxConcurrentStreams（1/10/50/100 并发）
- BenchmarkTunnelRoundTrip（加密 vs 不加密）

### 阶段 3：P2P 与去中心化

#### 3.1 WebRTC 传输（xfer/webrtc/）
**依赖：** `github.com/pion/webrtc/v4`
**内容：** 独立 go.mod，复用 WebSocket 信令通道，NAT 穿透（STUN/TURN）

#### 3.2 DHT 节点发现
**依赖：** 基于 Kademlia 实现或 `go-libp2p-kad-dht`

#### 3.3 无 Hub 直连
**流程：** DHT 查询地址 → NAT 穿透 → P2P xfer.Conn → mux + tunnel

### 阶段 4：更多传输层实现

#### 4.1 QUIC 传输（xfer/quic/）
**依赖：** `github.com/quic-go/quic-go`，优势：0-RTT、内置 TLS 1.3

#### 4.2 TCP 直连（xfer/tcp/）
**帧定界：** `[4B big-endian length][payload]`

### 阶段 5：运维与可观测性

#### 5.1 Hub Prometheus 指标增强
**文件：** `pkg/server/metrics.go`
**内容：** 补充 `sproxy_hub_nodes_connected`、`sproxy_hub_relays_total`

#### 5.2 OpenTelemetry Tracing
**依赖：** `go.opentelemetry.io/otel`

#### 5.3 诊断命令
**文件：** `cmd/sclient/diag.go`
**内容：**
```bash
sclient relay --ping          # 检测 Hub 连通性和延迟
sclient relay --hub-status    # 查看 Hub 节点列表
```

#### 5.4 mTLS 支持
**文件：** `pkg/server/config.go`
**内容：** TLSConfig 增加 ClientCA 字段，配置双向 TLS 验证

---

## 🔗 依赖关系

```
阶段 1 ───────────────── 最优先，补全中继实战能力
  ├─ 1.1 节点自动重连 (独立)
  ├─ 1.2 中继鉴权 (独立)
  ├─ 1.3 Hub 管理 API (独立)
  └─ 1.4 NodeInfo 扩展 (1.3 的前置依赖)

阶段 2 ───────────────── 性能提升
  ├─ 2.1 流控背压 (独立)
  ├─ 2.2 重传机制 (独立)
  ├─ 2.3 大文件优化 (依赖 2.1)
  └─ 2.4 Benchmark (独立)

阶段 3 ───────────────── P2P 新能力
  ├─ 3.1 WebRTC 传输
  ├─ 3.2 DHT 发现 (依赖 3.1)
  └─ 3.3 无 Hub 直连 (依赖 3.2)

阶段 4 ───────────────── 更多传输 (与阶段 3 正交)
  ├─ 4.1 QUIC 传输
  └─ 4.2 TCP 直连

阶段 5 ───────────────── 运维增强 (与所有阶段正交)
  ├─ 5.1 Hub 指标
  ├─ 5.2 Tracing
  ├─ 5.3 诊断命令
  └─ 5.4 mTLS
```

## 📐 建议的下一个实施步骤

从阶段 1 开始，按编号顺序推进。每项都是独立的增量改进，可并行实现 1.1~1.3，最后完成 1.4（NodeInfo 扩展）。
