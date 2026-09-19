<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 经目标节点出口的正向 HTTP 代理（http-proxy）设计

> 状态：设计草案（分支内过程产物，合并前并入权威文档后删除）
> 日期：2026-09-20
> 关联：`pkg/socks5`（对称库）、`pkg/tunnel/mesh`（SmartDial 竞速）、`cmd/sclient/socks.go`（骨架复用）

## 1. 背景与目标

sproxy mesh 已支持「经目标节点出口」访问（`mesh connect` / `socks` / `udp map` / `relay dial`），
但面向「请求外网页面 + 下载资源」的能力现状：

| 形态 | 现状 | 局限 |
|------|------|------|
| `sclient tunnel <url>` | 单次 HTTP 请求隧道 | 非代理形态，其他程序无法复用 |
| `sclient socks` | SOCKS5 代理 | 浏览器/Git/Go 应用对 `http_proxy` 环境变量的原生支持远广于 socks5（curl 需 `--socks5-hostname`，Git/Go 原生只认 http 代理） |
| `cloud-download` | 服务端异步下载 | 下载落**服务端存储**，不解决「经目标出口访问」语义 |

**目标**：
1. 经指定目标节点（`--exit`）出口请求外网页面 / 下载资源；
2. **sclient 自己能访问**：本地起代理端口，任意程序（curl/wget/浏览器/Git/Go·Python·Node 应用）配 `http_proxy`/`https_proxy`/`no_proxy` 即用；
3. **提供能力让其他服务访问**：`pkg/httpproxy` 嵌入库（Go 服务 import，注入任意 Dial），与 `pkg/socks5` 对称；
4. 网络良好时**本地直连优先**，网络差（被墙/超时）自动走目标节点出口——提前设计演进，避免返工；
5. 探测出的既有能力文档缺口同步补齐（保证权威文档正确有效）。

## 2. 方案对比与选择

| 方案 | 说明 | 优点 | 缺点 | 结论 |
|------|------|------|------|------|
| **A. 本地 HTTP 代理端口**（`sclient http-proxy`） | 标准正向代理，绝对 URI + CONNECT | 程序零改动（http_proxy 环境变量）、跨平台、HTTPS 走 CONNECT 端到端 TLS | 需每程序配代理（环境变量/配置） | **采纳（首期）** |
| **B. 透明代理**（iptables 劫持 80/443） | 零配置，进程无感 | 无需程序配代理 | **仅 Linux + root**；HTTPS 需按 SNI 推断目标（复杂）；跨平台否决 | 不采纳（YAGNI，见 §9） |
| **C. pkg 嵌入库**（`pkg/httpproxy`） | Go 服务 import，注入 Dial | 与 pkg/socks5 对称、可独立单测、宿主可自定义路由 | 需宿主集成 | **采纳（与 A 同构一并交付）** |
| **D. 服务端侧增强**（cloud-download / tunnel 外部转发经出口） | 服务端下载/请求经目标节点出口 | 闭环「服务端能力」 | 涉及 pkg/tunnel 核心协议装配 | 二期（接口预留，见 §8） |

首期范围 = **A + C**（用户已确认）。B/D 只作演进预留，不实现。

## 3. 架构总览

```
本机客户端程序（curl/wget/浏览器/Git/Go·Python·Node 应用）
  │  http_proxy=http://127.0.0.1:1080  https_proxy=同  no_proxy=本地不走代理
  ▼
sclient http-proxy -l :1080 [--exit <node>] [--proxy-user u --proxy-pass p] [--exit-only]
  │  pkg/httpproxy.Server（新包，纯协议实现）
  │    ├─ 绝对 URI 请求（HTTP）→ 转发目标
  │    ├─ CONNECT host:443（HTTPS）→ 隧道 + 双向泵送
  │    └─ Proxy-Authorization Basic 认证（配置了才要求）→ 未认证 407
  │  Dial 注入（Config.Dial，与 pkg/socks5 同模式）
  ▼
mesh.NewLocalOrExitDial(localTimeout, exitDial)          ← 路由在 mesh 层组装
  ├─ 路径 1「本地直连」：net.Dialer 直连外网目标（网络好，零 mesh 开销）
  │    短超时（默认 3s）失败/超时（被墙/网络差）→ 回退路径 2
  └─ 路径 2「经出口」：既有 mesh dial（webrtc 直连 / hub 中继 / --smart 竞速）
        → 出口节点 mesh node --dial-allow（既有）→ 出口拨号策略把关 → 目标
```

### 组件职责（边界清晰，独立可测）

| 组件 | 落点 | 职责 | 依赖 |
|------|------|------|------|
| `pkg/httpproxy` | 新包 `pkg/httpproxy/` | HTTP 代理协议（绝对 URI / CONNECT / Basic 认证 / hop-by-hop 剥离）；**无路由逻辑** | stdlib + `pkg/iostream`（Pump/NormalizeListenAddr）+ `pkg/slogutil`（可选） |
| `NewLocalOrExitDial` | `pkg/tunnel/mesh`（新增） | 组装路由：本地直连优先（有界超时）→ 回退出口 mesh dial；**不感知 HTTP** | stdlib + mesh dial |
| `NewAutoExitDial` | `pkg/tunnel/mesh`（新增） | 自动选出口节点（`outbound-dial` 能力优先，`exclude` 排除名单，候选 failover） | 同上 + `client.ListHubNodes` |
| `cmd/sclient/internal/meshconn/`（新 internal 包） | sclient CLI | **flag 注册 + Dial 构造统一收敛**：socks/udp map/mesh connect/http-proxy 四命令共享同一套 mesh 连接参数组与装配（内部包，main 包只留薄命令层） | `pkg/cli` / `pkg/iostream` / mesh / `internal/clientfactory` |
| `cmd/sclient/http_proxy.go` | sclient CLI | http-proxy 命令（复用 meshconn 装配） | 上述 |

> 分层纪律：`pkg/httpproxy` 不 import `pkg/tunnel/mesh`（R1 分层，与 `pkg/socks5` 一致——socks5 也只依赖注入的 DialFunc）；路由在 mesh 层组装（`NewLocalOrExitDial` 返回 `httpproxy.DialFunc`）。

## 4. 协议处理细节（pkg/httpproxy）

与 `pkg/socks5` 对称的 API：

```go
// DialFunc 建立到目标 host:port 的 TCP 连接。由调用方实现传输路由
// （mesh 出口场景：经 mesh 到对端出口，对端按 dial 帧出站拨号；
//  本地直连场景：回退 net.Dialer，见 §5）。
type DialFunc func(ctx context.Context, addr string) (net.Conn, error)

type Config struct {
    Dial DialFunc                       // nil 回退 net.Dialer 直连（本机出口语义）
    Auth func(user, pass string) bool   // Proxy-Authorization Basic 校验；nil = 无认证
    Logger *slog.Logger                 // nil 用 slog.Default()
}
func New(cfg Config) *Server
func (s *Server) Serve(ctx context.Context, ln net.Listener) error
```

### 请求分派

| 请求形态 | 处理 |
|----------|------|
| `GET http://host/path`（绝对 URI，RFC 7230 §5.3.2） | 目标 = req.URL.Host；转发（流式 body 双向，不缓冲）；剥离 hop-by-hop 头后原样透传响应 |
| `CONNECT host:443`（RFC 7231 §4.3.6） | Dial 目标 → `200 Connection Established` → `iostream.Pump` 双向泵送（半关闭 + grace 宽限期） |
| 非上述形态（origin-form 无绝对 URI、未知方法） | 405 / 400（显式错误，不静默） |

### hop-by-hop 头剥离（转发时）

`Proxy-Authorization`、`Proxy-Connection`、`Connection` 及 `Connection` 头声明的字段——
只转发 `X-Forwarded-For`（追加客户端地址，透传语义），其余原样。

### 认证（配置了才要求，对齐 socks 约定）

- `--proxy-user`/`--proxy-pass` 任一配置即启用 `Proxy-Authorization: Basic` 校验（防只配密码被静默禁用）；
- 未认证/错误 → `407 Proxy Authentication Required` + `Proxy-Authenticate: Basic realm="sproxy"`；
- 校验用恒时比较（对齐 `pkg/socks5` 的 `subtle.ConstantTimeCompare` 模式）。

### 防环回（关键实现细节）

代理**转发**用的 `http.Client` 必须显式 `Transport.Proxy = nil`：
Go 默认 Transport 读 `http.ProxyFromEnvironment`，若不关掉，代理自身出站会再去走系统代理
（`http_proxy` 指向自己）形成环回。**pkg/httpproxy 内部转发 client 恒设 `Transport.Proxy = nil`**，
出站路由完全由注入的 Dial 决定（也满足「目标由 Dial 决定」的可测性）。

### 错误处理

| 场景 | 响应 |
|------|------|
| Dial 失败（出口不可达/目标拒绝） | `502 Bad Gateway`（CONNECT 阶段错误行 + 关闭） |
| 认证失败 | `407` + `Proxy-Authenticate` |
| 畸形请求 / 未知方法 | `400` / `405` |
| 超时/空闲 | 有界关闭（每连接读头超时 + 泵送 grace，对齐 `relayStreamIdleTimeout` 思路） |

所有错误对客户端**不暴露内部细节**（对齐仓库「handler 不回传原始 error」约定）。

## 5. 路由策略：本地直连优先（`mesh.NewLocalOrExitDial`）

### 语义

- **本地直连**：L 直接拨外网目标（`net.Dialer`），完全不进 mesh——「本机作为出口」的显式选择
  （与 `pkg/socks5` nil-Dial 回退、`mesh node --socks` 本地出口同语义）；**不经出口拨号策略**（本地出口由用户环境自己把关）；
- **经出口**：既有 mesh dial（webrtc 打洞优先 / hub 中继回落 / `--smart` 竞速），目标由出口节点
  `NewServiceDialPolicy` 把关（SSRF 边界）。

### 自动选出口节点（`--exit-auto`，设计预留，首期可选实现）

`--exit` 缺省时**不强制手动指定**：本地直连失败后自动从 mesh 节点中选一个合适出口。

**复用已有能力（已实证）**：
- 候选源：`ListHubNodes` → `GET /api/hub/nodes`（SproxySig 签名数据源，与 mesh connect 的
  vipTable 同源）；出口候选判据 = **`Capabilities` 含 `outbound-dial`**（`hub.CapabilityOutboundDial`）
  ——mesh node 开启 `--dial-allow` 时声明（`RegisterFrame.Meta.Capabilities`），hub `/api/hub/nodes`
  已透出 `Capabilities` 字段（与 `via_node.go` 的中间节点候选同一判据，已实证）；
  设计初稿提的 `Tags: ["exit"]` 是注册帧 Meta.Tags 且**未透出到 hub/nodes**，故改用 Capabilities（更正）；
- failover：候选拨号失败跳过下一个（对齐 P1-13 候选 failover 既有模式）。

**选路语义**（与显式 `--exit` 完全同构，仅节点发现自动化）：

```
--exit-auto（无 --exit 时可选）
  1. 本地直连（有界超时 localTimeout）→ 网络好零 mesh 开销
  2. 本地失败 → 拉 hub 节点列表，候选 = Capabilities 含 "outbound-dial" 的节点
     （排除名单命中跳过；无 outbound-dial 则全部在线节点减排除名单）
  3. 顺序尝试候选（失败跳过下一个）；全部不可达才报错
  4. 目标仍由出口节点拨号策略把关（SSRF 边界不变，信任面不扩大）
```

**实现落点**（增量，不动已设计签名）：

```go
// mesh 包新增：自动选出口节点。nodeLister 注入候选源（生产 = client.ListHubNodes → []client.HubNodeInfo，
// 测试 = 桩）；exitDialFor(nodeID) 构造经该节点的出口拨号闭包；exclude 是**出口候选排除名单**
// （精确 node-id 匹配，命中跳过——这些节点仍可被 SmartDial via-node 选为**中转**中间节点，见下）。
// 签名与 httpproxy.DialFunc 兼容。
func NewAutoExitDial(localTimeout time.Duration,
    nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error),
    exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error),
    exclude []string,
) func(ctx context.Context, addr string) (net.Conn, error)
```

**CLI 形态**（出口指定矩阵，与 §6 合并）：

| CLI 输入 | 路由 |
|----------|------|
| `--exit <node>`（默认） | 本地直连优先 → 回退该出口节点 |
| `--exit <node> --exit-only` | 恒经该出口（不试本地直连） |
| `--exit-auto` | 本地直连优先 → 回退自动选中的出口（`outbound-dial` 能力优先） |
| `--exit-auto --exit-only` | 恒经自动选中的出口 |
| `--exit-auto --exit-exclude <id>[,<id>...]` | 自动选中时跳过排除名单节点（`--exit` 固定节点时忽略） |
| 无 `--exit` / 无 `--exit-auto` | 恒本地直连（等价 `Config.Dial=nil`，与 pkg/socks5 同语义） |

- `--exit` 与 `--exit-auto` 互斥（fail-closed 报错，不静默选一）；
- `--exit-exclude` 是 `--exit-auto` 的**出口候选排除名单**（逗号分隔 node-id，可多次）：
  排除的节点**不作为出口**（不被 `--exit-auto` 选中），但**仍可被 SmartDial via-node 选为
  中转中间节点**（`--smart` 竞速时）——「能中转但不出站」的明确语义；
  `--exit <node>` 固定节点时 `--exit-exclude` 无意义（fail-closed 报错提示）；
- `--exit-only` 无出口候选（无 `--exit` 且无 `--exit-auto`）时 fail-closed 报错提示需要其一；
- 与 `--smart` 正交：`--smart` 是 mesh **建连**竞速（webrtc/中继/via-node），`--exit-auto` 是出口
  **节点选择**自动化——可组合，不冲突；
- 安全边界：候选源是 hub 签名数据（防投毒）；出口侧拨号策略把关不变；auto 只是发现自动化，
  不扩大信任面（所有出口节点都需 `--dial-allow` + 策略放行才能出站）。

### 首期：顺序 + 有界超时

```go
// mesh 包新增（签名兼容二期竞速升级）：
// exit 是「经出口」拨号闭包：调用方（CLI）捕获 svc/signaler/目标解析逻辑，
// 入参 addr（外网 host:port）→ 构造 MeshService{Node: exitNode, Addr: addr} → mesh.Dial。
// 返回签名与 httpproxy.DialFunc 一致（func(ctx, addr) (net.Conn, error)），
// mesh 包不 import pkg/httpproxy（R1 分层），仅靠签名兼容。
func NewLocalOrExitDial(localTimeout time.Duration, exit func(ctx context.Context, addr string) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error)
```

```
NewLocalOrExitDial(localTimeout, exitDial) DialFunc
  1. localTimeout 内本地直连成功 → 返回（网络好零 mesh 开销）
  2. 超时/失败（被墙、网络差、DNS 失败）→ 回退 exitDial（经 --exit 出口）
```

- `localTimeout` 默认 3s（被墙 TCP 黑洞可感知的合理上界；可由 `--local-timeout` 覆盖）；
- 首期不做并行竞速（避免每个请求双倍连接开销 + 竞速状态），顺序语义足够；
- CLI 提供 `--exit-only`：关闭本地直连，强制全部经出口（审计严格环境）；
- `exitDial` 为 nil（未配 `--exit`）时 `NewLocalOrExitDial` 退化为纯本地直连（等价 `Config.Dial = net.Dialer`），
  `localTimeout`/`--exit-only` 均无意义——CLI 层在无 `--exit` 且无 `--exit-only` 时直接注入 `net.Dialer` 即可（实现细节，见 §6）。

### 二期演进（接口预留，签名兼容，避免返工）

对齐 `pkg/tunnel/mesh/smart.go` 的竞速模式（`PathProvider`/`SmartOptions` 的 CacheTTL 思想）：
`NewLocalOrExitDial` **签名不变**，内部升级为并行竞速双候选（local/exit 同时拨，先成功者胜 +
胜者 TTL 缓存；`SmartOptions.CacheTTL` 复用）。之所以**不**把 local 塞进 `SmartPathRegistry`
（`SmartPathRegistry` 面向 `*client.MeshService` 目标，http-proxy 目标是任意外网 host:port，
语义不同，混入会造成候选混淆），而是 mesh 层单独封装——注册表体系留给 mesh 服务连接，
外网目标走独立路由，各自演进互不污染。

`NewLocalOrExitDial` / `NewAutoExitDial` 的竞速升级（并行双候选/多候选 + TTL 缓存）**签名均不变**
（`NewAutoExitDial` 的二期为多候选并行竞速 + 胜者 TTL 缓存，对齐 SmartOptions；
首期可只实现顺序尝试，候选少、语义简单）。

## 6. CLI 设计与连接装配抽象（meshconn）

### 现状重复（实证）

`cmd/sclient/socks.go` / `udp.go` / `mesh.go` 三处**逐行重复**同一套 mesh 连接参数组与装配：

| 重复项 | socks.go | udp.go | mesh.go |
|--------|:--:|:--:|:--:|
| `--hub/--node-id/--insecure` 读值 + svc 回落 | ✓ | ✓ | ✓ |
| `--stun/--turn/--turn-user/--turn-pass` 读值 + T6b 配置回落 + `SetSTUN/SetTURN/SetTURNCredential` + `applyTURNRESTFlags` | ✓ | ✓ | ✓ |
| mDNS 初始化（`NewMDNS` + `Start` + `defer Close`） | ✓ | ✓ | ✓ |
| `AutoRegister` 信令注册（含 ca-file 回落、错误提示、defer Closer） | ✓ | ✓ | ✓ |
| `--gateway` 复用（`GatewayConnect`，ErrNoPeerLink 回落） | ✓ | ✗ | ✓ |
| `--smart`/`--smart-ttl` 竞速分支（`DialSmartWithOptions`/`DialSmartDefault`） | ✓ | ✗ | ✓ |
| `--exit` 出口目标构造（`MeshService{Node: exit, Addr: addr}`） | ✓ | ✓ | ✓（mesh connect 为服务名解析） |

### meshconn 抽象（新包 `cmd/sclient/internal/meshconn/`，本次一并交付）

**目标**：flag 定义 + 连接装配统一收敛，四命令（socks / udp map / mesh connect / http-proxy）
共享同一参数组；**同一连接方式下的后续扩展（如 --exit-auto、新传输、竞速升级）一处修改，所有使用方收益**。

> **落点**：`cmd/sclient/internal/meshconn/`（internal 包，非 main 包）——
> ① 避免 main 包堆积复杂装配逻辑（对齐 `internal/clientfactory`、`internal/contextcfg` 既有模式）；
> ② 复用关系清晰：仅 sclient 内部命令可 import，main 包只留薄命令层（读 flags → 调 meshconn → Serve）；
> ③ 可独立单测（`internal/meshconn` 不依赖 main 包全局，注入桩即可）。

```go
// cmd/sclient/internal/meshconn/meshconn.go

// Flags 是 mesh 连接共用的 flag 集合（--hub/--node-id/--webrtc/--insecure/--ca-file/
// --stun/--turn/--turn-user/--turn-pass/--turn-rest 族/--mdns/--mdns-secret/--gateway/--smart/
// --smart-ttl/--virtual-subnet/--exit/--exit-auto/--exit-only/--local-timeout）。
func AddFlags(cmd *cobra.Command) { ... }

// Conn 是一次命令的 mesh 连接上下文（由 flags + 配置回落装配，socks/udp/mesh/http-proxy 共用）。
// 字段：ExitNode / ExitAuto / ExitOnly / LocalTimeout / GatewayAddr / Smart / SmartTTL /
//       MDNS / MDNSSecret / WebRTC / HubURL / NodeID / Insecure / STUN / TURN...
type Conn struct { ... }

// FromFlags 读 flags + 配置回落（T6b：stun/turn 从 context env 回落；hub/node-id 从 svc 回落；
// mdns-secret 回落 access_key_secret），应用 SetSTUN/SetTURN/SetTURNCredential/applyTURNRESTFlags。
func (c *Conn) FromFlags(cmd *cobra.Command, cfgSvc ConfigProvider) error

// Signalers 装配 mDNS 信令（BrowseOnly）或 hub AutoRegister 信令器（含 ca-file 回落、
// 注册失败回落中继）。返回 signaler + close 闭包（nil 安全）。
func (c *Conn) Signalers(ctx context.Context, svc *client.FileClient) (signaler webrtc.Signaler, closeFn func() error, err error)

// Target 构造拨号目标：--exit 固定节点 → MeshService{Node: exit, Addr: addr}；
// --exit-auto → NewAutoExitDial 的候选解析；服务名模式（mesh connect）→ refresher 解析。
func (c *Conn) Target(addr string) (*client.MeshService, error)

// Dial 构造最终拨号函数（httpproxy.DialFunc / socks5.DialFunc 兼容签名）：
//   出口路径（gateway/smart/webrtc/mdns 全部既有逻辑收敛于此，复用 meshGatewayDial 等）
//   + 本地直连优先（NewLocalOrExitDial）或恒出口（--exit-only）
func (c *Conn) Dial(ctx context.Context, svc *client.FileClient) httpproxy.DialFunc  // 或 func(ctx, addr) (net.Conn, error)
```

**收敛边界**（避免过度抽象）：
- `mesh connect` 的**服务发现/目标解析**（`NewMeshTargetRefresher` / vipTable / mDNS 服务名
  解析）与 mesh connect 专有 flags（`--virtual-subnet` 目标寻址语义）保留在 `mesh.go`，
  不强行并进 MeshConn——MeshConn 只收敛「连接参数组 + 装配」，目标解析语义留给各命令；
- socks/udp/http-proxy 的 `--exit`（固定出口节点）与 mesh connect 的 `--exit` 语义不同
  （前者 = 出口节点，后者 = 服务名）→ MeshConn 提供 `Target(addr)` 抽象，各命令传入自己的目标语义；
- udp map 的 `--remote`（UDP 目标）不是连接参数，留在 udp.go；
- 保留既有命令行为零回归：socks/udp/mesh 首期**行为不变**，只把装配体抽到 meshconn；
  http-proxy 复用同一装配。

### CLI 形态（http-proxy）

```
sclient http-proxy [-l :port] [--exit <node>] [--exit-auto] [--exit-only] [--local-timeout 3s]
                   [--proxy-user u] [--proxy-pass p] [--gateway <addr>] [--smart] [--smart-ttl <dur>]
                   [--mdns] [--mdns-secret <s>] [--webrtc] [--hub <addr>] [--node-id <id>]
                   [--insecure] [--stun ...] [--turn ...] [--turn-user ...] [--turn-pass ...]
```

- 出口指定矩阵（§5）；`--exit` 与 `--exit-auto` 互斥（fail-closed）；
- `--proxy-user`/`--proxy-pass` 任一配置即启用 Basic 认证；
- 监听默认 `127.0.0.1:1080`（`NormalizeListenAddr`，loopback 安全默认；LAN 暴露需显式监听地址）；
- 输出横幅：`HTTP 代理就绪: <addr>（本地直连优先 ⇄ 出口 <node>）`（Ctrl+C 退出）。

## 7. 测试策略

| 层 | 内容 | 约束 |
|----|------|------|
| `pkg/httpproxy` 单测 | 绝对 URI 转发（body/流式/大响应/响应头透传）、CONNECT 隧道（回显/大流量/半关闭）、Basic 认证（成功/407/错误凭据/恒时）、hop-by-hop 剥离、畸形请求/未知方法、**Dial 注入桩验证「目标由 Dial 决定」而非直连**、`Transport.Proxy=nil` 防环回（设 http_proxy 环境变量后请求仍不环回） | 纯 stdlib，`httptest.NewServer`，`t.Parallel()` |
| `NewLocalOrExitDial` 单测 | 本地桩成功（不调 exit）、本地超时→回退 exit、本地失败→回退 exit、exit 失败透传、localTimeout=0 直接 exit | 注入桩，`t.Parallel()` |
| `MeshConn` 单测（`cmd/sclient/internal/meshconn/meshconn_test.go`） | **零回归验证**：socks/udp/mesh/http-proxy 四命令 flag 集合一致（`AddFlags` 注册的每项都能被各命令读取）、FromFlags 回落（stun/turn 配置回落、hub/node-id 回落、mdns-secret 回落）、Dial 构造分支（gateway/smart/exit-only/本地直连） | 纯 stdlib + cobra 内存执行，`t.Parallel()` |
| mesh 集成 | 复用 `pkg/tunnel/mesh/mesh_socks5_test.go` 模式（in-process hub + 出口节点，**不 import pkg/server**，R4 分层门禁）：http-proxy 经出口拉取目标页面 | 127.0.0.1，Windows 兼容 |
| e2e | 真实二进制 + **Go 写的代理客户端**（`http.Transport{Proxy: ...}`，不依赖 curl 存在性）；`http_proxy`/`https_proxy`/`no_proxy` 环境变量行为验证 | `test/e2e_test.go` 模式，loopback |

Windows 铁律：所有监听经 `NormalizeListenAddr` 收敛 loopback，无防火墙弹窗
（对齐 `mesh_socks5_test.go`/`mesh_udp_test.go` 既有约束）。

## 8. 二期演进预留（本次不实现，只留接口口子）

### D-1 服务端 cloud-download 经目标节点出口

现有扩展点（已实证）：
- `pkg/downloader.Registry` 插件机制 + `HTTPDownloader` 可注入整只 `*http.Client`
  （`newHTTPDownloaderWithClient`，含自定义 `Transport`）→ 二期把 `Transport.DialContext`
  指向「经 mesh 到出口节点的连接工厂」即可，**下载器协议零改动**；
- 配置：`CloudDownloadConfig` 加代理字段（对齐 `cloud_downloader` 配置族），
  `pkg/server/routes.go:286` 装配处传入。

### D-2 tunnel 外部转发经目标节点出口

- `pkg/tunnel/handler_client.go` 的 `forwardExternal`（绝对 URL 分支）已存在，
  `NewLocalHandler(key, nil, logger)` 即纯外部转发模式；
- 其 `httpClient` 为内部私有构造（`handler_client.go:45`）→ 二期在 `pkg/tunnel` 加
  `WithHTTPClient` 注入选项（把 `Transport.DialContext` 指向经 mesh 出口的连接工厂），
  使 `sclient tunnel <外部url>` 也能经目标节点出口。

### B 透明代理（明确不做）

仅 Linux + root；HTTPS 目标需 SNI 推断 + 数据面重路由（TUN/iptables），实现复杂且
**跨平台否决**（用户已确认跨平台为硬约束）。`http_proxy` 环境变量覆盖绝大多数程序，
剩余场景（无代理配置能力的程序）为少数——YAGNI，记录不实现。

## 9. 安全边界清单（全部复用既有模式）

1. **监听默认 loopback**（`NormalizeListenAddr`，裸 `:port` → `127.0.0.1:port`）；LAN 暴露需显式监听地址；
2. **认证配置了才要求**（`--proxy-user`/`--proxy-pass` 任一配置即启用 Basic，防只配密码被静默禁用）；
3. **SSRF 边界在出口节点拨号策略**（`NewServiceDialPolicy`：公网 + 白名单 + 宣告地址）；
   本地直连路径 = 本机出口显式选择（用户环境把关），与 socks5 nil-Dial 回退同语义；
4. **防环回**：转发 `http.Client` 恒设 `Transport.Proxy = nil`；
5. **不暴露内部错误**：所有失败映射为 HTTP 状态码，不回传原始 error；
6. **有界资源**：读头超时 + 泵送 grace + 空闲关闭，防半开连接/慢客户端占用；
7. **出口排除不扩大信任面**：`--exit-exclude` 只把指定节点移出 `--exit-auto` 候选（该节点仍可被
   `--smart` via-node 选中作**中转**中间节点——中转 ≠ 出口，二者能力独立：中转仅转发已确立的
   mesh 流，出口是代外部目标出站拨号，出口侧拨号策略把关不变）。

## 10. 权威文档同步清单（与本次设计同收敛）

| 文档 | 内容 |
|------|------|
| `docs/cli.md` | **http-proxy 专节**（新增，含 `--exit`/`--exit-auto`/`--exit-only` 路由矩阵）；`socks` / `udp map` / `mesh connect` / `mesh node` / `mesh status` / `mesh acl` / `cloud-download` 各自独立专节（从「TURN 中继」节拆出成体系，补齐子命令一览） |
| `docs/api.md` | cloud 端点（`/api/cloud/download`、`/batch`、`/tasks`、`/tasks/{id}`、`/cancel`、`/groups`）+ share / versions / archive / hub services·nodes / credentials / stats |
| `docs/config.md` | `cloud_downloader` 配置族（config.example.yaml 已有但权威文档缺失）+ `hub.transports` |
| `docs/architecture.md` | 正向代理数据流（本地直连/经出口双路径） |
| `docs/glossary.md` | 「正向 HTTP 代理」「出口直连」术语 |
| `docs/README.md` | 索引表补 api.md/cli.md 覆盖范围描述 |

> 既有缺口补全（不依赖代码）独立 commit 先行；设计 + 实现同 PR 合并前把 http-proxy 内容
> 并入权威文档（docs-lifecycle 纪律：实现合入前文档收敛，不留过程产物在 master）。

## 11. 交付物与验收

**首期（本设计）**：
- `pkg/httpproxy`（新包）：协议 + 认证 + 剥离 + 防环回，单测全绿；
- `pkg/tunnel/mesh.NewLocalOrExitDial`：本地直连优先路由，签名预留二期竞速升级；
- `cmd/sclient/internal/meshconn/`：mesh 连接参数组注册 + 装配统一收敛（socks/udp/mesh/http-proxy 四命令共享），
  零回归（既有命令行为不变）；后续新连接方式（--exit-auto 等）一处修改所有使用方收益；
- `sclient http-proxy`：CLI 全 flags，e2e 过真实二进制；
- 权威文档同步（§10 清单）。

**验收场景**：
1. `sclient http-proxy -l :1080 --exit <node>` + `curl --proxy http://127.0.0.1:1080 https://example.com` 经出口取回页面；
2. 网络良好时（本地可达目标）直连命中，`--exit` 出口零参与；
3. 网络差时（模拟目标不可达）自动回退出口；
4. 配 `--proxy-user/--proxy-pass` 后未认证请求 407；
5. `pkg/httpproxy` 被 Go 服务 import（注入自定义 Dial）独立可用；
6. `http_proxy`/`https_proxy`/`no_proxy` 环境变量对标准程序开箱即用。

## 12. 非目标（明确排除）

- 不做透明代理（§8-B）；
- 不做服务端 cloud-download/tunnel 经出口（§8-D，二期）；
- 不做 SOCKS5 UDP-ASSOCIATE / BIND（http-proxy 仅 CONNECT + 绝对 URI，与既有 socks5 定位一致）；
- 首期不做并行竞速（顺序 + 有界超时已满足「网络好直连、差走出口」；竞速为二期演进，签名已预留）；
- meshconn 不做过度抽象：mesh connect 的服务发现/目标解析语义、udp map 的 `--remote` 留在各自命令
  （§6 收敛边界）；main 包命令只读 flags → 调 meshconn → Serve，不重复装配逻辑。
