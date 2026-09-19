<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# via-node 多跳提供者（SmartDial 候选展开模型）设计

日期：2026-09-19
状态：设计定稿（用户已确认架构级重构：版本未发布，允许破坏性变更）
作者：pi agent（suixibing 需求）

## 1. 背景与问题

SmartDial（#382）已提供 `--smart` 自动选路：并行竞速直连/中继，按端到端建连
耗时择优，胜者缓存 TTL 内单路复用。但**多 node 场景**（用户核心需求：「可能经过
另一个 node 到目标 node rtt 等效果才是最佳的」）尚未真实落地——via-node 仅作为
注册表可扩展的接口预留，无真实提供者。

### 1.1 问题本质：粒度缺口

当前 `PathProvider` 粒度 = **路径类型**（direct/relay/via-node），竞速单位 =
**提供者实例**。但 via-node 的真实候选是**动态多个中间节点 X**（ListHubNodes 在线
节点 ∩ 声明 outbound-dial 能力）。把 X 遍历塞进单个 `Dial` 是错配：

| 技术债点 | 后果 |
|---|---|
| X 无法并行竞速 | Dial 内只能串行尝试或自行并发，与竞速核心「每候选一 goroutine」模型错配 |
| X 无法单独缓存 | 缓存 key=target.Node，无法记住「经 X1 最优还是 X2」 |
| 端到端 RTT 择优失效 | 「哪个 X 最快」无法自然胜出，靠内部逻辑猜测 |
| 复用性差 | 未来新路径类型（多出口 QUIC 等）同样需要多候选，逻辑重复 |

### 1.2 设计决策（用户确认）

**版本未发布（v0.15.0 后 #382 刚合入），允许破坏性变更**——不搞 `CandidateExpander`
可选接口的兼容层，直接重构 `PathProvider` 为**候选展开模型**：竞速核心理解「候选」
（最小单位），路径类型负责「如何展开候选」。无类型断言、无单候选回退分支。

## 2. 目标

1. **via-node 真实多跳**：`--smart` 自动包含经中间节点 X 中转（X 出站拨号到目标 T）
   的候选，端到端 RTT 最短路胜出。
2. **候选展开模型**：`PathProvider.Expand() []Candidate` 让一个路径类型展开多个
   候选（via-node 的每个 X），竞速核心只理解 `Candidate`。
3. **hub 能力透出**：`ListHubNodes()` 返回节点 `Capabilities`（via-node 发现候选 X
   的前提）。
4. **零新协议**：复用现有 `RelayStream(X, T)` 语义（X 出站拨 T，数据面经 X）。
5. **零 CLI 改动**：`--smart` 自动包含 via-node（hub 自动发现 X），用户零学习。

## 3. 架构：候选展开模型

### 3.1 接口重构（`pkg/tunnel/mesh/smart.go`）

```go
// PathProvider 是 SmartDial 的一条路径类型（P1 直连 / P2 中继 / P4 经中间节点多跳）。
// 通过 Expand 展开为竞速候选——一个类型可有多个候选（via-node 的每个中间节点 X）。
type PathProvider interface {
    Name() string
    Priority() int
    Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate
}

// Candidate 是竞速核心的最小单位：一条具体路径实例。
// ID 是候选唯一标识（缓存 key，如 "via-node:node-x" / "direct" / "relay"）。
type Candidate struct {
    ID       string
    Priority int // 继承提供者 Priority（排序/截断用），自包含
    Dial     func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
                   target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
}
```

**为何此形态**：
- 竞速核心只理解 `Candidate`（最小单位），不感知路径类型——单一职责
- via-node 的「多 X」天然展开为多候选，与 direct/relay 平级竞速
- 无类型断言、无单候选回退分支——接口就是真相

### 3.2 竞速核心改动（`DialSmart` 展开逻辑）

```go
// 遍历提供者 → 展开全部候选（direct=1, relay=1, via-node=N 个 X）
for _, name := range SmartPathRegistry.Names() {
    p, _ := SmartPathRegistry.Get(name)
    cands = append(cands, p.Expand(ctx, svc, target)...)
}
// 按 Candidate.Priority 降序 + 截断 MaxCandidates（默认 5：direct+relay+3X）
// 每候选一 goroutine 并行竞速 → 端到端 RTT 最短路胜出 → 缓存候选 ID
```

- 候选数上限 `MaxCandidates` 默认从 4 升到 **5**（容纳 direct + relay + 最多 3 个 X；
  防资源爆炸，用户可经 SmartOptions 调）。
- 缓存：key=target.Node 不变，值从「提供者名」升级为「候选 ID」（`"via-node:X1"` /
  `"direct"` / `"relay"`）——命中单路直达含经 X 的最优路径。

### 3.3 via-node 提供者（新建 `pkg/tunnel/mesh/via_node.go`）

```go
type viaNodeProvider struct{}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

func (p viaNodeProvider) Expand(ctx, svc, target) []Candidate {
    nodes, err := svc.ListHubNodes(ctx) // 前置：HubNodeInfo 带 Capabilities
    if err != nil { return nil }
    xs := filter(nodes, func(n) bool {
        return n.ID != target.Node &&
               slices.Contains(n.Capabilities, hub.CapabilityOutboundDial)
    })
    // 候选数受全局 MaxCandidates 约束；X 取前 3（在线顺序）
    return map(xs, func(x) Candidate{
        ID: "via-node:" + x.ID,
        Priority: 80,
        Dial: func(...) { // X 出站拨 T（零新协议，复用现有语义）
            start := time.Now()
            conn, err := svc.RelayStream(ctx, x.ID, target.Addr)
            if err != nil { return nil, fmt.Errorf("via-node(%s): %w", x.ID, err) }
            return &Result{Conn: conn, Kind: KindViaNode, Latency: time.Since(start)}, nil
        },
    })
}
```

**零新协议（侦察已证实）**：`RelayStream(X, T)` → hub 在 X 的 mux 开流 + 写
`{dial: T}`（relay_stream.go:246 现有逻辑）→ X 的 relay.Serve 收到 dial 帧 →
出站拨 T（leaf.go:160-180 现有逻辑）——数据面经 X 中转，全是现有代码。

## 4. hub 能力透出（via-node 发现前置）

`ListHubNodes()` 返回节点 `Capabilities`（当前 HubNodeInfo 无能力字段）：

| 文件 | 改动 |
|---|---|
| `pkg/tunnel/hub/route_table.go` | `NodeInfo` 加 `Capabilities []string`（注册时保存） |
| `pkg/tunnel/hub/router.go` | `registerNode` 保存 `reg.Capabilities` 到 info |
| `pkg/server/hub_handler.go` | `nodeResp` 加 `Capabilities []string`（透出） |
| `pkg/client/hub.go` | `HubNodeInfo` 加 `Capabilities []string` |

- 旧 hub/旧节点忽略未知能力位（已核实兼容）——新老混部署安全。
- 瞬态节点（disc-/mesh-/p2p-）无 outbound-dial 能力（relay start 才声明），天然过滤。

## 5. 可维护性 / 可扩展性 / 易用性 / 性能

| 维度 | 设计保证 |
|---|---|
| **可维护性** | 接口单一职责：`PathProvider` = 路径类型 + 候选展开，`Candidate` = 竞速最小单位；竞速核心不感知路径类型 |
| **可扩展性** | 未来「本地→X 打洞直连 + X 拨 T」（via-direct-X）只需 Expand 里对每个 X 生成两个候选（via-relay-X / via-direct-X），接口已支持、竞速核心零改动 |
| **易用性** | `--smart` 自动包含 via-node（hub 自动发现 X）；用户零学习、零新 flag |
| **性能** | 全候选并行竞速（每候选一 goroutine）；缓存候选 ID → 命中单路直达；MaxCandidates=5 防资源爆炸 |

## 6. 测试（TDD 红灯先行）

`pkg/tunnel/mesh/smart_test.go` + `via_node_test.go`：

| # | 用例 | 预期 |
|---|---|---|
| 1 | `Expand` 展开：mock ListHubNodes 返回 2 个 outbound-dial X → 2 候选（ID="via-node:X1"、"via-node:X2"，Dial 调 RelayStream(X,T)） | 展开正确 |
| 2 | **竞速经 X 胜出：via-node:X(快) + direct(慢) → 选 via-node:X** | 端到端 RTT 最短路胜出（核心场景） |
| 3 | 缓存候选 ID：命中后单路走 via-node:X | 缓存正确 |
| 4 | capability 过滤：无 outbound-dial 标记的节点不展开 | fail-closed |
| 5 | 既有用例回归：fakePath 改造为 Expand 返回单候选 | 全绿 |
| 6 | -race / R14 / R18 门禁 | 通过 |

测试基建：`viaNodeProvider.Expand` 用 mock `client.FileClient`（ListHubNodes 返回
可注入列表）；fake 候选 Dial 返回真实连接（net.Pipe）或延迟模拟。

## 7. 改动面清单

| 文件 | 动作 |
|---|---|
| `pkg/tunnel/mesh/smart.go` | PathProvider 接口重构（Dial→Expand）+ Candidate + 竞速核心展开 + 缓存候选 ID + MaxCandidates 默认 5 |
| `pkg/tunnel/mesh/via_node.go` | 新建：viaNodeProvider |
| `pkg/tunnel/mesh/mesh.go` | +KindViaNode = "via-node" 常量 |
| `pkg/tunnel/mesh/smart_test.go` | fakePath 改造（Expand 返回单候选）+ 新用例 |
| `pkg/tunnel/mesh/via_node_test.go` | 新建：Expand/竞速/缓存/过滤用例 |
| `pkg/tunnel/hub/route_table.go` | NodeInfo.Capabilities |
| `pkg/tunnel/hub/router.go` | registerNode 保存 capabilities |
| `pkg/server/hub_handler.go` | nodeResp.Capabilities 透出 |
| `pkg/client/hub.go` | HubNodeInfo.Capabilities |
| `build/lab/DEPLOY-GUIDE.md` | §5.3/§7/§9 补 via-node 说明（独立仓库） |

**CLI 零改动**（`--smart` 自动包含 via-node）。

## 8. 范围与不做

- **不做 via-direct-X**（本地→X 打洞直连 + X 拨 T）：第一版只做 via-relay-X（经 hub
  到 X + X 拨 T），可靠且零新协议；via-direct-X 作为 Expand 内生成双候选的后续增强。
- **不做活跃连接实时迁移**（沿用 #382 决策：新建连接时择优）。
- **不做候选数量动态感知**（X 列表变化由 ListHubNodes 每次 Expand 实时拉取——Expand
  每竞速调用一次，天然新鲜；缓存 TTL 过期重新竞速时重新拉取）。
