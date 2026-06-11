# sproxy 传输层发展路线图

> **版本：** v1
> **日期：** 2026-06-11
> **状态：** 设计稿

## 1. 概述

本路线图规划 sproxy 传输层的后续发展，基于已完成的分层传输架构（xfer → mux → tunnel →
hub），按优先级分 5 个阶段推进。

### 优先级顺序

| 阶段 | 主题 | 目标 |
|------|------|------|
| **1** | 中继网络实战化 | 节点保活/重连、鉴权、管理 API、运维工具 |
| **2** | 性能与可靠性 | mux 流控（背压）、重传、大文件优化、Benchmark |
| **3** | P2P 与去中心化 | WebRTC 传输、DHT 节点发现、无 Hub 直连 |
| **4** | 更多传输层实现 | QUIC 传输子模块、TCP 直连 |
| **5** | 运维与可观测性 | Prometheus 集成、OpenTelemetry Tracing、诊断命令 |

---

## 2. 阶段 1：中继网络实战化

### 2.1 节点保活与自动重连

**现状：** Node 连接 Hub 后，如果 WebSocket 断开（网络故障 / Hub 重启），
`sclient relay` 进程退出，需要人工介入重启。

**设计：**

```
Node                               Hub
 │   WebSocket connect              │
 ├────── Register{ID, Token} ──────→│
 │←──── Registered{OK} ─────────────┤
 │   ... 双向通信 ...               │
 │   Ping/Pong (30s)               │
 │   (连接断开)                     │
 │                                  │
 │   ── 自动重连 ──                 │
 │   exponential backoff:           │
 │   1s → 2s → 4s → ... → 30s max  │
 │   WebSocket reconnect            │
 ├────── Register{ID, Token} ──────→│
 │←──── Registered{OK} ─────────────┤
 │   继续中继服务                    │
```

**实现要点：**

```go
// cmd/sclient/relay.go — 重连循环

const (
    reconnectBaseDelay = 1 * time.Second
    reconnectMaxDelay  = 30 * time.Second
)

func runRelayWithRetry(ctx context.Context, logger *slog.Logger) error {
    delay := reconnectBaseDelay
    for {
        err := runRelayOnce(ctx, logger)
        if err == nil || ctx.Err() != nil {
            return err // 正常退出或 ctx 取消
        }
        logger.Warn("中继连接断开，即将重连", "delay", delay, "error", err)
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

- NodeID 持久化：重连时使用相同 NodeID，Hub 处理重复注册（新连接替换旧连接）
- 重连期间排队的请求直接返回 503 Service Unavailable

### 2.2 节点鉴权

**现状：** 任何知道 Hub WebSocket 地址的客户端都可以注册为中继节点，无身份验证。

**设计：**

```go
// pkg/tunnel/hub/auth.go

type RelayToken string

// Authenticator 验证中继节点的注册请求。
type Authenticator interface {
    // Authenticate 验证 token，返回 NodeID（允许动态分配）和错误。
    Authenticate(ctx context.Context, token RelayToken) (NodeID, error)
}

// SharedSecretAuth 使用预共享密钥验证（模式 1）。
type SharedSecretAuth struct {
    key []byte // AES-256 密钥
}

// TokenAuth 使用独立 relay_token 验证（模式 2）。
type TokenAuth struct {
    tokens map[NodeID]RelayToken // 预配置的节点 token 白名单
}
```

- 默认模式：Hub 配置 `relay_token`，Node 连接时在 Register 帧中携带
- 高级模式：支持 mTLS 客户端证书验证（在 WebSocket TLS 层完成）

Hub 配置新增：

```yaml
hub:
  enabled: true
  node_id: "sproxy-hub"
  relay_token: ""        # 共享中继 token，空 = 不验证
  relay_tokens:          # 逐节点 token 白名单
    node-a: "token-a-xxx"
    node-b: "token-b-xxx"
  transports:
    ws:
      enabled: true
      listen: ":18084"
      path: "/ws"
```

### 2.3 管理 API

**现状：** 无法查看 Hub 上有多少在线节点、它们的健康状态、中继统计信息。

**设计：** Hub 新增管理路由（受 authMiddleware 保护）：

| 路由 | 方法 | 说明 |
|------|------|------|
| `/api/hub/nodes` | GET | 列出所有在线节点（ID、地址、注册时间、健康状态） |
| `/api/hub/nodes/{id}` | DELETE | 踢出指定节点 |
| `/api/hub/stats` | GET | 中继统计（转发请求数、字节数、错误数） |

**NodeInfo 扩展：**

```go
type NodeInfo struct {
    ID        NodeID
    Mux       *mux.Mux
    Addr      string        // 远端地址
    Token     string        // 使用的 token（脱敏）
    Connected time.Time     // 连接时间
    LastPing  time.Time     // 最后心跳时间
    Stats     NodeStats     // 节点统计
}

type NodeStats struct {
    RequestsForwarded atomic.Int64
    BytesForwarded    atomic.Int64
    Errors            atomic.Int64
}
```

### 2.4 阶段 1 交付标准

- [ ] sclient relay 出现网络断开后自动重连，恢复中继服务
- [ ] 重连期间指数退避，不超过 30 秒
- [ ] Hub 拒绝未携带有效 token 的注册请求
- [ ] `GET /api/hub/nodes` 返回在线节点列表
- [ ] `DELETE /api/hub/nodes/{id}` 踢出节点
- [ ] `GET /api/hub/stats` 返回中继统计
- [ ] 所有新增代码通过 `go vet` + `go test -race`

---

## 3. 阶段 2：性能与可靠性

### 3.1 mux 流控（背压）

**现状：** Stream.Write 无限制地向 writeCh 发送数据，writeCh 满时阻塞。如果生产
者快于消费者，内存会无限增长（writeCh 缓冲区 256 + dataCh 缓冲区 64）。

**设计：** 引入窗口机制：

```go
// mux/flow.go

const (
    DefaultWindowSize = 65536 // 64 KB 初始窗口
    MaxWindowSize     = 1 << 20 // 1 MB 最大窗口
)

// WindowUpdate 帧告知远端可以发送更多数据。
type WindowUpdate struct {
    StreamID StreamID
    Increment uint32 // 增加的窗口字节数
}
```

- 每条流初始窗口 64 KB
- 接收方消费数据后发送 `WindowUpdate` 帧
- 发送方窗口耗尽时停止发送，等待 `WindowUpdate`
- 避免 mux 内部缓冲区无限增长

### 3.2 重传机制

**现状：** `xfer.Conn.Send` 失败时，mux 直接关闭整个连接。不区分"连接断开"和"临时
网络抖动"。

**设计：** mux 层的可靠传输（可选启用）：

- sendFrame 失败后，将待发送帧放入重传队列
- 重传队列使用指数退避（100ms → 200ms → 400ms → 3s max）
- 超过最大重试次数（默认 5 次）后关闭连接
- 重传队列容量上限（默认 1024 帧），超限时关闭流而非整个连接

### 3.3 大文件流式传输优化

**现状：** Tunnel.Do 将响应体通过 Pipe 从 stream 传输，每次从 dataCh 读取最大
64KB。完全没有并行。

**优化方向：**
- mux 层增加并发流读写（多个 dataCh 并行读取）
- 大文件分块通过多条流并行传输（在应用层控制）
- Tunnel.Do 的 streamBody 预读缓冲

### 3.4 基准测试

新增 Benchmark 套件：

```go
func BenchmarkMuxThroughput(b *testing.B)
func BenchmarkMuxConcurrentStreams(b *testing.B)
func BenchmarkTunnelRoundTrip(b *testing.B)
func BenchmarkXferPipe(b *testing.B)
```

测试范围：
- 小消息（64 bytes）和 大消息（1 MB）的吞吐量
- 1/10/50/100 条并发流的延迟分布
- 加密 vs 不加密的吞吐差异
- WebSocket 传输的基准（端到端，真实网络）

---

## 4. 阶段 3：P2P 与去中心化

### 4.1 WebRTC 传输（`xfer/webrtc/`）

**目标：** 浏览器和 Node 之间直接 P2P 传输，不经过 Hub 中转。

**架构：**

```
 browser / sclient              sproxy (Signaling)          peer
    │                                │                      │
    │  WebSocket (信令通道)            │                      │
    ├───────── Offer SDP ────────────→│                      │
    │                                ├──── Forward Offer ───→│
    │←─────── Answer SDP ────────────│←─── Answer SDP ──────┤
    │                                │                      │
    │══════ WebRTC DataChannel ══════│═══════════════════════│
    │        (P2P，不经过 Hub)        │                      │
```

**实现要点：**
- 复用现有信令 WebSocket 连接（不新增端口）
- `xfer.Conn` 基于 WebRTC DataChannel（SCTP，可靠有序）
- NAT 穿透依赖 STUN/TURN（内建 STUN，TURN 可选配置）
- 浏览器端可以直接连接，无需 sclient

### 4.2 DHT 节点发现

**目标：** 节点无需预配置 Hub 地址，通过 DHT 网络发现 peers。

**设计：**
- 基于 Kademlia 风格的分布式哈希表
- 节点 ID = SHA-256(公钥) 的前 20 字节
- 节点信息存储在 DHT 中：`{nodeID → [addrs]}`（地址列表）
- 新节点加入时，通过已知引导节点（bootstrap node）加入 DHT 网络

### 4.3 无 Hub 直连模式

**目标：** 两个 sclient 实例之间直接建立隧道，不经过任何中心节点。

**流程：**
1. Node A 通过 DHT 查询 Node B 的地址
2. 尝试 NAT 穿透（ICE/STUN）
3. 建立 P2P xfer.Conn 连接
4. 在连接上创建 mux + tunnel，直接交换 HTTP 请求

---

## 5. 阶段 4：更多传输层实现

### 5.1 QUIC 传输（`xfer/quic/`）

**依赖：** `github.com/quic-go/quic-go`

**优势：**
- 基于 UDP，0-RTT 握手，比 TCP 快
- 内置多路复用（无需 mux 层？——仍需要 mux 层做流 ID 管理）
- 内置 TLS 1.3 加密
- 更好的弱网性能

```go
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
    tlsConfig := &tls.Config{MinVersion: tls.VersionTLS13} // 使用系统 CA 信任链
    conn, err := quic.DialAddr(ctx, addr, tlsConfig, nil)
    if err != nil { return nil, err }
    stream, err := conn.OpenStreamSync(ctx)
    if err != nil { return nil, err }
    return &quicConn{stream: stream}, nil
}
```

**独立 go.mod：** `module github.com/cocomhub/sproxy/xfer/quic`

### 5.2 TCP 直连传输（`xfer/tcp/`）

**目标：** 在 LAN 或可信网络环境中，提供比 WebSocket 更低延迟的传输。

**实现：** TCP 连接 + 简单的长度前缀帧定界

```go
// tcpConn 将 net.Conn 包装为 xfer.Conn
// 帧格式：[4B big-endian length][payload]
type tcpConn struct {
    conn net.Conn
    rBuf []byte
}
```

**适用场景：**
- 同一数据中心内的 sproxy 集群
- Kubernetes 集群内节点通信
- 开发环境本地调试（无需 WebSocket 升级）

---

## 6. 阶段 5：运维与可观测性

### 6.1 Prometheus 指标

**现状：** `GET /metrics` 已输出基本 sproxy + mux 指标，但非标准 Prometheus 格式
（没有 `# TYPE` 注释会有兼容性问题）。

**改进：**
- 确保所有指标包含 `# TYPE` 和 `# HELP` 注释
- 增加 hub 层指标：`sproxy_hub_nodes_connected`、`sproxy_hub_relays_total`
- 增加 xfer 层指标：`sproxy_xfer_conns_active`（按传输类型区分）
- 支持 `promhttp.Handler()` 集成（标准 Prometheus 客户端库）

### 6.2 OpenTelemetry Tracing

**目标：** 追踪一次中继请求的完整链路：客户端 → Hub → 目标 Node → 本地 HTTP 服务。

```go
// 在关键路径上注入 span：
// 1. RelayHandler.ServeHTTP — 接收到中继请求
// 2. RouteTable.Lookup — 查找目标节点
// 3. Tunnel.Do — 通过 mux 发送请求
// 4. Node 端 HTTP handler — 转发到本地服务
```

- 使用 `go.opentelemetry.io/otel` 标准 API
- Trace ID 通过 HTTP Header（`Traceparent`）传播
- 默认不启用，通过配置 `observability.tracing.enabled: true` 开启

### 6.3 诊断命令

**现状：** `sclient list/stat/tunnel` 只能操作远程文件，不能诊断连接健康状态。

**新增命令：**

```bash
# 诊断 Hub 连接状态
sclient relay --diagnose

# 输出示例：
# Node: my-node
# Hub: ws://hub:18084/ws
# Status: connected (uptime: 2h31m)
# Latency: 5ms (last ping-pong)
# Streams: 3 active / 128 total
# Errors: 0

# 传输层 ping
sclient ping --transport ws --hub ws://hub:18084/ws

# Hub 状态查询
sclient relay --hub-status
# 输出 Hub 上的节点列表和统计
```

---

## 7. 阶段优先级与依赖关系

```
阶段 1 ──────────────────────────────────────────
  ├─ 节点保活与重连
  ├─ 节点鉴权 (relay_token / mTLS)
  └─ 管理 API (/api/hub/nodes, /api/hub/stats)
        │
        ▼
阶段 2 ──────────────────────────────────────────
  ├─ mux 流控（窗口机制）
  ├─ 重传机制
  ├─ 大文件流式传输优化
  └─ Benchmark 套件
        │
        ▼
阶段 3 ──────────────────────────────────────────
  ├─ WebRTC 传输 (xfer/webrtc/)
  ├─ DHT 节点发现
  └─ 无 Hub 直连
        │
        ▼
阶段 4 ──────────────────────────────────────────
  ├─ QUIC 传输 (xfer/quic/)
  └─ TCP 直连 (xfer/tcp/)
        │
        ▼
阶段 5 ──────────────────────────────────────────
  ├─ Prometheus 指标标准化
  ├─ OpenTelemetry Tracing
  └─ 诊断命令
```

**依赖规则：**
- 阶段 2 依赖阶段 1（中继实战化后才能做性能优化）
- 阶段 3 依赖阶段 1 + 4（P2P 需要先有非 WS 传输层）
- 阶段 4 无硬依赖（可作为独立子模块开发）
- 阶段 5 与所有阶段正交，可并行推进

---

## 8. 安全考量

| 阶段 | 安全特性 | 说明 |
|------|----------|------|
| 1 | `relay_token` 鉴权 | 防止未授权节点注册到 Hub |
| 1 | mTLS 支持 | 传输层双向证书验证 |
| 2 | 流控防滥用 | 防止恶意客户端耗尽 Hub 内存 |
| 3 | WebRTC DTLS + SRTP | P2P 链路端到端加密 |
| 3 | 节点身份签名 | DHT 中节点信息使用私钥签名防篡改 |
| 4 | QUIC TLS 1.3 | 强制加密 |
| 5 | 指标脱敏 | `/metrics` 不暴露 token 等敏感信息 |
