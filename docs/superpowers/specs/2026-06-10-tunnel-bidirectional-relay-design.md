# Tunnel 传输层抽象与双向中继设计规格

> **版本：** v1
> **日期：** 2026-06-10
> **状态：** 设计稿

## 1. 概述

### 1.1 目标

在 sproxy 现有 `pkg/tunnel` AES-256-GCM 加密隧道基础上，设计传输层抽象接口，实现：

1. **双向事件流（B）** — sproxy 和 sclient 之间建立持久双向连接，两端可随时主动发送数据
2. **星型中继网络（C）** — 一个 Hub（sproxy）节点，多个 Node（sclient）连接，通过 Hub 转发请求到指定节点
3. **可插拔传输层** — 定义传输层抽象接口，支持 WebSocket 实现作为第一个外部子模块，未来可扩展 gRPC、QUIC 等

### 1.2 非目标

- ~~向后兼容旧 `tunnel.Client.Do()` / `tunnel.Handler` — 旧代码保留不改，只新增~~
- ~~P2P 网状拓扑 — 留待未来扩展~~
- ~~B 端内网穿透（反向代理） — 可由中继实现自然支撑~~

### 1.3 设计原则（Go 原生）

- **接口发现而非设计** — 从使用场景提炼最小接口
- **包名简短** — `xfer.Conn` 而不是 `transport.Conn`
- **无泛型** — 所有类型具体
- **子模块隔离** — 需要外部依赖的传输实现使用独立 `go.mod` + `replace` directive
- **零值可用** — Mux 控制流/心跳内置，无需外部配置

## 2. 架构总览

```
┌──────────────────────────────────────────┐
│ 应用层                                     │
│  sclient upload/download/list/relay       │
│  sproxy /api/relay (中继转发)              │
├──────────────────────────────────────────┤
│ hub 层 — 路由表 + 节点注册 + 中继转发     │
│  RouteTable / RelayHandler               │
├──────────────────────────────────────────┤
│ tunnel 层 — HTTP 请求-响应交换             │
│  Tunnel.Do(req) → *http.Response          │
│  复用 Request/Response + AES-256-GCM      │
│  stream.go (EncryptStream/DecryptStream)   │
├──────────────────────────────────────────┤
│ mux 层 — 虚拟流多路复用                     │
│  Mux{Open/Accept/Close}                  │
│  Stream{io.ReadWriteCloser}              │
│  控制流(CtrlStream): Ping/Pong + Node注册  │
├──────────────────────────────────────────┤
│ xfer 层 — 传输层抽象                       │
│  Conn{Send/Receive/Close}                │
│  Registry — 按名字查找传输实现             │
├──────────┬──────────┬──────────┬──────────┤
│ xferhttp │ xfer/ws  │ xfer/grpc│ xfer/quic│
│ (主模块)  │ (子模块)  │ (未来)    │ (未来)    │
└──────────┴──────────┴──────────┴──────────┘
```

### 2.1 层间关系

- **xfer 层** — 定义最小消息连接接口。`Conn` = `Send/Receive/Close`。任何传输层（WebSocket、gRPC 双向流、QUIC 流、HTTP POST 包装）都实现这 3 个方法
- **mux 层** — 在一条 `xfer.Conn` 上多路复用 N 条虚拟流。每个 `Stream` 实现 `io.ReadWriteCloser`，可与 `http.Request.Body`/`http.ResponseWriter` 桥接
- **tunnel 层** — 在 mux 之上构建 HTTP 请求-响应语义。`Tunnel.Do(req)` = `mux.Open()` → 写加密请求 → 读解密响应
- **hub 层** — 在 tunnel 之上构建节点注册、路由表查找、中继转发

### 2.2 数据流

```
请求路径（sclient → Hub → Node B）:

sclient                    sproxy (Hub)                   Node B
  │                           │                            │
  │ WebSocket connect          │                            │
  ├───── Register{ID:A} ──────→│                            │
  │                           │ 注册 A 到 RouteTable        │
  │                           │                            │
  │ POST /api/relay            │                            │
  │ {target:"B", method:GET,   │                            │
  │  path:"/api/files"}        │                            │
  ├───────────────────────────→│                            │
  │                           │ RouteTable.Lookup("B")      │
  │                           │ B.Mux.Open()  ← 新建流      │
  │                           ├──────── relay request ──────→│
  │                           │                            │ 本地 HTTP 处理
  │                           │←──────── relay response ────│
  │←──────── HTTP 200 JSON ───│                            │
```

## 3. 详细设计

### 3.1 xfer 传输抽象层

**包路径：** `pkg/tunnel/xfer/xfer.go`

**目标：** 最小的消息式连接接口，任何传输层都能实现。

**未使用 `net.Conn` 的原因：**
- `net.Conn` 是字节流接口（`Read/Write`），不保留消息边界
- 缺少 `context.Context` 支持（超时/取消需要额外包装）
- 我们的场景是消息式（一次 Send = 一次 Receive），非流式

```go
package xfer

import (
    "context"
    "io"
)

// Conn 是双向保序消息连接。
//
//   Send(ctx, msg) 发送一条消息，远端 Receive 返回相同的 msg。
//   Receive(ctx) 阻塞等待一条消息。
//
// 每条消息是独立的 []byte，消息边界由实现保证。
// 实现类型：WebSocket（原生消息）、gRPC 双向流（原生消息）、
//           HTTP POST（Send=请求, Receive=响应）、
//           TCP（需要帧定界包装）。
type Conn interface {
    Send(ctx context.Context, msg []byte) error
    Receive(ctx context.Context) ([]byte, error)
    io.Closer
}

// Listener 接受来自远端的连接。
type Listener interface {
    Accept(ctx context.Context) (Conn, error)
    io.Closer
}

// Transport 是传输层实现注册单元。
type Transport struct {
    Name   string
    Dial   func(ctx context.Context, addr string) (Conn, error)
    Listen func(ctx context.Context, addr string) (Listener, error)
}
```

**注册表：**

```go
var registry = make(map[string]*Transport)

func Register(t *Transport) {
    if _, exists := registry[t.Name]; exists {
        panic("xfer: duplicate transport name: " + t.Name)
    }
    registry[t.Name] = t
}

func Get(name string) *Transport { return registry[name] }
```

**内置 HTTP POST 传输（`pkg/tunnel/xfer/http.go`）：**

将现有的 HTTP POST 隧道模式包装为 `Conn`。
- `Send(ctx, msg)` → 作为加密帧 POST 到 `/tunnel` 的 body
- `Receive(ctx)` → 读取响应 body 返回
- 每次 Send/Receive 是一次 HTTP 往返

```go
package xfer

type httpConn struct {
    url    string
    key    []byte
    client *http.Client
}

func DialHTTPPost(ctx context.Context, addr string) (Conn, error) {
    return &httpConn{url: addr + "/tunnel"}, nil
}
```

### 3.2 mux 多路复用层

**包路径：** `pkg/tunnel/mux/mux.go`

**目标：** 在一条 `xfer.Conn` 上多路复用 N 条虚拟流，每条流实现 `io.ReadWriteCloser`。

**帧格式（在 xfer.Conn 上传输，每条消息是一帧）：**

```
[4B big-endian StreamID]
[1B FrameType]
[1B Flags]
[2B PayloadLength big-endian]
[PayloadLength 字节的负载]
```

**帧类型：**

| FrameType | 名称 | 方向 | 用途 |
|-----------|------|------|------|
| 0x00 | Data | 双向 | 用户流数据，负载为用户流内容 |
| 0x01 | Open | → Listener | 通知远端有新的用户流 |
| 0x02 | Close | 双向 | 关闭指定流 |
| 0x03 | Ping | 双向 | 心跳探测 |
| 0x04 | Pong | 双向 | 心跳回复 |

**保留流 ID：**

| StreamID | 用途 |
|----------|------|
| 0 | 控制流（Ping/Pong + 注册帧） |
| 1 以上 | 用户流 |

```go
package mux

import (
    "context"
    "io"
    "github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

type StreamID uint32
type FrameType byte

const (
    FrameData  FrameType = 0x00
    FrameOpen  FrameType = 0x01
    FrameClose FrameType = 0x02
    FramePing  FrameType = 0x03
    FramePong  FrameType = 0x04
)

type Role int

const (
    RoleDialer   Role = iota // 主动拨号端（sclient）
    RoleListener             // 监听接受端（sproxy Hub）
)

// Mux 在一条 xfer.Conn 上多路复用多条虚拟流。
type Mux struct {
    conn     xfer.Conn
    role     Role
    streams  map[StreamID]*Stream
    nextID   StreamID
    acceptCh chan *Stream
    ctrlCh   chan *ctrlMsg  // 控制流消息通道
    done     chan struct{}
    // 单 goroutine 事件循环
}

// New 创建 Mux，启动事件循环 goroutine。
func New(conn xfer.Conn, role Role) *Mux { ... }

// Open 创建一条新流（Dialer 端使用）。
func (m *Mux) Open(ctx context.Context) (*Stream, error) { ... }

// Accept 等待接受一条新流（Listener 端使用）。
func (m *Mux) Accept(ctx context.Context) (*Stream, error) { ... }

// Close 关闭所有流和底层连接。
func (m *Mux) Close() error { ... }

// Stream 代表一条虚拟流，实现 io.ReadWriteCloser。
type Stream struct {
    id   StreamID
    mux  *Mux
    rCh  chan []byte
    wCh  chan []byte
    done chan struct{}
}

func (s *Stream) Read(p []byte) (n int, err error) { ... }
func (s *Stream) Write(p []byte) (n int, err error) { ... }
func (s *Stream) Close() error { ... }
func (s *Stream) ID() StreamID { ... }
```

**Mux 事件循环（单 goroutine 驱动）：**

```go
func (m *Mux) loop() {
    for {
        select {
        case <-m.done:
            return
        default:
        }
        msg, err := m.conn.Receive(m.ctx)
        if err != nil {
            m.closeAll(err)
            return
        }
        hdr := parseFrameHeader(msg)
        switch hdr.Type {
        case FrameData:
            m.dispatchData(hdr.StreamID, hdr.Payload)
        case FrameOpen:
            m.acceptStream(hdr.StreamID)
        case FrameClose:
            m.closeStream(hdr.StreamID)
        case FramePing:
            m.conn.Send(m.ctx, buildPongFrame())
        case FramePong:
            m.handlePong()
        }
    }
}
```

**心跳机制：**

- Dialer 端每 30s 发 Ping，Listener 端回复 Pong
- 使用控制流（StreamID=0）
- 90s 内无 Pong 回复视为断开，closeAll

### 3.3 tunnel 层（重构）

**包路径：** `pkg/tunnel/tunnel.go`（新增 Tunnel 类型，旧类型不动）

```go
// Tunnel 在一条 mux 连接之上提供 HTTP 请求-响应交换。
type Tunnel struct {
    mux    *mux.Mux
    key    []byte // AES-256 密钥，nil 表示不加密
}

// NewTunnel 创建隧道。key 可 nil（不加密）。
func NewTunnel(m *mux.Mux, key []byte) *Tunnel { ... }

// Do 发送一个 HTTP 请求并返回响应。
//
// 流程：
//   1. mux.Open() 创建一条新流
//   2. 写入请求 Request metadata (JSON) + body
//   3. 读取响应 Response metadata (JSON) + body
//
// Request/Response 结构体复用现有定义。
func (t *Tunnel) Do(req *http.Request) (*http.Response, error) { ... }

// Serve 在一个 http.ServeMux 上提供服务端能力：
//   接受流 → 读取请求 → 在 mux 中处理 → 写回响应
func (t *Tunnel) Serve(handler http.Handler) error { ... }
```

**`Do()` 内部流程（加密版本）：**

```
1. stream = mux.Open(ctx)
2. reqMeta = json.Marshal(Request{Method, URL, Headers})
3. encMeta = Encrypt(key, reqMeta)
4. stream.Write(encMeta)
5. EncryptStream(key, req.Body, stream)  // 流式加密 body
6. stream.CloseWrite()  // 半关闭
7. respMetaRaw = stream.Read()  // 读响应元数据
8. respMeta = Decrypt(key, respMetaRaw)
9. 从 stream 流式 DecryptStream 读取响应体
```

### 3.4 hub 中继层

**包路径：** `pkg/tunnel/hub/hub.go`

**控制帧（在 mux 控制流 StreamID=0 上传输）：**

通过 mux 的控制流，发送自定义控制消息。控制流消息格式为 `[1B MsgType][Payload]`。

| MsgType | 名称 | 方向 | Payload |
|---------|------|------|---------|
| 0x01 | Register | Node→Hub | NodeID 字符串 |
| 0x02 | Registered | Hub→Node | OK / 错误信息 |
| 0x03 | Unregister | Node→Hub | NodeID 字符串 |

```go
package hub

import "sync"

type NodeID string

// RouteTable 是 Hub 的核心路由表。
// 线程安全，支持并发读写。
type RouteTable struct {
    mu      sync.RWMutex
    nodes   map[NodeID]*NodeInfo
}

type NodeInfo struct {
    ID      NodeID
    Mux     *mux.Mux
    Addr    string      // 远端地址
}

func (rt *RouteTable) Lookup(id NodeID) (*mux.Mux, bool) { ... }
func (rt *RouteTable) List() []NodeInfo { ... }
```

**中继请求处理（sproxy Hub 端新增）：**

sproxy 新增 `POST /api/relay` 路由：

请求体 JSON：
```json
{
    "target": "node-abc",
    "method": "GET",
    "path": "/api/files",
    "headers": {"Accept": "application/json"},
    "body_base64": ""
}
```

响应体 JSON：
```json
{
    "status": 200,
    "headers": {"Content-Type": "application/json"},
    "body_base64": "...."
}
```

Node 端收到中继请求后，将请求转发到本地 HTTP 服务并返回响应。

### 3.5 WebSocket 传输子模块

**模块路径：** `github.com/cocomhub/sproxy/xfer/ws`

**独立 go.mod：**

```
module github.com/cocomhub/sproxy/xfer/ws

go 1.26

require (
    github.com/coder/websocket v1.8.12
    github.com/cocomhub/sproxy v0.0.0
)

replace github.com/cocomhub/sproxy => ../../../
```

**实现：**

```go
package xferws

import (
    "context"
    "github.com/coder/websocket"
    "github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
    xfer.Register(&xfer.Transport{
        Name: "ws",
        Dial: Dial,
        Listen: Listen,
    })
}

type wsConn struct {
    wc  *websocket.Conn
    rctx context.Context // 读超时控制
}

func (c *wsConn) Send(ctx context.Context, msg []byte) error {
    return c.wc.Write(ctx, websocket.MessageBinary, msg)
}

func (c *wsConn) Receive(ctx context.Context) ([]byte, error) {
    _, msg, err := c.wc.Read(ctx)
    return msg, err
}

func (c *wsConn) Close() error {
    return c.wc.Close(websocket.StatusNormalClosure, "bye")
}

func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
    wc, _, err := websocket.Dial(ctx, addr, nil)
    if err != nil {
        return nil, err
    }
    return &wsConn{wc: wc}, nil
}

func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
    // HTTP server 处理 WebSocket 升级
    // 返回的 Listener.Accept() 返回升级后的 wsConn
}
```

### 3.6 现有代码变动

**保留不动：**
- `pkg/tunnel/tunnel.go` — 全部现有代码（`Request`/`Response`、`Encrypt`/`Decrypt`、`Handler`、`Client`、`NewClient`/`NewHandler`/`NewLocalHandler`、`streamRecorder`）
- `pkg/tunnel/stream.go` — 全部现有代码（`EncryptStream`/`DecryptStream`）

**新增文件：**
- `pkg/tunnel/xfer/xfer.go` — 传输层抽象接口 + 注册表
- `pkg/tunnel/xfer/http.go` — HTTP POST 包装为 Conn（内置）
- `pkg/tunnel/mux/mux.go` — 多路复用器
- `pkg/tunnel/tunnel.go` — 新增 `Tunnel` 类型（`NewTunnel`/`Do`/`Serve`）+ `RelayConn`
- `pkg/tunnel/hub/route_table.go` — 路由表
- `pkg/tunnel/hub/control.go` — 控制帧定义 + 编解码
- `xfer/ws/go.mod` — WebSocket 子模块
- `xfer/ws/ws.go` — WebSocket 传输实现
- `pkg/server/relay.go` — sproxy 中继路由 handler

**修改文件：**
- `pkg/client/client.go` — 新增 `WithXfer(name string)` 选项
- `cmd/sproxy/root.go` — 注册 ws 传输 + 新增 `/api/relay` 路由
- `cmd/sclient/root.go` — 新增 `relay` 子命令
- `go.mod` — 新增 `github.com/coder/websocket` 等依赖

### 3.7 sclient `relay` 子命令

```bash
sclient relay --transport ws --hub ws://hub.example.com/ws --local http://localhost:8080
```

功能：
1. 使用 xferws 连接到 Hub 的 WebSocket 端点
2. 创建 Mux（RoleListener）
3. 通过控制流注册 NodeID
4. 循环 `mux.Accept()` 等待中继请求
5. 每条请求流：读取 HTTP 请求 → 发送到 `--local` 本地服务 → 写回响应

### 3.8 配置更新

sproxy 配置新增段：

```yaml
hub:
  enabled: true              # 启用 Hub 模式
  node_id: ""                # 本机节点 ID，空串自动生成
  relay_token: ""            # 中继认证 token

  transports:
    ws:
      enabled: true
      listen: ":18084"       # WebSocket 监听地址
      path: "/ws"            # WebSocket 升级路径
```

sclient 配置新增段：

```yaml
relay:
  transport: ws
  hub_url: ws://hub.example.com/ws
  node_id: ""
  local_addr: http://localhost:8080
```

## 4. 安全设计

- **加密层次** — mux 层不加密，tunnel 层在流数据上做 AES-256-GCM
- **传输层安全** — WebSocket 可走 `wss://`，依赖 TLS
- **节点认证** — 使用现有 tunnel_key 做注册帧加密 + token
- **心跳认证** — Ping/Pong 携带随机 nonce，防伪造重放
- **中继授权**— `/api/relay` 需 Bearer token 或 API key

## 5. 实现计划阶段

| 阶段 | 内容 | 依赖 |
|------|------|------|
| 1 | xfer层接口 + HTTP实现 + 注册表 | 无 |
| 2 | mux层（帧协议 + 事件循环 + 流管理 + 心跳） | 阶段1 |
| 3 | tunnel 层（Tunnel.Do + Tunnel.Serve） | 阶段2 |
| 4 | hub 层（RouteTable + 控制帧 + 中继转发） | 阶段3 |
| 5 | WebSocket 子模块 | 阶段1 |
| 6 | sclient relay 子命令 | 阶段4+5 |
| 7 | sproxy 集成（路由 + 配置） | 阶段4+5 |
| 8 | FileClient 集成 WithXfer | 阶段3 |

## 6. 测试策略

- **xfer 层** — 单元测试：注册表操作，MockConn 实现测试
- **mux 层** — 单元测试：使用 `xfertest.Pipe()`（内存管道）测试 Open/Accept/Close/心跳/并发
- **tunnel 层** — 集成测试：mux + tunnel 端到端 HTTP 请求-响应
- **hub 层** — 集成测试：两个模拟 Node 连接 Hub，测试中继转发
- **WebSocket 子模块** — 集成测试：启动 WS listener，连接测试消息往返
- **端到端** — 启动 sproxy Hub，sclient relay 连接，通过中继执行文件操作

## 7. 未来扩展

- **gRPC 传输** — `xfer/grpc/`，使用 gRPC 双向流
- **QUIC 传输** — `xfer/quic/`，使用 quic-go
- **WebRTC 传输** — `xfer/webrtc/`，浏览器直连
- **网状拓扑** — RouteTable 增加多跳路由
- **传输层自动协商** — 客户端和服务端协商最佳传输协议