# 阶段 4·虚拟 IP / 子网分配（工作项 1）最终版学习记录

> 日期：2026-08-31
> 范围：PR #134（A hub 分配 → B 出口 NAT → C mesh 路由+CLI → D E2E，1 个 PR 全量合入）
> 状态：已合入 master `863f84a`（阶段 4 复盘：`docs/superpowers/specs/2026-08-31-sproxy-fullmesh-stage4-review.md`）
> 设计：`docs/superpowers/specs/2026-08-30-sproxy-fullmesh-stage4-virtualip.md`（v2，含对抗审查修正）
> 前置：阶段 1–3 已合入（master `7048018`）

> 说明：本文是 **已落地实现** 的最终版记录。设计阶段的交接简报见
> `2026-08-31-stage4-virtualip-handoff.md`（描述 v2 设计的规划意图）。
> 本文记录实际实现后的差异与验证结论——handoff 是"设计要做什么"，本文是"实
> 际做成什么样"。

## 1. 目标（与设计一致）

Tailscale/ZeroTier 风格虚拟 IP 寻址：mesh 节点（node-id）在所属 mesh 内获得稳定唯一
虚拟 IP；`mesh connect <vip>:<port>` 拨号到对端节点**本机** `<port>` 服务（虚拟主机语
义）；本地网关 NAT 路由到对端已建链路/hub 中继；端口白名单安全边界（C-1 红线）。

## 2. 用户已确认的决策（2026-08-30）

1. 分配双实现：hub 权威递增分配 + mDNS 无 hub 回落本地确定性哈希。
2. 虚拟主机语义 + 端口白名单（安全红线 C-1）：只放行 `--service` 宣告端口或
   `--vip-allow-port`；未开放端口拒绝。
3. REG_OK 能力位下发自身 VIP（不依赖 discovery 环）。
4. 实现顺序：文件同步先（A–E，PR #129–#133），虚拟 IP 后（#134）。

## 3. 实际实现的架构决策（与设计 v2 对照）

| 设计 v2 | 实际实现 | 差异说明 |
|---------|----------|----------|
| AD-1 `Allocator` 接口 + `hubAllocator`/`deterministicAllocator` | `pkg/tunnel/hub/vip.go` `hubAllocator`（递增分配 + `Reserve` + 快照重建灌回 PreloadAllocator）；`pkg/tunnel/mesh/vip.go` 实现 `hub.Allocator` 确定性回落 | 一致。确定性实现在 mesh 侧实现接口（hub 不 import mesh 防环） |
| AD-1 瞬态节点过滤 | `isTransientNodeID`（router.go:356）跳过 `disc-*`/`mesh-*`/`p2p-*` 临时身份，`registerNode` 内跳过分 VIP | 一致。防 `mesh connect` 每次分配/释放 VIP、vipTable 濒死条目 |
| AD-1 REG_OK 能力位下发 | `pkg/tunnel/hub/register_ack.go` `REG_OK` 扩展下发 `<secret>:<vip>`；旧客户端无能力位兼容 | 一致。Discover=false 的 relay 出口节点不静默失效 |
| AD-2 子网 CGNAT `100.64.0.0/10` | `VipTable` 限制 IPv4；`MeshConfig.VirtualSubnet` 三段式（CLI flag > 配置 > 默认）；`Validate` 拒绝 IPv6 | 一致 |
| AD-3 端口白名单 + 命中顺序 | `pkg/tunnel/relay/leaf_vip.go` `NewVirtualIPDialPolicy`：ServiceAddrs 精确匹配优先 → 虚拟子网白名单（==selfVIP 且端口 ∈ allowPorts → 改写 127.0.0.1）→ 拒绝 | 一致。真实 CGNAT 流量不被虚拟子网遮蔽 |
| AD-4 vipTable 防注入 | `pkg/tunnel/mesh/vip.go`：hub 权威模式每次刷新从签名 hub 列表**原子重建**（清陈旧）；mDNS 模式校验声明 VIP == deterministicAllocator(mesh, nodeID)；冲突/子网外拒绝 | 一致（安全审查闭环核心） |
| AD-5 CLI | `mesh connect <vip>:<port>`（vipDialFunc 包装 meshDialFunc）；`mesh status` 显示 virtual_ip；`mesh node --virtual-subnet`/`--vip-allow-port` | 一致 |
| AD-6 数据通路 | vip→node 解析 → 已建链路 GatewayConnect 优先 / mesh.Dial 回落 → 拨号帧 → 出口 DialPolicy 识别 selfVIP | 一致 |

## 4. 安全闭环（自动安全审查 MEDIUM）

- **VIP 注入面（可预测 + 后写覆盖）**：VipTable 冲突拒绝精化——hub 权威原子重建 +
  mDNS 确定性校验（声明 VIP == hash 计算值）+ first-writer-wins 防劫持 + 子网外拒绝。
- **C-1 端口白名单红线**：`mesh connect <vip>:18085`（网关）/SOCKS/未宣告端口被拒；
  回归测试 `TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected`（C-1：未宣告端口 9999 拒绝）。

## 5. 验证结论（PR #134）

- 2 条 E2E：`TestE2E_MeshConnect_VirtualIP`（闭环 echo）+ `TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected`（C-1）。
- 单测：`hub/vip_test.go`（分配/回收/保留/冲突/子网外拒绝/快照重建）+ `mesh/vip_test.go`
  （确定性分配/校验/冲突）+ `leaf_vip_test.go`（DialPolicy 命中顺序/白名单/逃生口）。
- CI：Build×6 / Lint / Test(ubuntu+windows) / Benchmark / UI E2E / SonarQube 全绿。
- 规模：1 个 PR，8 commits，+3160 行。

## 6. 踩坑与教训

1. **REG_OK 能力位必须自洽不依赖 discovery 环**：若 selfVIP 只靠 `/api/hub/nodes`
   discovery 拉取，Discover=false 的 relay 出口节点会恒零值 → 对它发起的虚拟 IP 拨号
   fail-closed 全拒（静默失效）。随注册 ACK 直接下发是唯一可靠路径。
2. **VipTable 后写覆盖风险**：多数据源（hub 列表 vs mDNS）刷新竞态下，坏源可覆盖好源。
   解法：hub 权威模式每次刷新原子重建（清陈旧），mDNS 模式校验声明值 == 确定性计算值。
3. **出口 DialPolicy 命中顺序**：虚拟子网判断必须在 ServiceAddrs 精确匹配**之后**——
   否则宣告在 CGNAT 段内的真实服务会被虚拟子网遮蔽（真实流量失密）。
4. **确定性回落与 hub 权威冲突**：mDNS 无 hub 模式的确定性分配必须与 hub 分配语义一致
   （同一子网、同一前缀规则），否则节点在 hub/mDNS 间切换时 VIP 变化导致连接漂移。
5. **安全审查的书面确认也是闭环**：IDOR（无 owner 设计）注释书面确认即可通过，但
   **安全红线（端口白名单、fail-closed、子网外拒绝）是硬约束**，必须测试强制。

## 7. 对后续（阶段 5+）的建议

- `--xfer` 服务端接线可直接复用本阶段的 DialPolicy（出口）/锁棚装配模式。
- 联邦/TURN 持久化的原子写可复用 `hub.Persister`（0600 + 原子改名）。
- learnings 现改为入库（T0），后续阶段复盘记得写最终版而非只留 handoff。
