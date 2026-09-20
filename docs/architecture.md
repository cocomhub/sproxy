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
│  tcp     │ xfer/ws  │ xfer/grpc│ xfer/quic│
│ (内置)    │ (子模块)  │ (子模块)  │ (子模块)  │
└──────────┴──────────┴──────────┴──────────┘
```

### xfer 层（`pkg/tunnel/xfer`）

传输层抽象，定义最小消息式连接接口。任何传输协议（TCP、WebSocket、gRPC 双向流、QUIC 流、
WebRTC DataChannel）只需实现 3 个方法即可接入上层多路复用系统。

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

**内置实现：** TCP（`pkg/tunnel/xfer/internal/tcp`，含 `tcp+tls` 变体）——长度前缀帧协议。

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

**已装配传输**（`cmd/sclient`/`cmd/sproxy` 经 import 注册）：`ws`（WebSocket，挂主 HTTP
端口）/ `tcp`（裸 TCP，独立端口）/ `quic`（QUIC UDP，独立端口，`relay --transport quic`
与 `hub.transports.quic` 装配；自带 TLS/ALPN `sproxy-quic`）。

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

**接收侧：不阻塞的帧分发 + 以窗口为上界的溢出缓冲。** `readLoop` 是**单 goroutine 串行**
处理所有帧（包内常驻 goroutine 只有 readLoop / writeLoop / pingLoop），因此任何帧处理器里的
同步阻塞都会停摆**整条连接**（含其它健康流），并因收不到对端 Pong 而在 90–120s 后被心跳
超时拆掉整条连接。据此：

- 数据帧投递**绝不阻塞 readLoop**：每流有一个 64 帧的接收通道，通道满时帧落入**溢出缓冲**
  （FIFO，保序；一旦有溢出项，后续帧一律继续追加，免得后到帧先于更早到达的帧交付）；
- 溢出缓冲以**流控窗口为上界**：本侧只为被应用取走的帧补发等长信用（窗口 65536 字节），
  守协议对端在本侧的未确认字节 ≤ 窗口，而信用在“取帧”时即发放 ⇒ 持有量上界 = 窗口 + 一帧
  （131071 字节；判据为**严格大于**才违约 ⇒ 等界不误判）；
  越过该上界即只可能是对端**超窗口灌数据** ⇒ 只 `Abort` 该流（fail-closed，连接与其它流存活），
  计入 `sproxy_mux_stream_window_violations`；
- 溢出缓冲的**内存**有三重上界：未消费字节（窗口 + 一帧）、**数据帧条目数**（窗口字节数 + 2，
  兜住“零长度帧不增字节只增条目”的远程灌填）、**底层数组**（已消费前缀在累计 4096 条后被压缩
  搬运，故数组峰值与传输总量无关，排空时整体释放）；
- **interop 前提**：上述上界假定对端**按字节**计流控窗口（本仓实现如此）。若对端**按帧**计窗口，
  它可能在字节上仍守协议却因字节/条目上界被判违约 ⇒ 属行为差异，需协议文档与对端实现确认
  （本仓 `stream.Write` 对 `len(p)==0` 不发帧，故仓内对端不受影响）；
- 心跳回复与 UDP 数据报同样不在 readLoop 内做同步发送（见 `frame_handler.go`）。

**指标收集：** mux 内置 `Metrics` 结构体，记录流数、帧数、字节数、Ping/Pong 和错误
计数，可通过 `GET /metrics` 查看。readLoop 相关观测：`sproxy_mux_readloop_{push,datagram,pong}_*`
（进入次数/耗时；修复后 datagram/pong 应≈ 0，push 的**耗时**也应≈ 0，其
`sproxy_mux_readloop_push_waits` 仍记录“险些阻塞/溢出”的真实背压次数）、
`sproxy_mux_stream_datach_max_frames`（帧数量纲，恒 ≤ 64）、
`sproxy_mux_stream_buffered_max_bytes`（字节量纲的“已收未消费”峰值）、
`sproxy_mux_stream_overflow_spills`、`sproxy_mux_stream_window_violations`。

### tunnel 层（`pkg/tunnel`）

在 mux 之上构建 HTTP 请求-响应语义。提供两种隧道模式：

**传统模式：**
- `NewLocalHandler(key, localMux)` → 标准 `http.Handler`（`key` 参数占位，真实密钥由认证层放入请求 ctx）
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
- **跨 hub 联邦（多跳链式中继）：** 本 hub 路由表未命中目标节点时，把 relay 拨号请求
  转发到「上报该节点的联邦对端 hub」，实现 `A→hub1→hub2→B` 链式中继（见
  `pkg/server/federation_forward.go`）。

#### 多路径自动选路（SmartDial，2026-09-19，`pkg/tunnel/mesh`）

`sclient mesh connect --smart` 并行竞速「直连 / hub 中继 / 经中间节点多跳」三类候选，
按端到端建连耗时（RTT 近似）择优；胜者缓存 TTL 内单路复用（链路变化自动重竞速）。

**候选展开模型**（`PathProvider.Expand() []Candidate`，v0.15.0 后为外部 API）：

- 每个路径类型负责展开候选——direct=1、relay=1、via-node=N 个中间节点 X（每个 X 生成
  `via-relay:X` + `via-direct:X` 双候选，见下）；竞速核心只理解「候选」这一最小单位。
- 候选 ID 即缓存 key（`via-relay:X1` / `via-direct:X1` 可区分）；注册表 `Register/Delete`
  递增代次（gen），缓存快照在 gen 未变时直接复用（零 Expand、零 ListHubNodes 网络往返）。
- 优先度：direct(100) > via-node(80) > relay(50)；`MaxCandidates` 默认 8。

**经中间节点多跳（via-node）**：

- `via-relay:X`：数据面经 hub 中继（`RelayStream(X, T)`），X 出站拨 T。
- `via-direct:X`：数据面 webrtc 直连 X（`DialWebRTC(HubSignaler(X))` 打洞，hub 只承载信令
  控制面），X 的 `relay.Serve` 出口拨 T——零新协议；无 hub 信令桥时 fail-closed 报错。
- 安全边界：X 出口拨号由 `DialPolicy`（`--dial-allow` 精确放行）把守，webrtc 直连 mux 流
  与 hub 中继流走同一 relay.Serve 分支，无新暴露面。

#### 多跳发现（方案 B，2026-09-19）

联邦节点表端点（`GET /api/hub/federation/nodes`）除返回本 hub 路由表外，**合并本 hub
的联邦候选**（`FederationClient.Candidates()`），使对端能看到 2 级节点
（`A→hub-B→hub-C→B` 链式发现）：A 从 hub-B 拉节点表即可发现注册在 hub-C 的节点，
转发时逐级递归（A→hub-B→hub-C）。

防环设计：
- **同步是单次拉取、不递归**——hub-B 只返回「路由表 + 自己的直接候选」，不再次拉取
  hub-C 的候选合并，因此 A 最多看到 2 级节点，无无限回声；
- **候选的转发链路**由 `X-Relay-Hop`（上限 4）+ `X-Relay-Path`（回源拒绝）防环；
- A↔B 互配时可能互相看到对方节点，但仅作发现/可达性候选，不进入路由表，且转发时
  本 hub 路由表命中优先，无实际危害。

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

## 数据流示例：正向 HTTP 代理（http-proxy）

`sclient http-proxy` 是标准正向 HTTP 代理（绝对 URI + CONNECT），支持**本地直连优先**：

```
本机客户端（curl/浏览器/Go 应用，http_proxy 环境变量）
  │  HTTP 绝对 URI（GET http://host/）或 CONNECT host:443
  ▼
pkg/httpproxy.Server（协议：转发 / 隧道 / Basic 认证 / hop-by-hop 剥离）
  │  Dial 注入（本地直连优先，失败回退出口）
  ├─ 路径 1：net.Dialer 直连目标（网络好，零 mesh 开销）
  └─ 路径 2：mesh.Dial → 出口节点（webrtc 直连 / hub 中继）→ 出口拨号策略 → 目标
```

- 本地直连是「本机作为出口」的显式选择（不经出口拨号策略）；出口路径的目标由出口节点
  `NewServiceDialPolicy` 把关（SSRF 边界不变，信任面不扩大）；
- `--exit-auto` 为出口节点自动选择（hub 节点列表的 `Tags: ["exit"]` 优先，候选 failover）；
- 防环回：`pkg/httpproxy` 转发用 `http.Client` 恒设 `Transport.Proxy = nil`（不读系统代理环境变量）。

## 客户端追踪（`pkg/client` + `pkg/telemetry`）

`pkg/telemetry` 是零依赖的 OpenTelemetry 式骨架（`Tracer` / `SpanContext` / `Carrier`），
默认实现基于标准库 `log/slog`。`pkg/client` 的每个请求都会开一个 span（名 `METHOD /path`），
并把 W3C `traceparent` 注入请求头，服务端 `requestLogMiddleware` 据此把两侧日志串到同一个
`trace_id` 上。

默认行为：

| 关注点 | 默认 | 说明 |
|--------|------|------|
| 日志量 | **静默** | span 结束行以 `DEBUG` 落地；默认 Info 级 handler 下不产生任何输出（此前每请求一行 `INFO`） |
| `traceparent` | **照旧注入** | 静默只针对日志，链路关联能力不变 |
| 落地点 | 客户端 logger | 默认 tracer 的 logger 即 `FileClient` 的 logger ⇒ `client.WithLogger(lg)` 同时改道追踪输出 |

打开 span 日志（三选一）：

- CLI：`sclient -v`（等价于把 handler 级别设为 `debug`）；服务端进程内同理由 `log_level: debug` 打开。
- SDK：`client.WithLogger` 注入 Debug 级 logger——
  `client.NewFileClient(url, client.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))))`。
- 换实现：`client.WithTracer(myTracer)`（如 `pkg/telemetry/ext/otel` 的 OTel 适配）。

`trace_id`/`span_id` 由 `telemetry.WithContextHandler` 从 ctx（`SpanContextKey`）自动附加，
无需在业务日志里手工传参；该包装是幂等的（`sclient` 的 `initLogger` 与客户端的默认 logger
会各包一层，幂等保证每行只有一份 ID）。

**完全关闭追踪**：`client.WithTracer(nil)`（等价 `telemetry.Nop()`）——不建 span、不打日志，
**也不再注入 `traceparent`**。只想「静默但仍透传 traceparent」时保持默认即可，不要用 `Nop()`。

## 相关包路径

| 层 | 包路径 | 说明 |
|----|--------|------|
| xfer | `pkg/tunnel/xfer/` | 传输层抽象接口 + 注册表 |
| tcp | `pkg/tunnel/xfer/internal/tcp/` | TCP 内置传输实现（含 tls 变体） |
| xferws | `xfer/ws/` | WebSocket 传输子模块（独立 go.mod） |
| xferquic | `xfer/quic/` | QUIC 传输子模块（独立 go.mod；relay/hub 装配，UDP 形态） |
| mux | `pkg/tunnel/mux/` | 虚拟流多路复用器 |
| tunnel | `pkg/tunnel/tunnel_mux.go` | 多路复用隧道（Tunnel 类型） |
| hub | `pkg/tunnel/hub/` | 中继路由表 + 注册框架 |
| relay | `cmd/sclient/relay.go` | sclient 中继节点命令 |
| telemetry | `pkg/telemetry/` | 追踪骨架（slog tracer / `traceparent` 传播，见上节） |

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

### 多卷布局（可选）

多卷（配置 `volumes`，`pkg/volume` 纯域 + `pkg/server/volumes.go` 装配）在不破坏单卷布局的
前提下把「存储根」从单个扩展为多个物理根。每卷是独立物理根，装配时各自
`storage.OpenRoot`（独立 `LAYOUT_VERSION` 校验）并建立卷容量池（`pkg/quota.Pool`）：

```
volumes[0] (默认卷，root=storage_root 或显式)     volumes[1] (追加盘，如 disk2)
  LAYOUT_VERSION                                  LAYOUT_VERSION
  <tenant>/                                       <tenant>/
    user/  cloud/  archive/  chunk/  version/       user/  cloud/  archive/  chunk/  version/
    meta/   # meta 单点权威：默认卷（volumes[0]）持有 checksums/凭据/任务状态
```

- **寻址**：文件 = `(卷, owner, 相对路径)`。`storage.Tenant.UserRel/FeatureRel` 映射不变；
  每卷的 user 桶都遵循同构六桶布局（AD-5：非默认卷 `version/`/`chunk/` 等特征桶跟随 user 卷）。
- **默认卷权威（meta 单点）**：`<default>/<owner>/meta/` 仍是 checksum、凭据与云任务状态的
  权威归属；默认卷被 ACL 排除的 owner 不得经回退读默认卷遗留（AD-6 收口）。
- **写路由**：`routeUpload`（`pkg/server/volumes.go`）按 owner 卷视图（ACL）选卷——显式
  `volume` 单候选、自动 `placement`（`prefer-default`/`spread`）+ 换卷。owner 全局配额跨卷合计，
  卷容量独立封顶；双账本（Scope + 卷 Pool）reserve→Commit/Adjust/Release。
- **读定位**：`locateOwnerFile`/`locateForRead` 默认卷快路径 + 视图其余卷只读探测；唯一性
  （AD-4）保证同 rel 至多一卷命中。带 `?volume=` 时只在指定卷定位（fail-closed 404）。
- **卷 ACL**：`volumes[].acl.mode`（deny 黑名单/allow 白名单）由 `parseVolumeACL` 解析为
  `pkg/volume.ACL`；owner 卷视图 = `volume.AllowedVolumes`。默认缺省（deny + 空名单）= 默认开放。
- **跨卷移动**：`POST /api/volumes/move?from_volume&to_volume&filename`——to 侧双 reserve →
  流式复制（O_EXCL 临时 + fsync + 原子 rename）→ 删源 → 双 commit + from 侧释放；目标唯一性查重 409。
- **卷再平衡**：`POST /api/volumes/rebalance?from_volume&to_volume&max_bytes`——把 from 卷 user 桶文件
  按大小降序逐文件复用 move 原子语义迁到 to 卷，max_bytes 用尽或无可迁文件即停（单文件失败跳过，
  尽力而为）；remaining 为迁移后 from 卷池 Usage。
- **API/客户端**：`GET /api/volumes`（per-owner）、list 文件条目 `volume` 字段、upload 成功
  `X-Volume` 头、可选 `volume` 参数（upload 表单 / 其余 query）。FileClient 卷上下文
  （`WithVolume`/`SetVolume`，零值=auto）、`Volumes()`/`MoveVolume()`；sclient `volumes` /
  `--volume` / `mv --to-volume`；WebUI 卷 badge + 仪表 + 上传下拉。
