# sproxy 传输层后续发展路线图

> **版本：** v2
> **日期：** 2026-06-11
> **状态：** 设计稿
> **基于：** 已完成的分层传输架构（xfer → mux → tunnel → hub）所有基础能力已交付。

## ✅ 已完成（不再规划）

| 能力 | 说明 |
|------|------|
| xfer 传输抽象 | Conn/Listener/Transport 接口 + 注册表 + HTTP 内置实现 |
| mux 多路复用 | 帧协议 + Open/Accept/Close + Stream(io.RWC) + 心跳 + Metrics |
| tunnel 隧道 | Tunnel.Do/Serve HTTP 交换 + AES-256-GCM 加密 + bufferedResponseWriter |
| hub 路由表 | RouteTable(Add/Remove/Lookup/List) 线程安全 + 100% 测试覆盖 |
| WebSocket 传输 | `xfer/ws` 独立 go.mod，init() 注册 |
| relay 命令 | `sclient relay` 完整实现（Tunnel.Serve + HTTP 转发） |
| FileClient 集成 | `WithXfer` 选项 + 重连检测 |
| mux 指标 | Streams/Frames/Pings/Bytes 计数，`GET /metrics` 输出 |
| 竞争修复 | FrameData/CloseWrite 锁内发送、atomic lastPong、Context 缓存 |
| 文档 | architecture.md、cli.md(relay)、config.md(hub)、README |
| 测试 | mux 边界用例、relay e2e、client WithXfer e2e |

---

## 2. 阶段 1：中继网络实战化（部分完成，需补全）

### 2.1 节点自动重连 🔴 未实现

**现状：** `sclient relay` 在网络断开后直接退出，无自动恢复能力。

**实现：** 在 `cmd/sclient/relay.go` 中，将现有 `runRelay` 包装为重连循环：

```go
const (
    reconnectBaseDelay = 1 * time.Second
    reconnectMaxDelay  = 30 * time.Second
)

func runRelayWithRetry(ctx context.Context, logger *slog.Logger) error {
    delay := reconnectBaseDelay
    for {
        err := runRelayOnce(ctx, logger)
        if err == nil || ctx.Err() != nil {
            return err  // 正常退出
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

- `runRelayOnce` = 当前的 `runRelay` 逻辑（提取出来）
- Hub 端需处理 NodeID 重复注册（新连接替换旧连接）

### 2.2 中继鉴权 🔴 未实现

**现状：** 任何知道 Hub WebSocket 地址的客户端都可以注册为中继节点。

**实现：**

```yaml
# sproxy.yaml 新增配置
hub:
  enabled: true
  relay_token: "shared-secret"    # 共享 token，空 = 不鉴权
```

```go
// pkg/tunnel/hub/auth.go
type Authenticator struct {
    relayToken string  // 共享密钥模式
    key        []byte  // AES-256 模式（与 tunnel_key 共用）
}

func (a *Authenticator) Authenticate(token string) error {
    if a.relayToken != "" && token != a.relayToken {
        return fmt.Errorf("invalid relay token")
    }
    return nil
}
```

- Register 帧在 NodeID 后附加 token
- Hub 的控制流处理中验证 token
- 支持未来扩展 mTLS（在 WebSocket TLS 层完成）

### 2.3 Hub 管理 API 🟡 未实现

**现状：** 无法查看 Hub 在线节点、无管理接口。

**新增路由（受 authMiddleware 保护）：**

| 路由 | 方法 | 说明 |
|------|------|------|
| `GET /api/hub/nodes` | GET | 在线节点列表（ID + 连接时间 + 地址） |
| `DELETE /api/hub/nodes/{id}` | DELETE | 踢出指定节点 |
| `GET /api/hub/stats` | GET | 中继统计（转发请求数 / 字节 / 错误） |

**RouteTable 增强：**

```go
// NodeInfo 扩展字段（已有基础结构）
type NodeInfo struct {
    ID        NodeID
    Mux       *mux.Mux
    Addr      string        // 现有
    Connected time.Time     // 新增：连接时间
    Token     string        // 新增：使用的 token（脱敏）
}
```

### 2.4 阶段 1 交付清单

- [ ] sclient relay 断开后自动重连（指数退避 1s → 30s max）
- [ ] Hub 处理重复 NodeID 注册（新连接替换旧连接）
- [ ] Hub 拒绝未携带有效 token 的注册请求
- [ ] `GET /api/hub/nodes` 返回在线节点
- [ ] `DELETE /api/hub/nodes/{id}` 踢出节点
- [ ] `GET /api/hub/stats` 返回中继统计
- [ ] 全部通过 `go vet` + `go test -race`

---

## 3. 阶段 2：性能与可靠性

### 3.1 mux 流控（背压）🔴 未实现

**问题：** Stream.Write 不受限，无背压反馈。当生产者快于消费者时内存增长。

**实现：** 引入窗口机制（`FrameWindowUpdate` 帧类型）：

```go
const DefaultWindowSize = 65536  // 64 KB

// 新增帧类型
const FrameWindowUpdate FrameType = 0x07
```

- 每条流初始窗口 64 KB
- 接收方消费 dataCh 数据后自动发送 `WindowUpdate`
- 发送方窗口耗尽时暂停，等待 `WindowUpdate` 到达

### 3.2 重传机制 🟡 可优化

**现状：** `xfer.Conn.Send` 失败时 mux 直接关闭连接，不区分临时抖动和永久断开。

**实现：** writeLoop 中失败帧进入重传队列（指数退避 100ms→3s，最多 5 次）。

### 3.3 大文件优化 🟡 可优化

**现状：** Tunnel.Do 通过 Pipe 串行读写 dataCh，无并行。

**优化：** streamBody 预读缓冲 + 可选并发流（应用层控制）。

### 3.4 Benchmark 基准 🟡 未实现

```go
func BenchmarkMuxThroughput(b *testing.B)
func BenchmarkMuxConcurrentStreams(b *testing.B)
func BenchmarkTunnelRoundTrip(b *testing.B)
```

- 小消息（64 B）/ 大消息（1 MB）吞吐
- 1/10/50/100 并发流延迟分布
- 加密 vs 不加密差异

---

## 4. 阶段 3：P2P 与去中心化

### 4.1 WebRTC 传输（`xfer/webrtc/`）🔴 未实现

**目标：** 浏览器和节点之间直接 P2P 传输，不经过 Hub 中转。

**依赖：** `github.com/pion/webrtc/v4`

**实现要点：**
- 复用现有 WebSocket 信令通道
- `xfer.Conn` 基于 WebRTC DataChannel（SCTP 可靠有序）
- NAT 穿透：内置 STUN，TURN 可选
- 独立 go.mod：`xfer/webrtc/`

### 4.2 DHT 节点发现 🔴 未实现

**目标：** 节点无需预配置 Hub 地址，通过 DHT 发现 peers。

**依赖：** `github.com/libp2p/go-libp2p-kad-dht` 或基于 Kademlia 的自实现。

### 4.3 无 Hub 直连 🔴 未实现

**流程：** DHT 查询地址 → NAT 穿透(ICE/STUN) → P2P xfer.Conn → mux + tunnel

---

## 5. 阶段 4：更多传输层实现

### 5.1 QUIC 传输（`xfer/quic/`）🔴 未实现

**依赖：** `github.com/quic-go/quic-go` | **优势：** 0-RTT、内置 TLS 1.3、抗丢包

### 5.2 TCP 直连（`xfer/tcp/`）🔴 未实现

**适用：** 数据中心 / K8s 集群内部，比 WebSocket 更低延迟。

**帧定界：** `[4B big-endian length][payload]`

---

## 6. 阶段 5：运维与可观测性

### 6.1 Prometheus 标准化 🟡 部分完成

**现状：** `GET /metrics` 有基本指标，但缺少 hub 层指标。

**补充：** `sproxy_hub_nodes_connected`、`sproxy_hub_relays_total`

### 6.2 OpenTelemetry Tracing 🔴 未实现

**依赖：** `go.opentelemetry.io/otel`

**链路：** 客户端 → Hub → 目标 Node → 本地 HTTP 服务

### 6.3 诊断命令 🔴 未实现

```bash
sclient relay --ping         # 检测 Hub 连通性和延迟
sclient relay --hub-status   # 查看 Hub 节点列表
```

---

## 7. 依赖关系

```
阶段 1 ───────────────── 最优先，补全中继实战能力
  ├─ 节点自动重连 (独立)
  ├─ 中继鉴权 (独立)
  └─ Hub 管理 API (独立)

阶段 2 ───────────────── 性能提升
  ├─ 流控背压 (依赖阶段 1)
  ├─ 重传 (独立)
  ├─ 大文件优化 (独立)
  └─ Benchmark (独立)

阶段 3 + 4 ───────────── 新功能子模块，与阶段 1/2 正交
  ├─ WebRTC / QUIC / TCP (独立 go.mod)
  ├─ DHT 发现 (依赖 WebRTC)
  └─ 无 Hub 直连 (依赖 DHT)

阶段 5 ───────────────── 运维增强，与所有阶段正交
  ├─ Prometheus (独立)
  ├─ Tracing (独立)
  └─ 诊断命令 (独立)
```

**建议的下一个实现阶段：** 阶段 1（中继实战化）的三项可以**并行推进**，每项都是独立的增量改进。是否要从阶段 1 开始实施？
