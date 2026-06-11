<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 传输层架构

sproxy v2 引入了全新的分层传输架构，在原有的文件服务与加密隧道之上，增加了可插拔传输层
抽象、多路复用能力和中继网络支持。

## 分层架构

```
┌──────────────────────────────────────────┐
│           应用层 (Application)             │
│  sproxy HTTP 路由 + sclient CLI           │
│  FileClient Go SDK                        │
├──────────────────────────────────────────┤
│  hub 层 — 节点注册 / 路由表 / 中继转发      │
│  RouteTable / RelayHandler                │
├──────────────────────────────────────────┤
│  tunnel 层 — HTTP 请求-响应交换             │
│  Tunnel.Do(req) → *http.Response           │
│  Tunnel.Serve(ctx, handler)               │
│  复用现有 Request/Response + AES-256-GCM   │
├──────────────────────────────────────────┤
│  mux 层 — 虚拟流多路复用                    │
│  Mux{Open/Accept/Close}                  │
│  Stream{io.ReadWriteCloser}              │
│  控制流: Ping/Pong + 节点注册              │
├──────────────────────────────────────────┤
│  xfer 层 — 传输层抽象 (Transport Abstr.)    │
│  Conn{Send/Receive/Close}                │
│  Transport 注册表 — 按名字查找传输实现      │
├──────────┬──────────┬──────────┬──────────┤
│ xferhttp │ xfer/ws  │ xfer/grpc│ xfer/quic│
│ (内置)    │ (子模块)  │ (未来)    │ (未来)    │
└──────────┴──────────┴──────────┴──────────┘
```

### xfer 层（`pkg/tunnel/xfer`）

传输层抽象，定义最小消息式连接接口。任何传输协议（WebSocket、gRPC 双向流、QUIC 流
、HTTP POST 包装）只需实现 3 个方法即可接入上层多路复用系统。

**核心接口：**

```go
// Conn 是双向保序消息连接
type Conn interface {
    Send(ctx context.Context, msg []byte) error
    Receive(ctx context.Context) ([]byte, error)
    io.Closer
}

// Transport 是注册单元
type Transport struct {
    Name   string
    Dial   func(ctx context.Context, addr string) (Conn, error)
    Listen func(ctx context.Context, addr string) (Listener, error)
}
```

**内置实现：** `xferhttp` —— 将 HTTP POST 请求-响应包装为 `Conn`（兼容已有 tunnel 模式）。

**扩展方式：** 第三方传输层通过 `init()` 注册到全局注册表：

```go
func init() {
    xfer.Register(&xfer.Transport{
        Name: "ws",
        Dial: wsDial,
        Listen: wsListen,
    })
}
```

### mux 层（`pkg/tunnel/mux`）

在单条 `xfer.Conn` 上多路复用多条虚拟流。

**流（Stream）：** 实现 `io.ReadWriteCloser`，可与 `http.Request.Body` 和
`http.ResponseWriter` 直接桥接。每条流的读写都是独立的，不会相互阻塞。

**帧协议：**

```
[4B StreamID][1B FrameType][1B Flags][2B PayloadLength][Payload...]
```

**帧类型：**

| 帧类型 | 用途 |
|--------|------|
| `FrameData` | 用户流数据 |
| `FrameOpen` | 通知远端打开新流 |
| `FrameClose` | 关闭指定流 |
| `FrameCloseWrite` | 写半关闭（不再有更多数据发送，但仍可读取） |
| `FramePing` | 心跳探测（30s 间隔） |
| `FramePong` | 心跳回复 |

**心跳机制：** 30s 发送 Ping，90s 内未收到 Pong 则判定断开，自动清理。

**指标收集：** mux 内置 `Metrics` 结构体，记录流数、帧数、字节数、Ping/Pong 和错误
计数，可通过 `GET /metrics` 查看。

### tunnel 层（`pkg/tunnel`）

在 mux 之上构建 HTTP 请求-响应语义。提供两种隧道模式：

**传统模式：**
- `NewHandler(key)` / `NewLocalHandler(key, localMux)` → 标准 `http.Handler`
- `Client.Do(req)` → 每个请求创建一个 HTTP POST，适合短连接场景

**多路复用模式（推荐）：**
- `NewTunnel(mux, key)` → 在已有 mux 连接上创建隧道
- `Tunnel.Do(req)` → 在 mux 上分配一条新流，通过流完成 HTTP 请求-响应交换
- `Tunnel.Serve(ctx, handler)` → 接受流并路由到本地 handler

### hub 层（`pkg/tunnel/hub`）

星型中继网络的 Hub 端实现。

- **RouteTable：** 线程安全的节点路由表（`NodeID → *mux.Mux`）
- **节点注册：** 节点通过控制流发送 `Register` 帧向 Hub 注册
- **中继转发：** `POST /api/relay` 接收 JSON，查找目标节点，通过其 mux 转发

## 数据流示例：中继请求

```
sclient                    sproxy (Hub)                   Node B
  │                           │                            │
  │ WebSocket Connect          │                            │
  ├──── Register{ID:"node-a"}→│                            │
  │                           │ 注册到 RouteTable           │
  │                           │                            │
  │ POST /api/relay            │                            │
  │ {target:"node-b",          │                            │
  │  method:"GET",             │                            │
  │  path:"/api/files"}        │                            │
  ├───────────────────────────→│                            │
  │                           │ RouteTable.Lookup("node-b") │
  │                           │ targetMux.Open() → stream   │
  │                           ├───── HTTP GET /api/files ──→│
  │                           │                            │ 本地 HTTP 处理
  │                           │←──── HTTP 200 + body ──────┤
  │←──────── JSON 200 ────────┤                            │
```

## 相关包路径

| 层 | 包路径 | 说明 |
|----|--------|------|
| xfer | `pkg/tunnel/xfer/` | 传输层抽象接口 + 注册表 |
| xferhttp | `pkg/tunnel/xfer/http.go` | HTTP POST 内置传输实现 |
| xferws | `xfer/ws/` | WebSocket 传输子模块（独立 go.mod） |
| mux | `pkg/tunnel/mux/` | 虚拟流多路复用器 |
| tunnel | `pkg/tunnel/tunnel_mux.go` | 多路复用隧道（Tunnel 类型） |
| hub | `pkg/tunnel/hub/` | 中继路由表 + 注册框架 |
| relay | `cmd/sclient/relay.go` | sclient 中继节点命令 |
