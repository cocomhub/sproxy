---
title: sproxy 完全组网·阶段 4·虚拟 IP / 子网分配设计
status: planning
---

# sproxy 阶段 4·虚拟 IP / 子网分配设计

> 日期：2026-08-30
> 来源：路线图（`docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md` §3 阶段4，P2）+ 阶段 3 复盘（§8 建议：先做子网划分 + 路由表虚拟地址映射，网关层 NAT，再接 mesh 网关）
> 分支：feature/mesh-p4-virtualip（建议）
> 前置：阶段 1–3 已合入（master `7048018`）
> 修订：2026-08-30 v2——设计审查（C-1 端口白名单、I-1 命中顺序、I-2 下发路径、I-3 数据源、I-8 归属、M-1~M-4、R-1）已融入

## 1. 目标

给 mesh 节点提供 **Tailscale/ZeroTier 风格的虚拟 IP 寻址**：

1. 每个 mesh 节点（node-id）在所属 mesh 内获得一个**稳定、唯一**的虚拟 IP；
2. `mesh connect <vip>:<port>` 可直接访问对端节点——语义为「拨号到对端节点**本机**的 `<port>` 服务」，不再局限于按服务名（`mesh connect <name>`）寻址；
3. 本地网关层完成 NAT：发往虚拟 IP 的连接 → 路由到对应节点的已建直连链路 / hub 中继 → 出口节点把虚拟 IP:port 解析为本机服务；
4. 与既有寻址（服务名、`--gateway` peer-link、`--node` relay dial）共存，不破坏现有行为。

**安全收敛（v2，审查 C-1）**：虚拟主机语义必须配**端口白名单**——`vip:port` 只有在对端**显式开放**的端口上可达（见 AD-3），绝不「全端口直通本机」，否则 mesh 节点上所有未宣告的 loopback 服务（网关 18085、SOCKS 出口、agent socket、数据库）会被任意 mesh peer 触达。

**非目标（第一版不做）**：跨 hub 联邦场景的虚拟 IP 全局可达；每 mesh 独立子块划分；虚拟 IP 的 DNS 名解析（预留 `vipTable.Name` 字段占位，R-1）。

## 2. 现状盘点（已核实源码）

| 能力 | 位置 | 与本设计的关系 |
|------|------|----------------|
| 本地网关（loopback） | `pkg/tunnel/mesh/gateway.go` `Gateway` + `GatewayConnect`（:244） | NAT 骨架：已实现「本地端口 → linkPool 已建链路 → 拨号帧 → 对端出口」。`gatewayRequest{Token,Peer,Addr,Status}`（:53），`Addr` **须等于对端宣告服务地址**（出口 `NewServiceDialPolicy` 精确匹配，leaf.go:536） |
| 统一选路函数 | `pkg/tunnel/mesh/mesh.go:132` `mesh.Dial`（webrtc 优先→hub 中继回落）；CLI 侧 `cmd/sclient/mesh.go:30` `meshDialFunc` | **虚拟 IP 映射决策的插入点**：所有 mesh 拨号最终收敛到这里 |
| 链路池 | `pkg/tunnel/mesh/links.go` `linkPool`（peer→mux） | 虚拟 IP 路由的 peer 解析目标 |
| 节点注册权威时机 | `pkg/tunnel/hub/router.go:301` `registerNode` + `MeshRouteTable.Add`（mesh_route_table.go:71） | **分配虚拟 IP 的权威时机**（已拿 node-id/mesh/per-node secret）；`isTransientNodeID`（router.go:356）用于排除瞬态节点 |
| 持久化 | `pkg/tunnel/hub/persist.go` `NodeSnap{ID,Mesh,Addr,Secret,RealNodeID,Connected,Services}` + `MeshRouteTable.onChange` 驱动 | 分配结果落盘，重启不丢 |
| 出口拨号策略 | `pkg/tunnel/relay/leaf.go:529` `NewServiceDialPolicy` + `ServeOptions.DialPolicy` | **出口侧虚拟 IP→本机 NAT 的扩展点**（DialPolicy 闭包） |
| 中继回落 | `pkg/server/relay_stream.go:174` `RelayStreamHandler` + `pkg/client/relay.go:66` `RelayStream` | 打洞失败时经 hub 中继的回落路径 |
| mesh 隔离 | `MeshRouteTable` 按 mesh 分表；`tunnel.AccessKeyMesh` 从 AK 解析 mesh；`MeshOf(id)` | 虚拟 IP 只在所属 mesh 可路由 |
| mDNS 无 hub 模式 | `pkg/tunnel/mesh/direct_signal.go`、`mdns.go`、`mdns_node.go` | 无 hub 时的本地确定性分配回落 |

## 3. 架构决策

### AD-1：分配器抽象 + 双实现（hub 权威 / 本地确定性）

新增分配器接口，**归属定死：接口 + `hubAllocator` 放 `pkg/tunnel/hub`（`vip.go`），`deterministicAllocator` 在 `pkg/tunnel/mesh` 实现 `hub.Allocator`**（v2，审查 I-8：mesh import hub、hub 不 import mesh，接口放 mesh 会成环；放第三个中性包过度设计）。

```go
// pkg/tunnel/hub/vip.go
type Allocator interface {
    // Alloc 为指定 mesh 下的 node-id 分配虚拟 IP（稳定：同一 node-id 复用上次分配）。
    Alloc(mesh, nodeID string) (netip.Addr, error)
    // Release 在节点移除时释放虚拟 IP（回收复用）。
    Release(mesh, nodeID string)
    // Subnet 返回本分配器使用的虚拟子网。
    Subnet() netip.Prefix
}
```

- **hub 权威实现** `hubAllocator`：挂在 `registerNode` 成功后（`HubServer` 持有），从配置子网内**递增分配**、按 `(mesh,nodeID)` 记录并复用；节点移除（`MeshRouteTable.Remove`/`RemoveIfOwned` → `onRemove` 回调）时回收。**瞬态节点过滤（v2，审查 I-2）**：`registerNode` 内用 `isTransientNodeID`（router.go:356）跳过 `disc-*`/`mesh-*`/`p2p-*` 临时身份，避免每次 `mesh connect` 分配/释放 VIP、vipTable 出现濒死条目。分配结果写入 `NodeInfo.VirtualIP`，随 `persist.go` 落盘；**重启从快照重建分配表**（`RestoreFromSnapshot` 把已分配的 `(mesh,nodeID)→VIP` 灌回 allocator，避免把已持久化的 VIP 再分给新节点）。
- **本地确定性实现** `deterministicAllocator`：`vip = subnetBase + hash(mesh, nodeID) mod (subnetHosts-2) + 2`，无状态、无 hub 可用。用于 mDNS 无 hub 模式（`runNodeMDNSOnly`）。冲突概率低（默认 /10 宿主量大），碰撞检测为后续增强。**仅 IPv4（v2，审查 M-3）**：`Validate` 限制 `virtual_subnet` 必须是 IPv4 CIDR（确定性分配做 IPv4 算术）。
- **选型**：`mesh node` / `relay` 有 hub 时用 hub 分配的虚拟 IP；无 hub 纯 mDNS 模式回落确定性分配。
- **自身 VIP 下发不依赖 discovery 环（v2，审查 I-2）**：VIP 随注册 ACK 直接下发——`REG_OK` 扩展新能力位（如 `virtual-ip`），hub 在 `REG_OK:<secret>:<vip>` 下发本节点 VIP；任何注册节点**立即**得知自身 VIP，不受 `Discover=false` 或 `relay start` 出口节点（不拉 `/api/hub/nodes`）影响。避免「selfVIP 恒零值 → 对它的虚拟 IP 拨号全部 fail-closed 拒绝」的静默失效。

### AD-2：默认子网 CGNAT `100.64.0.0/10`，可配置，仅 IPv4

- 默认 `100.64.0.0/10`（RFC 6598，Tailscale 同款，不与常见私网 `10/8`、`172.16/12`、`192.168/16` 冲突）。
- 配置项：`hub.virtual_subnet`（服务端）/ `mesh node --virtual-subnet`（客户端，mDNS 模式本地用）。`Validate` 校验 CIDR 合法 + **IPv4 限定**（v2）。
- **第一版不按 mesh 划分子块**：所有 mesh 共享配置子网，虚拟 IP 全局唯一由 hub 保证；mesh 隔离仍由 `MeshRouteTable` 路由面保证。子网首地址（`100.64.0.1`）保留为网关/默认，实际分配从 `.2` 起。
- 重叠风险提示：CGNAT 段可能与运营商/容器网络真实地址重叠；**命中顺序保证逃生口（v2，审查 I-1）**——出口拨号策略**先做 `ServiceAddrs` 精确匹配，再判虚拟子网**，故宣告在虚拟子网内的真实服务地址仍可访问（见 AD-3 与 §4.3）。

### AD-3：虚拟 IP 语义 = 「虚拟主机」+ **端口白名单**（v2，审查 C-1 修正）

`mesh connect <vip>:<port>` 等价于「在出口节点上 `connect 127.0.0.1:<port>`」。虚拟 IP 代表**节点本身**，但节点**只开放显式声明的端口**，不是全端口。

- 出口侧 `DialPolicy`（虚拟 IP 分支）：
  1. 目标 host ∈ 虚拟子网 **且端口 ∈ 本节点开放端口集合**（= `ServiceAddrs` 解析到 loopback/本机端口的集合 ∪ 显式 `--vip-allow-port` 声明的端口）→ 改写拨号目标为 `127.0.0.1:<port>`（本机服务）→ 放行；
  2. 目标 ∈ 虚拟子网但 **端口不在白名单** → 拒绝（防未宣告服务被 mesh 触达）；
  3. 目标 ∈ 虚拟子网但 != 本节点虚拟 IP → **拒绝**（虚拟 IP 属于其他节点，防 SSRF/地址劫持）；
  4. 目标不在虚拟子网 → 回落现有 `NewServiceDialPolicy` 精确匹配/CIDR 逻辑（**虚拟子网判断在宣告地址精确匹配之后**，保证真实 CGNAT 流量不被遮蔽）。
- 安全前提：虚拟 IP 拨号同样受 `--dial-allow`（出口模式）门控——未开启出口模式的节点收到 dial 帧仍被拒（沿用 leaf.go:132-139）。
- **效果**：`mesh connect <vip>:22` 在宣告了 `--service ssh:127.0.0.1:22`（或 `--vip-allow-port 22`）的节点上可用；未宣告的 18085（网关）/SOCKS/agent socket 端口不可达（C-1 闭环）。

### AD-4：虚拟 IP 表（vipTable）的数据源与防注入（v2，审查 I-3）

- `vipTable: map[netip.Addr]string`（虚拟 IP → peer node-id），常驻 mesh node 与一次性 CLI 各持一份，无跨进程一致性要求（M-8）。
- **常驻 node 数据源**：`runDiscoveryLoop` 拉 `/api/hub/nodes` 时填充（列表带 `virtual_ip`）；mDNS 模式用 TXT 记录 `vip=<addr>`（签名 TXT 用 `--mdns-secret` HMAC）。
- **一次性 CLI `mesh connect <vip>:<port>` 数据源（v2，审查 I-3）**：新增 `pkg/client` 方法拉 `/api/hub/nodes`（SproxySig 签名，返回带 `virtual_ip` 的节点列表），CLI 据此构建 vipTable；`mesh.Dial` 内部不做未知 VIP 猜测。**`ListHubNodes`（discovery.go:92）返回类型扩展为带 `virtual_ip` 的结构**（这是必须改的一处接口）。
- **防注入**：映射只接受来自 hub 认证列表（SproxySig 签名 `/api/hub/nodes`）或 mDNS 签名 TXT；客户端不信任任意来源的映射。`vipTable` 预留 `Name` 字段占位（R-1）。

### AD-5：CLI 形态（兼容 + 新增）

| 命令 | 变更 |
|------|------|
| `mesh connect <vip>:<port>` | 新增寻址形态：参数 host 解析为 netip.Addr 且 ∈ 虚拟子网时，按虚拟 IP 路由（`vipDialFunc` 包装 `meshDialFunc`，先查 vipTable→node-id，再回落 `mesh.Dial`/`GatewayConnect`）；否则维持服务名寻址 |
| `mesh status` | 节点列表显示 `virtual_ip`（**走 `/api/hub/nodes` 或扩展 `/api/hub/services` 响应**，v2 审查 M-2） |
| `mesh node --virtual-subnet <cidr>` | 可选：覆盖默认子网（mDNS 模式用；有 hub 时以 hub 分配为准） |
| `mesh node --vip-allow-port <port>` | 可选：虚拟 IP 开放端口白名单（重复可多次）；缺省 = `--service` 宣告端口自动开放 |
| `relay start` / `p2p listen` 出口 | `DialPolicy` 自动包虚拟 IP 规则（若配置了虚拟 IP） |
| `relay dial --node <id> --tcp <vip>:<port>` | 出口侧同样识别虚拟 IP→本机（可选增强，第一版聚焦 mesh connect） |

### AD-6：数据通路（端到端）

```
mesh connect 100.64.0.5:22
  ├─ 解析：host=100.64.0.5 ∈ 虚拟子网 → 拉 /api/hub/nodes 构建 vipTable → vipTable[100.64.0.5] = "nodeB"
  ├─ 选路（vipDialFunc / meshGatewayDial 叠加）：
  │    ├─ 优先 GatewayConnect(peer=nodeB, addr="100.64.0.5:22")  → linkPool 已建链路
  │    └─ 回落 mesh.Dial(svc={Node:nodeB, Addr:"100.64.0.5:22"})  → webrtc / hub 中继
  ├─ 拨号帧：WriteDialFrame(stream, "100.64.0.5:22")
  └─ nodeB relay.Serve DialPolicy：100.64.0.5 == self.VirtualIP 且 22 ∈ 端口白名单
     → 拨号 127.0.0.1:22 → 双向 pump → 客户端拿到 nodeB 本机 22 端口 TCP 流
```

## 4. 关键接口

### 4.1 `pkg/tunnel/hub`（分配 + 下发）

- 新增 `pkg/tunnel/hub/vip.go`：`Allocator` 接口 + `hubAllocator` 实现（含从快照重建）。
- `NodeInfo` 增加字段 `VirtualIP netip.Addr`（route_table.go:22）。
- `registerNode`（router.go:301）成功后：瞬态过滤 → allocator.Alloc(mesh, nodeID) → 写 `NodeInfo.VirtualIP`；`REG_OK` 扩展能力位 `virtual-ip` 下发本节点 VIP（`buildRegisterAck`，router.go:240 扩展）。
- `MeshRouteTable` 增加 `VirtualIPOf(id)` / `NodeByVirtualIP(mesh, addr)` 反查。
- `persist.go` `NodeSnap` 增加 `VirtualIP`；`RestoreFromSnapshot` 填回 + 重建 allocator。
- `/api/hub/nodes` 序列化增加 `virtual_ip` 字段（`pkg/server/hub_handler.go`）。
- 配置：`server.HubConfig` 增加 `VirtualSubnet string`（默认 `100.64.0.0/10`），`Default()`/`SetDefaults()`/`Validate()` 三段式补齐，`Validate` 限定 IPv4。

### 4.2 `pkg/tunnel/mesh`（路由 + 本地表）

- 新增 `pkg/tunnel/mesh/vip.go`：`vipTable`（`map[netip.Addr]vipEntry{vipEntry: nodeID + Name}`，锁保护）；`ParseVirtualAddr` / `IsVirtualAddr(addr, subnet)`；`deterministicAllocator`（实现 `hub.Allocator`）。
- `NodeConfig` 增加 `VirtualSubnet string` / `VirtualIP netip.Addr` / `VIPAllowPorts []int`（本节点 VIP 与开放端口白名单）。
- `runDiscoveryLoop`（discovery.go）拉 `/api/hub/nodes` 时填充 `vipTable`（`ListHubNodes` 返回类型扩展带 `virtual_ip`）；mDNS TXT 增加 `vip=`。
- `gateway.go` `handleConn` 增加虚拟 IP 路由分支：`req.Addr` host ∈ 虚拟子网 → 查 `vipTable` 定位 peer（`req.Peer` 为空时）→ 复用已建链路。**`newGateway` 注入 `vipTable` 指针或 `NodeConfig`**（v2，审查 M-1：Gateway 结构当前无 vipTable 字段，需接线）。
- `mesh.go` 新增 `VipDial`（或 `vipDialFunc` 包装）：解析虚拟 IP → node-id → 回落 `mesh.Dial`/`GatewayConnect`；`Result.Kind` 新增 `"virtual-ip"` 但**透传真实路径 Kind**（webRTC/relay/peer-link），不覆盖（v2，审查 M-4）。

### 4.3 `pkg/tunnel/relay`（出口 NAT）

- 新增 `NewVirtualIPDialPolicy(subnet netip.Prefix, selfVIP netip.Addr, allowPorts []int, allowCIDRs, serviceAddrs []string) func(string)(string,bool)`：
  1. **先委托内部 `NewServiceDialPolicy`（ServiceAddrs 精确匹配）**——命中宣告地址直接放行（真实 CGNAT 流量逃生口，I-1）；
  2. 未命中 → 目标 host ∈ 虚拟子网：==selfVIP 且端口 ∈ allowPorts（含宣告端口）→ 改写 `127.0.0.1:<port>` 放行；==selfVIP 但端口不在白名单 → 拒绝；!=selfVIP → 拒绝；
  3. 其余 → 回落 `DialAllowed` 公网/CIDR 逻辑。
- `ServeOptions.DialPolicy` 已支持注入，`mesh node` / `relay start` / `p2p listen` 装配时带上。

### 4.4 `pkg/server`（中继回落翻译，可选增强）

- `RelayStreamHandler`（relay_stream.go）：`RelayStreamRequest{Target,Addr}` 的 `Target` 已含 node-id；若客户端以虚拟 IP 寻址（`--node <vip>`），在此层翻译为 `(Target=node-id, Addr=<vip>:<port>)`。**第一版可选**（mesh connect 已本地解析），标记为后续接线点。

### 4.5 `pkg/client`

- 新增/扩展：`ListHubNodes(ctx, mesh) ([]HubNodeInfo, error)`（SproxySig 签名拉 `/api/hub/nodes`，`HubNodeInfo` 含 `VirtualIP`）——供一次性 CLI 构建 vipTable。

### 4.6 `cmd/sclient`

- `mesh.go` `newCmdMeshConnect`：参数解析区分服务名 / 虚拟 IP（`netip.ParseAddr` + `IsVirtualAddr`）。
- `mesh_node.go`：`--virtual-subnet` / `--vip-allow-port` flag → `NodeConfig.VirtualSubnet` / `VIPAllowPorts`。
- `mesh status`：显示 `virtual_ip`。

## 5. 边界与安全面

1. **虚拟 IP 分配不是未认证的地址注入面**：分配权在 hub（注册准入 SproxySig + HMAC proof，`auth.Authenticate` 恒时比较 + ts/nonce 防重放），节点不可自选虚拟 IP；客户端侧 `vipTable` 只接受认证数据源（hub 节点列表 / mDNS 签名 TXT）。
2. **mesh 隔离不破**：虚拟 IP 只在所属 mesh 节点列表下发；`NodeByVirtualIP(mesh, addr)` 按 mesh 过滤；跨 mesh 的虚拟 IP 不可路由。
3. **出口 SSRF 边界不放松**（v2，C-1 闭环）：`DialPolicy` 虚拟 IP 分支只放行「目标 == 本节点虚拟 IP 且端口 ∈ 开放白名单」，改写为 `127.0.0.1:<port>`；虚拟子网内其他地址、未开放端口一律拒绝；虚拟子网外回落既有精确匹配/CIDR（公网默认拒私有）。
4. **默认 loopback**：网关仍仅 loopback（`isLoopbackHost` fail-closed）；虚拟 IP 路由不引入新的非 loopback 监听。
5. **拨号门控不变**：虚拟 IP 拨号仍受 `--dial-allow` 约束（未开启出口模式的节点拒绝 dial 帧）。
6. **子网配置校验**：`hub.virtual_subnet` 非法/非 IPv4 CIDR 由 `Validate` 拒绝；与真实 CGNAT 段重叠时，因命中顺序（宣告地址优先）不遮蔽真实流量。
7. **并发正确性**：`vipTable` 与分配器持锁；`-race` 稳定；分配/释放与注册/移除回调串行（`onRemove`）；一次性 CLI 与常驻 node 的表不共享（M-8）。

## 6. 与现有模块的接口点（汇总）

| 模块 | 文件 | 变更 |
|------|------|------|
| hub 注册 | `pkg/tunnel/hub/router.go` | `registerNode` 瞬态过滤 + 分配虚拟 IP；`REG_OK` 能力位下发 |
| hub 分配 | `pkg/tunnel/hub/vip.go`（新） | `Allocator` 接口 + `hubAllocator`（含快照重建） |
| hub 路由表 | `pkg/tunnel/hub/mesh_route_table.go` | `VirtualIPOf` / `NodeByVirtualIP` |
| hub 持久化 | `pkg/tunnel/hub/persist.go`、`persist_snapshot.go` | `NodeSnap.VirtualIP` 落盘/恢复/重建 allocator |
| hub 节点列表 | `pkg/server/hub_handler.go` | `/api/hub/nodes` 带 `virtual_ip`（+ 可选扩展 `/api/hub/services`） |
| 配置 | `pkg/server/config.go` | `HubConfig.VirtualSubnet` + 三段式 + IPv4 校验 |
| mesh 本地表/路由 | `pkg/tunnel/mesh/vip.go`（新）、`discovery.go`、`gateway.go`、`mesh.go` | `vipTable`、`ListHubNodes` 扩展、`VipDial`、网关虚拟 IP 分支（注入 vipTable） |
| 出口 NAT | `pkg/tunnel/relay/leaf.go` | `NewVirtualIPDialPolicy`（宣告地址优先 + 端口白名单） |
| 客户端 | `pkg/client` | `ListHubNodes`（拉带 `virtual_ip` 的节点列表） |
| CLI | `cmd/sclient/mesh.go`、`mesh_node.go` | `mesh connect <vip>:<port>`、`--virtual-subnet`、`--vip-allow-port`、status 显示 |
| 测试 | `pkg/tunnel/mesh/*_test.go`、`pkg/tunnel/relay/*_test.go`、`pkg/server/hub_*_test.go`、`pkg/client/*_test.go` | 见 §7 |

## 7. Definition of Done

1. 节点注册获得稳定虚拟 IP，重启/hub 重启后不变化（hub 持久化 + 快照重建分配表测试，**不把已持久化的 VIP 再分给新节点**）。
2. 瞬态节点（`disc-*`/`mesh-*`/`p2p-*`）**不分配** VIP；`REG_OK` 下发本节点 VIP，`Discover=false` 的 mesh node / `relay start` 出口节点也能立即得知自身 VIP。
3. `mesh connect <vip>:<port>` 经已建直连链路访问对端节点本机**白名单端口**服务（in-process `-race` 测试 + 真实双节点二进制验证）。
4. **安全（C-1 闭环）**：`mesh connect <vip>:18085`（网关）/SOCKS/未宣告端口被出口拒绝（非白名单端口拒绝测试）；虚拟子网内非本节点地址拒绝；`--dial-allow` 门控仍生效。
5. **逃生口（I-1 闭环）**：真实地址落在虚拟子网内的宣告服务（`--service web:100.64.x.y:8080`）经宣告地址仍可访问（命中顺序测试）。
6. webrtc 打洞失败时回落 hub 中继路径仍可用（`mesh.Dial` 回落传 `(node, <vip>:<port>)` 出口侧可解析）。
7. mDNS 无 hub 模式回落确定性分配，同网段两节点虚拟 IP 互访可用；`virtual_subnet` 配置为 IPv6 被 `Validate` 拒绝。
8. 质量门禁：受影响包 `go test -race -count=1 ./...` 全绿；`make lint` 0 issues（主 + 每个子 go.mod）；`make build-all` + `make test-all` + `make check-loopback` 全绿。
9. 对抗式审查全部发现（含 Minor/参考级）修复，reviewer 逐条关单；无未解决 Critical/Important。
10. 合并后写 `docs/superpowers/learnings/2026-08-30-stage4-virtualip.md`。

## 8. 拆分子任务建议（实现阶段按此 PR 拆）

1. **A：hub 侧分配 + 下发**（`hub/vip.go` + NodeInfo.VirtualIP + registerNode 瞬态过滤 + REG_OK 能力位 + persist 快照重建 + `/api/hub/nodes` + 配置三段式）。
2. **B：出口侧 NAT**（`NewVirtualIPDialPolicy` 端口白名单 + 命中顺序 + mesh node/relay 装配）。
3. **C：mesh 路由 + CLI**（vipTable + `ListHubNodes` 客户端方法 + 网关虚拟 IP 分支注入 + `VipDial` + `mesh connect <vip>:<port>` + `--vip-allow-port` + status 显示 + mDNS 回落）。
4. **D：E2E + 对抗式审查 + 修复**。

每子任务独立 feature 分支 → PR → CI → 人工 squash 合并，流程沿用阶段 3。
