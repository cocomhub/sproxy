<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# via-direct-X：经中间节点 X 的数据面直连多跳（SmartDial 双候选）

日期：2026-09-19
状态：设计定稿（用户已确认方案 A：经 hub 信令打洞到 X，数据面直连）
作者：pi agent（suixibing 需求）

## 1. 背景与问题

via-node（#385）已实现 `via-relay-X`：本地 → hub → X（RelayStream）+ X 出站拨 T。
数据面**每一字节都经 hub 中继**——hub 是转发瓶颈（带宽、延迟、可用性）。

规格（2026-09-19-via-node-multihop-design.md §10）明示的后续增强：**via-direct-X**
（本地 → X 打洞直连 + X 拨 T）。本设计把「信令经 hub 桥、数据面 webrtc 直连 X」
作为第一版形态（方案 A，用户已确认）。

### 1.1 问题本质

via-relay-X 的数据面路径：`L ⇄ hub ⇄ X ⇄ T`（两段都经 hub 字节）。
via-direct-X 的目标路径：`L ⇄(webrtc 打洞)⇄ X ⇄ T`（数据面只有一段经 X，不经 hub 字节）。

hub 信令桥（HTTP 存转）只承载 offer/answer 控制面（K 级），不承载数据字节——
数据面直连 X 后，hub 不再是瓶颈，多跳 RTT 显著缩短。

### 1.2 关键前提（可行性侦察结论）

1. **X 常驻节点恒在 `runWebRTCAcceptLoop` 监听**（mesh node 模式）：`webrtc.ListenWithSignalerCtx`
   经 hub 信令桥消费 offer。`HubSignaler(X)` 打洞到 X 的 offer/answer 由 X 的 accept 侧消费——
   **零新信令路径**。
2. **`DialWebRTC(HubSignaler(X), MeshService{Node: X, Addr: T})`** 已做完整链路：webrtc 打洞 →
   `WebRTCStream` 开 mux 流 → 写 `DialRequest(T)` 拨号帧 → X 的 `relay.Serve` 出口拨 T。
   该函数恰好是「直连 X + X 拨 T」的现成实现——**零新拨号逻辑**。
3. **候选展开模型（#385）天然支持双候选**：Expand 对每个 X 生成 `via-relay:X`（现有）+
   `via-direct:X`（新增），竞速核心零改动、候选索引（#386）零改动。

## 2. 目标

1. **via-direct-X 真实多跳**：`--smart` 自动包含「打洞直连 X + X 拨 T」候选，与
   via-relay-X / direct / relay 平级竞速，端到端 RTT 最短路胜出。
2. **数据面直连**：L→X 数据面 webrtc 直连（不经 hub 字节），hub 只承载信令控制面。
3. **零新协议 / 零新信令路径**：复用 HubSignaler、DialWebRTC、WebRTCStream、DialRequest、
   relay.Serve 出口拨号。
4. **零 CLI 改动**：`--smart` 自动包含（hub 信令桥寻址 X 已注册节点）。

## 3. 架构：双候选展开

### 3.1 候选生成（`pkg/tunnel/mesh/via_node.go`）

```go
// viaNodeProvider.Expand 对每个 X 生成两个候选（平级竞速，端到端 RTT 择优）：
//   via-relay:X   —— 数据面经 hub 中继（现有，RelayStream(X, T)）
//   via-direct:X  —— 数据面 webrtc 直连 X（DialWebRTC(HubSignaler(X))，X 出口拨 T）
```

- 候选 ID：`via-relay:<xID>` / `via-direct:<xID>`（缓存 key 可区分，故障转移天然）
- 两候选同 Priority（80，继承 viaNodeProvider）；`MaxCandidates` 需上调容纳双候选
- 候选生成需 `Dial` 可访问**信令器**（拨号 X 用）：`Candidate.Dial` 签名已带
  `signaler webrtc.Signaler` 参数（#382 设计）——竞速核心把调用方传入的 signaler
  透传给每个候选。via-direct-X 用该 signaler 打洞到 X。
  - **前提**：调用方（CLI mesh connect --smart）传入的 signaler 是 `*hub.HubSignaler`
    （已注册节点身份）→ 可对任意已注册节点 X 打洞。
  - 若 signaler 非 hub 信令（mDNS DirectSignaler）或 nil → via-direct-X 候选不可用
    （Expand 时无法预知，Dial 时 `SignalerUsable` 检查失败即放弃该候选，竞速聚合错误）。

### 3.2 via-direct-X 拨号逻辑

```go
func viaDirectXDial(ctx, signaler, xID, target) (*Result, error) {
    if !SignalerUsable(signaler) {
        return nil, fmt.Errorf("via-direct(%s): 无可用信令器（需 hub 信令桥）", xID)
    }
    conn, err := DialWebRTC(ctx, signaler,
        &client.MeshService{Node: xID, Addr: target.Addr}, opts.ICE)
    if err != nil {
        return nil, fmt.Errorf("via-direct(%s): %w", xID, err)
    }
    return &Result{Conn: conn, Kind: KindViaDirect, Latency: time.Since(start)}, nil
}
```

- `DialWebRTC` 内部：webrtc 打洞到 X（signaler 的 offer/answer 经 hub 桥）→
  `WebRTCStream` 开 mux 流写 `DialRequest(target.Addr)` → X 的 `relay.Serve` 出口拨 T →
  pump。返回的 `Result.Conn` 是「L ⇄ X ⇄ T」全链路。
- **错误语义**：打洞失败（X 不可达/NAT 限制）→ 返回错误 → 竞速聚合（via-relay-X 候选
  仍参与，故障转移天然）。

### 3.3 新 Kind 常量

```go
const KindViaDirect = "via-direct"
```

（结果 Kind 用于测试断言与未来 UI/日志展示；与 `KindViaNode`/`KindWebRTC`/`KindRelay` 并列。）

### 3.4 MaxCandidates 调整

当前默认 5（direct + relay + 3X）。via-direct-X 后每 X 双候选 → 默认需上调到 **8**
（direct + relay + 3×2）。经 `SmartOptions.MaxCandidates` 可调（外部库覆盖）。

## 4. 安全边界

via-direct-X 与 via-relay-X **同一出口边界**：

1. **X 出口拨号**仍由 X 的 `DialPolicy`（`--dial-allow` 精确放行 + 服务宣告地址）把守——
   经 webrtc 直连 mux 流写入的 `DialRequest(T)` 与经 hub 中继的同帧走**同一** `relay.Serve`
   处理路径（`leaf.go` 的 dOK 分支），无新暴露面。
2. **信令认证**：HubSignaler 经 hub 信令桥（X-Node-Secret + SproxySig 签名）——
   offer/answer 只发往已注册节点 X 的收件箱，不可伪造。
3. **mux 流身份**：webrtc 直连的 mux 流与 hub 中继流在 X 侧无差别（都是 `m.Accept`），
   `DialPolicy` 是唯一地址授权点（防 SSRF）。

## 5. 测试策略（TDD 红灯先行）

| 用例 | 断言 |
|---|---|
| `TestViaNodeExpand_GeneratesDualCandidates` | Expand 对每个 X 生成 `via-relay:X` + `via-direct:X` 双候选 |
| `TestViaNodeExpand_DirectCandidateNoSignaler` | via-direct-X Dial 无信令器 → 错误（via-relay-X 仍参与） |
| `TestViaDirect_E2E_RealDataPlane` | in-process hub + X 节点 `runWebRTCAcceptLoop` + `DialWebRTC(HubSignaler(X))` 打洞 → 数据面 echo（**真实链路**） |
| `TestDialSmart_ViaDirectWinsWhenFastest` | via-direct-X（快）vs via-relay-X（慢）→ via-direct-X 胜出（RTT 优先） |

变异验证：via-direct-X 拨号目标改错（直拨 T 而非经 X）/ 信令 peer 改错 → e2e 红。

## 6. 明确不做（YAGNI）

- **不做 mDNS 直连信令到 X**（方案 B）：hub 模式下 X 的信令端点地址未向 hub 宣告，
  需扩展协议（Meta 加 signal 字段）——改动面大且与 via-relay-X 的 hub 发现模型割裂。
  纯 mDNS 无 hub 场景的 via-direct-X 待未来（届时 X 信令端点经 mDNS TXT 发现）。
- **不做 hub 信令端点透出**（方案 C）：同上，第一版零协议扩展。
- **不做活跃连接实时迁移**（沿用 #382/#385 决策：新建连接时择优）。

## 7. 影响面

| 文件 | 改动 |
|---|---|
| `pkg/tunnel/mesh/via_node.go` | Expand 生成双候选 + via-direct-X 拨号逻辑 + `KindViaDirect` 常量 |
| `pkg/tunnel/mesh/smart.go` | `MaxCandidates` 默认 5 → 8 |
| `pkg/tunnel/mesh/via_node_test.go` | 双候选展开测试 |
| `pkg/tunnel/mesh/via_node_e2e_test.go` | 真实数据面 e2e 扩展（HubSignaler 打洞） |
| `pkg/tunnel/mesh/smart_test.go` | via-direct-X 竞速胜出用例 |

零新协议、零 API 变更（`KindViaDirect` 为新常量，非破坏）、零 CLI 改动。
