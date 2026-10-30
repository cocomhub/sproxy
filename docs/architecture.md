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
- **跨卷复制**：`POST /api/volumes/copy?from_volume&to_volume&filename`——与 move 同原子语义，
  但**不删源**（from 侧账本不动）；目标同 rel 已存在且 checksum 一致 → 200 幂等（不重复占配额），
  不一致 → 409（copy API 不覆盖用户内容）。
- **卷镜像（mirror_to / mirror_interval）**：`volumes[].mirror_to` 声明镜像目标，`mirror_interval`
  周期 goroutine（与 versionGCLoop 同构）逐 owner（默认卷根扫描）逐文件复制源卷 user 桶到目标卷
  （ACL 过滤 owner 视图；目标一致跳过、不一致覆盖收敛；配额不足跳过尽力而为；每文件 uploadingFiles 锁）；
  审计 `volume_mirror`/`volume_copy`；配置校验拒绝自指/不存在/成环镜像链。
- **卷再平衡**：`POST /api/volumes/rebalance?from_volume&to_volume&max_bytes`——把 from 卷 user 桶文件
  按大小降序逐文件复用 move 原子语义迁到 to 卷，max_bytes 用尽或无可迁文件即停（单文件失败跳过，
  尽力而为）；remaining 为迁移后 from 卷池 Usage。
- **API/客户端**：`GET /api/volumes`（per-owner）、list 文件条目 `volume` 字段、upload 成功
  `X-Volume` 头、可选 `volume` 参数（upload 表单 / 其余 query）。FileClient 卷上下文
  （`WithVolume`/`SetVolume`，零值=auto）、`Volumes()`/`MoveVolume()`；sclient `volumes` /
  `--volume` / `mv --to-volume`；WebUI 卷 badge + 仪表 + 上传下拉。

### 统一存储卷模型（StorageVolume 唯一身份 + VolumeView 唯一取卷入口）

设计：`docs/designs/2026-10-28-unified-volume-model.md`（U1-U5 已落地）。

本地卷与外部卷统一为 **`registry.StorageVolume`** 契约（`Name/Kind/FS/Close` + 可选能力
`Footprinter`/`PhysBound`/`StatsProvider`），`registry.Set` 持有 `storageVolumes` 表，
`Set.StorageVolume(name)` 是唯一取卷入口；`assembleLocalVolume` 改走 `NewLocalBackend`
（`*storage.Root` 成为本地卷内部细节，仅默认卷 tenant/globalRoot 存量路径保留）。

**按用途统一访问器 `Set.VolumeView(name, use)`**：同一卷按用途返回不同装饰链视图——
`user`（用户文件：Guard → trusted.Wrap → capacity → staging 消费方套）、`meta`（控制面：
Guard，不经容量）。装饰链在**装配期**经 `SetVolumeViewDecorator` 注入（registry 内置默认
trusted 装饰，测试/生产行为一致）；消费方永远拿不到裸 FS（R27 直连点收缩）。

**转存支持本地卷**：`checkTransferVolumePreflight` 只要求目标卷可解析（FS 非 nil），不再
要求协议非空；本地卷 target 产物落 `<owner>/user/<rel>`（`ResolveUserLocation` 统一键空间，
可经 `/download` 命中）。**本地卷转存的容量记账（R16 P1）**：`rootfs` 适配器自述
`syncpkg.LocalVolume`（本地根恒本地）⇒ 转存判定非远程 ⇒ owner 配额探测生效；卷级容量池经
装配注入 `VolumePoolChargeFor` 做**写前探测 + 写后差分结算**（`pool.ProbeReserve`/`Adjust`），
删除侧 `ReleaseCommitted` 对称（外部卷容量权威仍在 FS 层 `CapacityFS`，不双记）。转存 URL 形态 `local://<卷>/<rel>`（U4：`RegisterLocalBackend`
声明 `local` 协议，`Set.ResolveURL` 经 `VolumeView` 读本地卷）。`volume://`（`Location.String()`
卷内定位序列化）与 `local://`（协议寻址）并存分工。

### 卷状态维护（list / checksum / 索引的统一缓存层）

设计：[docs/designs/2026-10-28-volume-state-maintenance.md](./designs/2026-10-28-volume-state-maintenance.md)。

**问题**：list 的目录视图与 checksum 派生值此前有**三套实现**（本地卷 `searchIndex` +
`checksums.json`、外部卷每次远端 `ListDir`、secretdata 自管索引）⇒ 三个真相源、三条失效路径
（#874：旁路写漏一处即「list 缺条目 + checksum 空」）。

**现状（2026-10-29）**：list 的数据源已收敛为**卷状态视图**（`VolumeView.ListDir` + 缓存），
视图不可用时**实时扫描**；`searchIndex` 只服务搜索（`index.list` 已删除并硬禁，R41）。
**唯一列举入口**是装配层的 `VolumeViewLister.ListDirView`——本地卷与外部卷共用（键空间由
`ResolveUserLocation` 统一），`ExternalList`/`ExternalVolumeSource` 已删除并硬禁。

**内存上界（2026-10-30）**：卷状态缓存有**双上限**——目录快照数（`DefaultMaxEntries` 4096）
与**总条目预算**（`DefaultMaxTotalEntries` 200_000，即所有目录 `entries` 之和）。超预算按 LRU
淘汰；**单目录快照超过总预算 ⇒ 不缓存**（回源），避免一个十万文件目录挤占全部内存。

**校验巡检的台账读取（2026-10-30）**：`/api/verify` 用可选扩展 `checksum.Iterator` **分批遍历**
（锁外回调，峰值内存 O(键列表 + 单批)），不再 `GetAll()` 全量拷贝；判空用 `LenCounter`（O(1)）。
store 未实现这些能力时回落 `GetAll`（零回归）。

**读取与写入口径（2026-10-30）**：目录列举与搜索的 `checksum` 一律**逐条 O(1) 查台账**
（`checksumGetter` / `checksumLookupFor`），不再每请求拷贝全量快照；台账写入经 **debounce
合并**（`ChecksumStore.Set` 只更新内存 + 累计代际，1s 定时器合并为一次全量落盘，`Flush`/`Close`
可强制落盘）——此前每条 `Set` 都全量重写 `checksums.json`，批量 N 文件退化为 O(N²) I/O。
崩溃窗口 ≤ debounce，台账可由对象 meta 侧车重建（`RebuildChecksums`），故窗口内丢失可自愈。
两个后端都实现**卷维度键**（`VolumeKeyedStore`，§15.1.1：卷键优先 + 旧键兜底）——
集群后端缺失该能力时 `checksumSet/Get` 会静默退化为无卷键，导致多卷同名文件 checksum 串值。
领域侧一律经 `checksum.ChecksumStoreIface` 使用台账（**不得**断言具体类型 `*ChecksumStore`：
那会让集群后端被判为「未装配」，导致 list/search 的 checksum 恒空、`RebuildChecksums` 失败）；
批量回填用可选扩展 `BatchWriter`/`VolumeBatchWriter`，判空用 `LenCounter`。
合并逻辑由**唯一实现** `internal/savemerger` 承担（通用「全量落盘合并器」）：checksum 的两个后端
（`ChecksumStore` 卷内文件 / `StateBackedChecksumStore` 集群 StateStore）与 dedup 台账
（`files.DedupStore`，本地 JSON / 集群 StateStore）共用同一语义。集群后端的台账实例**按 owner
缓存**（`Handlers.checksumStores` / `Handlers.dedupStores`，两者同构）——未缓存时每次首读都会从
StateStore 全量拉回 + 反序列化；`SetStateStore` 切换后端时两者都清空（旧实例先落盘/丢弃）。

**机制（四条不变量）**：

1. **卷维度**：缓存键恒为 `(卷名, 卷内相对路径)`；跨卷聚合是「对卷集合的派生遍历」，
   不是独立全局索引。
2. **横切关注点 = 装配期装饰器**：唯一入口 `volumestate.Wrap`（与 `capacity.WrapStorageVolume`
   同形），业务代码零感知；门禁 **R44** 保证没有第二个包装点。
3. **失效由变更事件驱动**：装饰器在 `WriteFile/Delete/Rename/MakeDir/WriteIfAbsent/Move/Copy/Link`
   成功后**自动失效**（含祖先目录），调用方不需要登记 —— 这是 #874 缺陷族的结构解。
4. **卷自管只报告事实**：卷经 `sync.Entry.Checksum*` 与可选 `volume.StateGeneration`
   （廉价代际：目录 mtime / 容器 meta 版本 / LastModified）如实上报，**不得自建 list 缓存**。

**权威口径**：卷是真相源、缓存只加速 —— `Stat` 恒回源；需要 checksum 的安全操作走
「调用方给 → 现取 → fail-closed」三段式，缓存值不作删除门禁唯一依据。
**写面纪律（E2E 锁定）**：外部卷的读/写入口必须经 `VolumeView`（不得直取 `External().FS()`），
否则写后不失效缓存（`test/e2e_volume_state_test.go` 有对照组断言）。
**派生状态唯一提供方（R40）**：list/index 级缓存类型必须登记（`internal/archcheck/list_cache_types.tsv`）——
新增复用 `pkg/volumestate`，只有「卷自身是唯一能算出真值的地方」（secretdata 的密文分块索引）才允许自管。
**配置默认值安全（R43）**：`Default()` 里的特化默认值（bool `true` / 非空 string）必须登记
（`config_defaults.tsv`）——新增字段须用安全默认，需要「默认开启」时把语义反转成 `disable_xxx`。

**本地卷写面纪律（R45）**：`storage.Root` 的结构性变异方法（mkdir/remove/rename/link/chtimes）在
成功后发 `Mutation` 事件（租户子根自动归一 `<owner>/<rel>` 前缀）；装配层把它接到
`files.Service.ApplyRootMutation`，搜索索引因此**自动**跟随旁路写（转存/归档/跨卷搬移），
不需要写路径逐点登记。新增 Root 变异方法必须调用 `notifyMutation`（R45 门禁拦截）。
**内容写**（`OpenFile` 裸句柄无法观察）由卷的 FS 写面在成功后调用 `Root.MarkWritten(rel)` 声明
（本地卷唯一调用点 = `pkg/rootfs.WriteFile`）⇒ 转存/归档/镜像/备份落盘同样自动进索引。
**R42**：带 `ListDir` 的 FS 实现必须实现 `StateGen` 或登记理由（`fs_derived_state.tsv`），
保证「有派生状态的实现能被缓存层探测到变化」。
**范围**：本期仅非本地卷（外部/封装）；本地卷沿用既有实现（门禁 R46 登记为只减不增）。

## 可信卷（Trusted Volume：完整性 sidecar + 元信息隔离 + 卷级容量）

「可信卷」把「读回内容 == 写入内容」从**约定**升级为**结构性不变量**：服务端为每个文件维护一份
用户不可访问的整文件完整性证据（`FileMeta` sidecar），读路径逐分块校验，损坏**fail-closed**。
设计细节与已记档边界见 [docs/designs/2026-10-10-trusted-volume.md](./designs/2026-10-10-trusted-volume.md)。

### 数据模型与布局

- `FileMeta`（`pkg/files/meta`）：`Size`/`TotalSHA256`/`ChunkSize` + 逐分块 `{index,offset,size,sha256,md5}`，
  外加 `CTime`/`MTime`/`MediaType`/`Extra`。分块数上限与 sidecar 读取上限（64MiB）**自洽**——
  `ResolveChunkSize` 会为保证「写出即可读」上抬档位（1MiB…32MiB）。
- **sidecar 独立桶**：`<owner>/meta/<rel>.meta`（不是用户目录内的 `.meta`）——
  与用户真实 `*.meta` 文件不再冲突，且业务层经 `MetaBucketGuard` **结构性不可达** meta 桶。
- **键空间单一来源**：`volume.Location.FSPath()` 恒为 `<归一 owner>/<bucket>/<path>`；
  备份目标等「相对键空间」经 `prefixFS` 前缀化后才进可信链（防跨 owner 互覆与 Guard 误判）。

### 装饰器链与能力透传

按「最内层原始卷 → 容量 → 可信 → 守卫 → 业务」装配：

```
业务层 → MetaBucketGuard(TrustedVolumeFS(CapacityFS(原始 FS)))
                     ↑ staging 门卫按卷另行叠加（StagingQuotaGateFS）
```

- 每个装饰器**透明转发全部可选能力**（`meta.Provider`/`MetaExtraUpdater`/`RangeReader`/
  `DirectURLProvider`/`WriteIfAbsent`/`ReserveSpace`/`LocalVolume`/`Mover`/`Copier`/`Linker`/
  `StagingQuota*`），并由编译期断言 + 门禁 R21（`internal/archcheck/capability_parity_gate_test.go`）
  机械保证：新增能力接口若漏转发即报红（`BlockAccessor` 为一致豁免，理由见设计 §8）。
- **能力探测必须下探 `syncpkg.Innermost`**：装饰器无条件回显能力（透传契约）⇒ 直接对装饰层做
  类型断言恒真。这条纪律已修过三类失效：staging 门卫恒跳过、`RangeReader` 回落成死代码、
  `Provider` 探测失败。
- **卷内容一律经 `storage.Root`（或经 Root 的 FS 适配器 `server.NewRootFS`）**：`sync.NewLocalFS`
  是「任意目录 → 纯 os I/O」的适配器，用在**卷根**上会绕过 at-rest 加解密、封印、用户可见长度与
  容量接线——历史三个 P0 全出自这一类（DAV 面、备份源、同步本地侧）。该纪律由门禁 R26
  （`internal/archcheck/volume_root_bypass_gate_test.go`）机械保证：非测试源码中
  `NewLocalFS(...)` 的调用点必须登记并写明「为什么可以不经 Root」（与 R25
  `user_io_gate_test.go` 互补：R25 管「经 Root 但绕过用户可见入口」，R26 管「根本不经 Root」）。

### 读写路径保证

- **写**：上传（simple/multipart）、S3 网关 PUT/multipart、DAV PUT、块写、跨卷 copy/move、
  云转存、备份/恢复、回收站、sync push/pull 均在**同一写窗口**生成 sidecar；覆盖写场景
  必然**失效旧 sidecar**（否则旧 meta 描述旧内容 → 新内容永久校验失败、文件不可读）。
  写失败/序列化失败同样删旧 sidecar 退化为「无 meta」（可自愈）。
- **读**：`/download`、`/download/chunk`、S3 兼容网关 GET、DAV GET、外部卷服务端转发统一经
  `meta.VerifyReadSeeker` 逐分块比对；失败**主动中断连接**（`panic(http.ErrAbortHandler)`）+
  指标 `success=false`，绝不「200 + 截断内容」。
- **加密卷（at-rest）**：默认下发**明文**（加密只在存储层，与 DAV/S3/分享/预览语义一致），
  `?ciphertext=1` 显式导出存储原样密文；尺寸/`Content-Range`/Range 一律按**明文坐标**
  （`storage.Root.LogicalSize`），密文坐标仅在 `ciphertext=1` 分支。
- **降级可观测**：sidecar 缺失（升级后存量文件）记 Debug、读失败/畸形记 Warn；`skip_verify`
  只关读侧校验、不关 meta 生成与桶隔离。

### 卷级容量

- 配置卷/用户卷容量在 **FS 层**（`capacity.CapacityFS`）统一强制：写入累计、删除/覆盖/改名/
  服务端 Copy 释放、未知长度按实测结算；计数快照持久化（`rename` + fsync 父目录）+
  损坏/版本不识标记（Warn，避免静默归零后可超额）。
- 外部卷覆盖写按**净增**探测（`externalReserveNeed`），避免「写满即永久 507」。
- S3 网关单对象 PUT 与 multipart complete / 版本恢复一致走 **owner Scope + 卷池双账本**。

### 本地 staging 配额

外部卷上传/同步先落本地暂存 → 必须防本地盘打满：`ApplyStagingQuota` 按**最内层原始卷**三分支——
显式豁免（`StagingQuotaExempt`，如 s3 流式直传）、自管（`StagingQuotaCapable`，如 baidupcs，
per-request/per-owner 门卫包装）、否则强制 `StagingQuotaGateFS`（fail-safe 宁多勿漏）。
staging 独立 Scope 但**挂在全局池之下**（受 `max_storage_bytes` 约束）。

## 统一转存（归档 = 转存 + 预处理）

**单一管道**：`源(source) → [可选预处理] → 目标卷(destination)`。无预处理 = 纯转存（individual）；
有预处理 = 打包后转存（旧「归档」概念升级）。预处理经 `pkg/transfer` registry 选型——内置
`tar`（压缩 gzip/zstd/brotli + 可选加密 aes-256-gcm）。

三个云端归档端点共用同一执行核心 `Handlers.cloudArchiveToVolume`：
`POST /api/cloud/tasks/{id}/archive`、`POST /api/cloud/archive`、`POST /api/cloud/groups/{id}/archive`；
请求体 `target_volume` + `target_path` 指定转存目标（空 = 落租户 archive 桶，既有行为零回归）。

**目标卷写视图**——单一出口 `Handlers.externalVolumeTransferFS`（与 cloud 下载转存共用，防解析漂移）：
- `trusted.Guard(trusted.Wrap(be.FS()))`：meta 恒生成 + meta 桶隔离；外部源卷封印经装配处
  `enclosure.SealBackend` 注入 ⇒ `be.FS()` 自动继承封印。
- 目标键经 `volume.ResolveUserLocation`（恒 `<owner>/user/<rel>` 前缀 + 路径安全），与读路径
  `resolveExternalDownload` 同键空间 ⇒ 产物可经 `/download` 命中（外部卷不进本地文件索引，
  故不出现在 `/api/files` 列表）；`target_path` 可含子目录，空 = `archive/<归档名>`。
- 本地 staging 配额（`ApplyStagingQuota`；长度未知的打包流由 gate 的 `reservingReader` 按实测
  增量预留 + 背压）+ `transferWriteLock`（per-(owner,rel)，与 HTTP/DAV/S3 同键空间）+ 目标卷
  `ReserveSpace` 预检（装饰器链未实现 → `ErrUnsupported` 跳过）。
- 拒绝静默覆盖：`WriteIfAbsent` 优先（同名 → 409）；`secrets` 密钥卷、未声明协议、空 scheme
  fail-closed。转存不占租户 archive 桶（仅 `cloud_archive_max_bytes` 上限校验）。
- 加密密钥来自 `archive.key_file`（与 `/api/archive` 同约定）；`encrypt`/`cipher`/`compression`/
  `target_path` 仅与 `target_volume` 同用（否则 400，防静默忽略），且 `cipher` 须与 `encrypt`
  同用（单独指定不加密 → 400）；加密产物命名追加 `.aes`（`<名>.tar.gz.aes`，不再伪装成可直接
  gunzip 的 `.tar.gz`）。CLI 用**卷 URI 路径**（`--target <卷名>:/<路径>`，
  cd/mv 同规约）指定，客户端归一为 `target_volume`/`target_path`（协议层不解析 URI）。

**未落地**：图片组「每文件一分块」封装（设计方向 A/B 待 #34 详设）；Web UI 未暴露转存目标。

## 远端派生状态缓存与删除失效纪律（2026-10-27）

仓库持有若干「从远端/对象内容派生出来的本地状态」。按**陈旧后果**分四类，决定各自的失效强度：

| 类别 | 定义 | 陈旧后果 | 允许的过期强度 |
|---|---|---|---|
| **A 内容派生值** | checksum / size / mtime / 明文块 | **安全**：错误「一致」放过被篡改文件；错误「不一致」误拒合法操作 | **强一致或 fail-closed**，禁止乐观复用 |
| **B 句柄/凭据** | dlink URL、access token | 可用性失败，失败可观测可自愈 | TTL + 失败自愈 |
| **C 结构视图** | 目录树 / 文件索引 / 聚合列表 | 幽灵条目、看不到新文件 | 中（必须有收敛路径） |
| **D 聚合账本** | 配额用量、容量计数 | 假 507 / 过度准入 | 强，但有界漂移 + 对账 |

判定口诀：*能导致「把坏数据当好数据」的是 A 类，必须强一致；只影响「这次失败下次再试」的是
B 类，TTL 就够。*

### 产出契约（谁能零成本给就必须给）

- `sync.Entry.Checksum` **允许为空**（= 未知，调用方按需 stat）；但底层能在**列举时零额外
  I/O** 给出整文件 SHA-256（内存态）的卷**必须填**（secretdata 的 `Original.SHA256`）。
- `ChecksumType` 必须自述算法：`baidupcs` 的 `etag` **不得**填入 `FileInfo.Checksum`
  （非整文件 SHA-256，会造成错误校验语义）；无法零成本取 SHA-256 的卷保持 unknown，
  由消费方 stat 兜底（三段式：给了用 / 没给现取 / 仍拿不到明确拒绝）。

### 消费侧三段式（任何需要 checksum 才安全的写操作）

```
① 调用方给了 → 用；
② 没给 → Stat / FileMeta 现取（A 类禁止用缓存猜）；
③ 仍拿不到 → 明确拒绝（fail-closed，不静默跳过校验）。
```

前端 `resolveOpChecksum` / CLI `FileClient.resolveDeleteChecksum` 即此形状。

### 删除三段式（顺序不可颠倒）

```
① 前：校验（删除前现取的 checksum；crossCheckExternalChecksum 用 sidecar 记录值比对 =
      「防误删」而非「防篡改」——API 文档须显式声明）
② 中：删远端。失败 ⇒ 本地一律不动（不释放配额、不清缓存），保持可重试
③ 后：失效本地派生状态（确认成功后才执行；单项失败只 Warn + 记指标，绝不回滚 ②）
     a 内容缓存（块/窗口/明文）→ 即时清
     b 结构视图（索引/列表缓存）→ 即时清
     c 句柄（dlink/token）→ 即时 clear key
     d 账本（配额/容量）→ 用删除前实测字节释放（与写侧同源口径）
     e 审计/事件 → 发布
```

**最重要结论**：*凡是**本进程之外**可能被删除的东西（外部卷、多实例共享卷、用户直接用网盘
App 改的卷），其派生值要么不缓存、要么有 TTL/重扫——不能靠「我删的时候记得清」。* A 类不缓存
（每次现取）；B 类 TTL+自愈；C 类需重扫（secretdata 索引 `loadIndex` 只在挂载时跑一次；
多进程/外部改动收敛由设计文档 §6 的裁定覆盖）；D 类靠对账。

## 作用域分层（Global / Server / Assembly）

> 2026-10-30 起生效（server 治理 S1）。解决的是「**命名声称的作用域**与**实际消费者**不一致」：
> `pkg/` 下曾有约 40% 的代码只有 `cmd/sproxy` 可达，却与真正的通用能力混在同一命名空间。

### 三层定义

| 作用域 | 判据 | 命名空间 | 允许导入 |
|---|---|---|---|
| **Global** | 与进程无关的能力；`sproxy` / `sclient` / `sproxy-mcp` / tools 任一可消费 | `pkg/<capability>` | 只能导 Global |
| **Server** | 只在 sproxy 服务端进程内有意义 | `pkg/server/<capability>` | Global + Server（**不得**导装配根） |
| **Assembly** | 只做装配、路由注册与生命周期 | `pkg/server`（根包，精确匹配） | 全部 |
| **Private** | 单个二进制私有 | `cmd/<bin>/internal/...` | 该二进制子树 |

### 命名规则（新增包必须遵守）

1. 被 `cmd/sclient` 或 `cmd/sproxy-mcp` 可达 ⇒ 才可放 `pkg/<capability>`（Global）；
2. 只被 `cmd/sproxy` 可达 ⇒ 放 `pkg/server/<capability>`；
3. 两个二进制都可达 ⇒ Global，且**不得**反向依赖任何 Server 包；
4. 只服务单个二进制的 ⇒ `cmd/<bin>/internal/<capability>`。

反例（曾出现/差点出现）：`pkg/config`（实际是 server 配置 schema）、`pkg/httpapi`
（HTTP 面只有服务端有）、`pkg/service/*`（`service` 是层名不是能力名）。

### 可执行边界（门禁）

| 门禁 | 断言 |
|---|---|
| **R1**（`arch_test.go`） | 已登记包不得导入层级更高的已登记包 |
| **R2** | 子包只允许父域子树与装配层导入（`pkg/server/config` 登记为 `pkg/server` 子包） |
| **R4** | `pkg/**` 中不在 `pkg/server/**` 的包不得导入 `pkg/server/**`（Global 不依赖 Server） |
| **R6**（`server_scope_gate_test.go`） | `pkg/server/**` 子包不得导入装配根 `pkg/server`；且每个子包必须在 `Levels` 登记 |

依赖方向固定为单向：

```
pkg/server（装配根） ──▶ pkg/server/<cap> ──▶ pkg/<Global>
```

### 当前落地状态（S1 配置 / S2 通知 / S3 调度器 / S4 Handlers 分域 / S5 S3 / S6 WebDAV / S7 AI / S8 分享 / S9 指标 / S10 巡检 / S11 用量统计 / S12 告警引擎 / S13 归档助手）

- `pkg/server/config`：配置**数据模型**（55+ 类型）、`Default`/`SetDefaults`、`Validate`
  及其校验/派生辅助（`ParseVolumeACL`、`ReadListenerPins`、`ParseMaintenanceWindow`、
  `IsLoopbackHost`、`ValidateIPList`）、`Load*`/`SaveConfig`。
- `pkg/server/config_alias.go`：**迁移期兼容层**（类型/常量别名 + 4 个转发函数），
  让 `pkg/server` 存量 100+ 文件与 `cmd/sproxy` 零改动。
  **移除条件**：后续切片逐个搬入 `pkg/server/<cap>` 后删除本文件，新代码直接 import
  `pkg/server/config`。
- 共享 IP 工具下沉 `pkg/netutil`（`ParseIPNets` / `AllowIPsCoverLoopback`）；
  配额桶名单下沉 `pkg/storage`（`QuotaBucketNames`）——两者都是 Global 能力，
  不应留在 server 作用域。

**S2：通知域（`pkg/server/notify`）**

- 迁入：`NotifyCenter`（注册表/规则路由/去抖/重试/有界历史）、全部渠道实现
  （wecom/serverchan/email/webhook/alertmanager/grafana）、webhook 签名、RSS/Atom feed 渲染。
- **依赖倒置**：通知域只接受自己的 `notify.Event`（Action/Object/Result/Detail/TS），
  装配层在 `RecordAudit` 里做一次字段映射——通知域不反向依赖 `AuditEvent`/装配层（R6）。
- 装配层保留 HTTP 面：`notify_handlers.go`（history/test/feed 三个端点 + feed token 门禁）。
- `pkg/server/notify_alias.go` 为迁移期兼容层（`NotifyCenter`/`Notifier`/`NotifyMessage`/`Event`）。
- HMAC-SHA256 下沉 `pkg/cryptox.HMACSHA256`（S3 SigV4 与 webhook 签名共用单一实现）。

**S3：统一周期任务调度器（`pkg/server/scheduler`）**

- 迁入：`Scheduler`/`Task`/`TaskFunc`（周期任务注册、单飞防重入、panic 恢复、维护窗口、
  优雅停止 + `FinalRunOnStop`），零 `pkg/*` 依赖（L0）。
- 装配层保留周期任务的**注册内容**（uploading 清理 / version-gc / trash-gc / share-cleanup /
  usage-flush），经 `scheduler_alias.go` 类型别名零改动。

**S4：`Handlers` 分域依赖（`pkg/server/deps.go`）**

- 100+ 扁平字段收敛为 **12 个值嵌入分组**：`coreDeps` / `httpDeps` / `authDeps` /
  `filesDeps` / `volumeDeps` / `cloudDeps` / `shareDeps` / `hubDeps` / `clusterDeps` /
  `auditDeps` / `aiDeps` / `opsDeps`。
- **值嵌入 + Go 字段提升**：`h.volSet` 等 580 个方法调用点与全部 `Handlers{...}`
  字面量零改动（Go 1.27 支持在复合字面量中初始化提升字段）。
- 每个分组就是**未来切片的 Deps**：搬 `volume` 切片时把 `volumeDeps` 变成
  `volume.Deps`，字段名即注入点，无需再拆结构体。
- 门禁 **R7**（`handlers_deps_gate_test.go`）：Handlers 直接字段数 ≤ 12、每个字段必须是
  嵌入的 `*Deps` 类型——新依赖必须加到 `deps.go` 的分组，禁止 god object 回归。

**S5：S3 兼容网关（`pkg/server/s3`）**

- 迁入：SigV4 验签、对象/桶读写、multipart（在途 part 双账本 + complete 结算）、列表与 Abort。
- **依赖倒置**：`s3.Deps` 注入 `Volumes/Credentials/TenantFor/VolumePoolForTenant/QuotaScopeFor/
  FileService/Meta/WithAuditVolume`；`MetaPolicy` 是 sidecar 策略的窄接口（装配层用
  `filesMetaPolicy` 适配），网关不反向依赖 `pkg/server`（R6）。
- 共享词汇上移：`ChunkPrefix`/`PartSuffix` → `pkg/storage`；用户可见读写直接调 `pkg/rootfs`
  （原 `pkg/server` 薄包装不再需要）；HMAC 走 `pkg/cryptox`。
- 装配：`pkg/server/s3_wiring.go` 的 `h.s3Service()` 构造 Deps；路由注册 `s3Service().Handler`。
- 白盒单测（part 账本）随迁 `pkg/server/s3`；HTTP 集成测试保留在装配层（经公开入口）。

**S6：服务端 WebDAV 挂载面（`pkg/server/dav`）**

- 迁入：`/dav/` 挂载（多卷 `?volume=` + 默认卷 ACL 同口径）、`davMetaFS` 装饰器
  （写/删/改名后 sidecar 联动、目录 MOVE 整棵 meta 子树迁移）。
- **依赖倒置**：`dav.Deps` 注入 `Volumes/TenantFor/DefaultVolumeAllows/FileService/
  Meta/QuotaScopeFor/WithAuditVolume/OwnerFromRequest`；`MetaPolicy`（`WriteMeta` +
  `SweepMetaTmp`）由装配层 `filesMetaPolicy` 适配，装饰器不反向依赖装配层（R6）。
- 装配：`pkg/server/dav_wiring.go` 的 `h.davService()`；路由 `authMiddleware(davService().Handler)`。
- 白盒装饰器测试随迁 `pkg/server/dav`（`MetaPolicy` 用测试替身）；HTTP e2e 保留装配层。

**S7：AI 域（`pkg/server/ai`）**

- 迁入：文件洞察（`AIInsight` + 缓存）、告警建议器（`AIAdvisor`）、AI 配额（`AIQuota`）、
  AI 派生数据隐私（`AIPrivacy`）、事件消费端（`AIEventConsumer`）。
- **依赖倒置**：事件源用 `ai.EventSource`（装配层把 `EventBus` 快照转成 `ai.FileEvent`），
  AI 域不反向依赖装配层（R6）。
- HTTP 面（summarize/tag/privacy/quota）保留装配层，经 `ai_alias.go` 别名与
  `AIInsight` 的 `Enabled/Cache/Quota/Advise` 访问器调用。
- 白盒缓存测试随迁 `pkg/server/ai`；handler/事件测试保留装配层（迁移期别名）。

**S8：分享存储（`pkg/server/share`）**

- 迁入：\`Store\`（进程内 + 落盘）、\`StateBackedStore\`（集群 StateStore 后端）、\`Link\`/\`Spec\`/
  \`StoreIface\`、持久化格式与过期/容量淘汰。
- 装配层保留 **HTTP 面**（create/access/list/revoke），经 \`share_alias.go\` 别名调用；
  分享域常量（\`MaxShareTTL\`/\`MaxShareEntries\`/\`MaxShareBodySize\`）与存储同源。
- 白盒持久化测试随迁 \`pkg/server/share\`；HTTP/e2e 测试保留装配层。

**S9：运行时指标注册表（`pkg/server/metrics`）**

- 迁入：`Metrics` 注册表（计数器/直方图/标签组）、`XferMetrics`/`XferConnMetrics`、
  `RebalanceProgress`、渲染辅助（`WriteMetric`/`WriteLabeledCounter`/`WriteHistogram`/
  `WriteGaugeSamples`/`WriteReadLoopBlock`/`WriteHybridMetrics`/`WriteGCIDMetrics`）、`RenderSLOMetrics`。
- `/metrics` HTTP 面、中间件与需要装配态的渲染段（卷水位/索引缓存/hub 节点）留在装配层，
  经 `metrics_alias.go` 调用。

**S10：checksum 巡检（`pkg/server/verify`）**

- 迁入：`VerifyOnce`/`VerifyPass`（全租户/单卷、单飞防重入）、台账分批遍历、坏文件隔离、`Report`/`Mismatch`。
- **依赖倒置**：`verify.Deps` 注入 `TenantFor`/`ListTenantIDs`/`LocateOwnerFile`/`ChecksumStoreFor`/
  `Alert`/`Audit`/`Busy`/`Hook`；装配层保留 `POST /api/verify` HTTP 面与调度注册。
- **单一事实源**：新增 `checksum.File(root, rel)`（用户可见摘要），`pkg/files` 与 `pkg/server` 的
  `*ChecksumRoot` 包装均委托它；漂移门禁改为「委托契约」断言。

**S11：用量统计（`pkg/server/ops`）**

- 迁入：`du` 递归统计（`WalkRoot`/`WalkFS`）、存储分类遍历（`WalkUploadStats`/
  `WalkUploadStatsByCategory`）、`CategoriesFromBuckets`/`BucketOf`/`IsStorageBucket`/`QuotaStatusOf`、
  stats 响应类型。
- **纯遍历**：logger 经参数注入，不持有装配状态；`GET /api/du`、`GET /api/stats` 的 HTTP 面与
  `statsRootFor`（租户/卷根解析）留在装配层。

**S12：阈值告警引擎（`pkg/server/alerts`）**

- 迁入：`Engine`（规则匹配 + FIRING/OK 状态机去抖 + 磁盘/配额水位轮询 + 事件驱动
  `OnVolumeDegraded`/`OnSyncFailed`/`OnLoginLocked`/`OnNATFailure`/`OnChecksumMismatch`）、
  `NewFromConfig`、`AdoptNotifyChannels`、AI 建议器挂点。
- **依赖已收敛**：只依赖已下沉的 `pkg/server/config`、`pkg/server/notify`、`pkg/server/ai`，无反向依赖。

**S13：归档打包助手（`pkg/server/archive`）**

- 迁入：`AddFileToTar`（递归打包、符号链接拒绝、Lstat↔open TOCTOU 交叉验证）、`DefaultMaxArchiveSize`。
- **删除 `pkg/server/user_visible_io.go`**：它只是 `pkg/rootfs` 的 6 个纯委托（收敛原则 #11 禁止薄封装），
  39 处调用点直接改为 `rootfs.*`。

### 剩余三块为何不再按切片下沉（2026-10-30 实测）

`cloud`（2.2k）、`volume`（6.5k）、`files`（3.9k）与已下沉的 13 片**形态不同**，实测判据：

| 维度 | 已下沉叶子包 | cloud / volume / files |
|---|---|---|
| 装配依赖（`h.*`） | 0–8 个 | cloud ≈ 30 个；volume/files 更高 |
| 依赖性质 | 纯数据/纯遍历/单一策略 | 配额预留与结算、卷路由、写策略、审计编排 |
| 抽出后 | 依赖经窄接口注入，耦合下降 | 需在 `Deps` 里复刻 `Handlers` 能力面 ⇒ **耦合不降反升** |

结论：这三块**就是装配核心本身**（HTTP 编排 + 跨域结算）。真正要降低其维护成本，前置是先把
「用户可见写策略（`pkg/files`）」「卷容量/配额记账（`pkg/volume/capacity`）」的边界定清，再谈搬运；
在那之前按切片外移只会新增一层 `Deps` 翻译层，与收敛原则相悖。**已完成的 13 片（装配包
42,568 → 32,849 行，−22.8%）到此为自然边界。**

### 新增 Server 切片的步骤

1. 建 `pkg/server/<cap>`，包内只依赖 Global 与同层 Server 包；
2. 在 `internal/archcheck/layers.go` 登记 `Levels`（层级按**实测依赖**取 max+1）与
   `ParentDomain`（父域 `pkg/server`）；依赖了此前未登记的 Global 包时一并登记；
3. 装配根通过构造参数注入依赖，**不**让切片反向 import `pkg/server`；
4. 搬移对应测试；跨域测试改为经 `Register`/构造入口做黑盒；
5. 更新按路径扫描的门禁清单（R38 路由文件、R25/R26 白名单、R43 目录等）。
