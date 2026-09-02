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
│  RouteTable / RelayStreamHandler         │
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
- **流中继转发：** `POST /api/relay/stream` 升级为到目标叶子的双向字节流（RelayStreamHandler）

## 数据流示例：中继请求

```
sclient                    sproxy (Hub)                   Node B
  │                           │                            │
  │ WebSocket Connect          │                            │
  ├──── Register{ID:"node-a"}→│                            │
  │                           │ 注册到 RouteTable           │
  │                           │                            │
  │ POST /api/relay/stream     │                            │
  │ {target:"node-b",          │                            │
  │  addr:"127.0.0.1:22"}      │                            │
  ├───────────────────────────→│                            │
  │                           │ RouteTable.Lookup("node-b") │
  │                           │ targetMux.Open() → stream   │
  │                           ├───── 拨号帧(addr) ─────────→│
  │                           │                            │ 叶子 DialAllowed 校验后拨号
  │                           │←── DialResultFrames ok ─────┤
  │←──────── HTTP 200 ────────┤                            │
  │  (此后为双向字节流)          │  ←───── TCP 数据 ────────── │
```

> 说明：`POST /api/relay/stream`（RelayStreamHandler）升级为到目标叶子的双向字节流。
> 拨号结果由叶子经 `DialResultFrames` 门控回报，hub 写 200 前先读结果帧（ok→200 /
> 拨号失败→502 / 超时→504），客户端据此可感知拨号失败并回退候选。

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

## 多租户存储布局（文件服务侧）

sproxy 文件服务采用**租户自包含存储布局**（`pkg/storage` / `pkg/quota` / `pkg/store`）：

```
<storage_root>/
  LAYOUT_VERSION          # 布局版本标记（storage.OpenRoot 写入/校验）
  <tenant>/               # 每租户一个子根（匿名租户名 "anonymous"）
    user/                 # 用户文件桶（upload/download/delete/rename/list/search）
    cloud/                # 云下载任务文件（<taskID>/<file>）
    archive/              # 云归档文件（<name>.tar.gz）
    chunk/                # 分块上传会话目录（<uploadID>/）
    version/              # 文件版本（<userRel>/<versionID>）
    meta/                 # 服务端内部账本（checksums.json / cloud 任务状态 / sync 状态）
```

- 旧的 `.__xx__` 魔法目录（`.__cloud__`/`.__versions__`/`.__chunked__`/`.__downloads__`/
  `.__cloud_archives__`/`.__sync__`）已废弃（P5 删除），用户文件统一映射到 `user/` 桶。
- **隔离**：每租户一个 `*os.Root`（`pkg/storage.Root`，os.Root 防穿越由标准库保证）+
  一个 `quota.Scope`（父子链聚合到全局池）。`storage.Tenant.UserRel/FeatureRel` 是路径
  判定单一入口（逐段 `ValidSegmentName`，拒绝 `.__` 前缀、Windows 保留设备名等）。
- **配额**：`pkg/quota` Pool/Scope/Reservation；写路径 TryReserve→Commit，覆盖 Adjust，
  删除 ReleaseUsage；周期扫描 `reconcileQuotaScopes` 校准 Scope 到磁盘实际（重启不回溯）。
- **历史路径**：`ValidateFilePath`（`pkg/server/validate.go`）保留做基础清洗，指向
  `pkg/storage.NormalizeRemote`；租户映射与段名校验以 `pkg/storage` 为准。
