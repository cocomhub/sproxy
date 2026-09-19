<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SmartDial 多路径竞速择优（自动选最佳路由）设计

日期：2026-09-19
状态：设计定稿（用户已确认方案 A 竞速 + 多 node 中转 + 可扩展性）
作者：pi agent（suixibing 需求）

## 1. 背景与问题

`build/lab/DEPLOY-GUIDE.md` 描述的四节点组网（新加坡 A hub / 新加坡 B / 办公室
WSL / 家用 mac-mini）依赖**手动选路**：

- `mesh.Dial`（`pkg/tunnel/mesh/mesh.go`）只做 **固定顺序选路**：webrtc 打洞优先，
  失败回落 hub 中继（单一回退）；
- `socks --exit` 只能指定**单个**出口节点，无自动择优；
- 部署指南 §5.3 用「多 socks 出口 + SwitchyOmega 代理切换工具」手动等效实现
  「请求自选最佳链路」，§7-6 要「实测后固定走最优」，§9 明言局限「没有多路径
  实时择优：建立时选路 + 断开重选」。

用户需求：**自动选最佳路由**（产品功能层），度量 = RTT 时延优先，切换粒度 =
**新建连接时择优**（断线重连自动换路，活跃连接不迁移）。且关键补充：**多 node
场景**——候选路径不只是「直连 vs 中继」两选一，而是含「经另一个 node 到目标
node」的多跳路径，择优依据是**端到端 RTT**（如家用→新加坡直连抖动 90-400ms，
但家用→办公室（快）→新加坡（42ms 稳定）端到端可能更优）。

## 2. 目标

1. **自动选最佳路由**：mesh connect / socks 建立连接时，在全部可达路径中按
   端到端 RTT 择优（而非固定顺序）。
2. **多 node 中转纳入候选**：中间节点 X 出站拨号到目标 T 的多跳路径参与竞速，
   端到端 RTT 最短路胜出。
3. **可扩展**：路径提供者用 `plugin.Registry[T]` 注册表（与 `xfer.Register`、
   `plugin.Registry` 同构），未来新增路径（QUIC 直连、WebSocket 中继等）只
   `Register` 新 PathProvider，不改竞速核心。
4. **零回归**：`--smart` 默认关闭 = 现有固定顺序行为；开启才走竞速择优。

## 3. 路径空间（候选路径集合）

目标 `MeshService{Node: T, Addr: A}` 的可达路径：

| 路径 | 描述 | 能力 | 中间节点可发现性 |
|---|---|---|---|
| P1 直连 | 本地 → T（webrtc 打洞，数据面不经 hub） | 现有 `DialWebRTC` | — |
| P2 中继 | 本地 → hub → T（hub 中继流） | 现有 `svc.RelayStream` | — |
| P3 网关 | 本地 → G（本地 mesh node 网关）复用 G→T 已建链路 | 现有 `GatewayConnect` | —（仅 --gateway 存在时启用） |
| P4..Pn 多跳 | 本地 → X（直连或中继）→ X→T（X 出站拨号转发） | #378 多跳 Forward + relay 出口 | **新增 `CapabilityOutboundDial`** |

**新增 capability 常量**（`pkg/tunnel/hub/router.go`）：

```go
const CapabilityOutboundDial = "outbound-dial" // 节点可作为中转出口（relay start --dial-allow 时声明）
```

- `cmd/sclient/relay.go` 的 `runRelayOnce` 在 `dialAllow == true` 时注册帧携带
  `hub.CapabilityOutboundDial`（现有 `NewRegisterFrame(..., hub.CapabilityPerNodeSecret,
  hub.CapabilityVirtualIP)` 追加一项）。
- SmartDial 中间节点候选 = `ListHubNodes()` 在线节点 ∩ 声明 `CapabilityOutboundDial`
  （fail-closed：不对非出口节点发拨号帧，避免被拒）。

## 4. 可扩展性：路径提供者注册表

`pkg/tunnel/mesh/smart.go` 新增（复用 `pkg/plugin/registry.go` 的 `Registry[T]`）：

```go
// PathProvider 是 SmartDial 的一条候选路径实现（P1..Pn 各实现一个）。
type PathProvider interface {
    Name() string                                   // "direct" / "relay" / "gateway" / "via-node"
    Dial(ctx context.Context, svc *client.FileClient,
         signaler webrtc.Signaler, target *client.MeshService,
         localNode string, opts DialOptions) (Result, error)
    Priority() int                                  // 高者优先（竞速排序）
    Enabled(ctx context.Context, svc *client.FileClient) bool // 条件启用
}

// SmartPathRegistry 是 SmartDial 的路径提供者注册表。
// builtin: direct(P1) + relay(P2)；外部插件可 Register 覆盖/新增（gateway(P3)、via-node(P4)）。
var SmartPathRegistry = plugin.New[PathProvider]("smart-path", builtinProviders())

// builtinProviders 返回内置兜底（direct + relay），对应现有 mesh.Dial 固定顺序。
func builtinProviders() PathProvider { ... }
```

- `plugin.New[T]` 已支持 `Active()`（最高优先级单个）/ `Get(name)` / `Names()`；
  竞速需要**全部已注册且 Enabled 的提供者**，故用 `Names()` 遍历 + `Get(name)`
  取实例（注册表内部按注册顺序返回，竞速排序用 Priority）。
- 未来新增路径：`plugin.Register` 一个实现 `PathProvider` 的插件即可，竞速核心
  不动。

## 5. 竞速 + RTT 度量

`pkg/tunnel/mesh/smart.go` 核心：

```go
// DialSmart 是 SmartDial 入口：缓存命中走缓存路径（单路），miss/过期并行竞速。
func DialSmart(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
               target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
```

**胜者缓存**（包级，`sync.Mutex` 保护）：

```go
type winnerCacheEntry struct {
    Kind     string
    Latency  time.Duration
    ExpireAt time.Time
}
var smartCache = struct {
    mu sync.Mutex
    m  map[string]winnerCacheEntry // key = target.Node
}{m: make(map[string]winnerCacheEntry)}
```

- key = 目标 node（路径依赖目标不变；P4 多跳的中间节点选择也随缓存，TTL 过期
  重新竞速时会重新选 X）。
- TTL 默认 30s（抖动链路自适应：过期自动重新竞速，链路变化自动切路）。
- 缓存命中 → 用缓存路径的单提供者 Dial（不竞速，干净）。
- miss/过期 → **并行竞速**：
  - 候选 = `SmartPathRegistry` 中所有 `Enabled()` 的提供者，按 `Priority()` 降序，
    总候选数 ≤ 4（P4..Pn 最多 3 个中间节点 + P1/P2 至少 2 个；超限截断）。
  - 每条提供者 goroutine 并发 Dial，记录**建连耗时**（发起 → 拨号 ack 首字节
    可读；含打洞/中继/多跳各段网络往返，端到端 RTT 务实近似）。
  - 竞速窗口 = min(所有路径建立超时, RaceWindow 默认 5s)。
  - 首条成功者胜出 → 其余关闭（优雅，ctx 感知清场）→ 胜者 `Result` 写缓存
    （含 `Latency`）。
  - 全失败 → 返回最先失败的可操作错误。

**Result 扩展**（`pkg/tunnel/mesh/mesh.go`）：

```go
type Result struct {
    Conn net.Conn
    Kind string
    Latency time.Duration // 建连耗时（端到端 RTT 近似）；SmartDial 填，单路径 Dial 为 0
}
```

## 6. 开关与回归

- `mesh connect` / `socks` 加 `--smart` 布尔 flag（默认 `false` = 现有行为）。
- `--smart=false`：dial = 现有 `mesh.Dial`（webrtc → relay 固定顺序），**零回归**。
- `--smart=true`：dial = `mesh.DialSmart`。
  - P1/P2 必选（builtin 提供者）；
  - P3 仅 `--gateway` 存在时 `Enabled()`；
  - P4..Pn 仅 hub 列表有声明 `CapabilityOutboundDial` 的在线节点时 `Enabled()`
    （候选中间节点最多 3 个，按 ListHubNodes 顺序截断）。

装配点（`cmd/sclient/mesh.go` / `socks.go` 的 dial 组装）：

```go
dial := meshDialFunc(mesh.Dial)
if smart { dial = meshDialFunc(mesh.DialSmart) }
if gatewayAddr != "" { dial = meshGatewayDial(gatewayAddr, svc.AccessKeySecret(), ios) }
if isVIP { dial = meshVIPDial(vipTable, vipSubnet, dial, ios) }
```

（保持现有 gateway/vip 包装顺序：vip 解析最外，smart 在最内层替代 mesh.Dial。）

## 7. 错误处理

- 全路径失败 → 返回最先失败的可操作错误（`fmt.Errorf("smart dial 全部候选失败: %v（直连: %v, 中继: %v, 经节点X: %v）", ...)` 聚合上下文）。
- 部分成功 → 首胜者确定后立即关闭其余在途候选（defer + ctx cancel 清场，不泄漏
  webrtc PeerConnection / relay 流）。
- ctx 取消（用户中断/命令超时）→ 竞速中断，关闭所有在途候选，返回 ctx.Err()。
- `--smart` 下 P1 失败但 P4 成功 → **仍选 P4**（多跳价值核心场景，不做「直连优先」
  主观排序——一切以端到端 RTT 为准）。

## 8. 测试（TDD 红灯先行）

`pkg/tunnel/mesh/smart_test.go`（注入 mock 路径拨号器模拟各路径不同延迟；
纯标准库 + 127.0.0.1）：

| # | 用例 | 预期 |
|---|---|---|
| 1 | 直连慢/中继快 | 选中继 |
| 2 | 直连快 | 选直连 |
| 3 | **多跳：home→office(快)→B vs home→B(慢)** | **选多跳**（端到端 RTT 最短路胜出） |
| 4 | 缓存命中走缓存路径 | 单路（不竞速） |
| 5 | TTL 过期重新竞速 | 链路变化自动切路 |
| 6 | 全失败错误上下文 | 可操作错误聚合 |
| 7 | -race 并发安全 | 缓存并发读写无竞态 |
| 8 | **可扩展性：Register 新 PathProvider → 自动参与竞速** | 插件模式验证 |

测试基建：mock 路径拨号器 = 每个 PathProvider 可注入不同延迟（如 10ms/100ms/
500ms）的 Dial 函数；竞速窗口短（如 200ms）避免测试慢。

## 9. 部署指南更新

`build/lab/DEPLOY-GUIDE.md`：

- §5.3 多链路手动切换 → 自动选路：`mesh connect --smart` / `socks --smart`，
  删除 SwitchyOmega 手动切换段落（说明自动竞速 + TTL 自适应）。
- §7-6 链路择优 → 自动：改为「--smart 开启即自动择优，无需实测固定」。
- §9 局限 → 更新：多路径实时择优已由 SmartDial 提供（建立时 + TTL 过期重竞速；
  活跃连接不迁移仍为局限，保留说明）。

## 10. 范围与不做

- **不做活跃连接实时迁移**（用户已选「新建连接时择优」；连接迁移需 mux 层
  重绑定协议，属未来增强）。
- **不做吞吐/丢包加权评分**（用户已选 RTT 时延优先；综合评分作未来演进）。
- **不改 mux 核心**（RTT 度量用建连耗时近似，不新增 mux Ping 往返接口——避免
  核心传输层回归风险）。
- **不做服务端多路径聚合**（SmartDial 纯客户端选路，服务端无改动——仅 relay
  声明新增 capability 标记）。
