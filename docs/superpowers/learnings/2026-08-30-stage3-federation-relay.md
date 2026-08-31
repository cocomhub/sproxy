# 阶段 3 子任务 3：hub 联邦 B——跨 hub 转发路径（A→hub1→hub2→B 链式中继）复盘

> 分支 `feature/mesh-p3-federation-relay`，PR #127（3 个 commit：c2407df 功能 +
> 2cf2521 审查修复 + d31e763 Minor 修复）。本文档为本地 gitignored 工件。

## 目标与结果

在 hub 联邦节点表可见性（#126）基础上打通数据面：hub-A 路由表未命中目标节点但该
节点是联邦候选时，把 `/api/relay/stream` 拨号请求转发到上报该节点的对端 hub，实现
A→hub1→hub2→B 链式中继。DoD 全达成：

1. 跨 hub 转发路径建立（in-process + CLI 级真实二进制 echo 往返）；
2. 防环/防回源机制（跳数上限 + 路径回源拒绝，环路 508）；
3. 自动测试（-race 连跑 3 次）+ 手动真实二进制双 hub echo 往返。

## 关键设计决策

1. **复用对端 `/api/relay/stream`（HTTP CONNECT 风格）做转发**，而非发明 hub-to-hub
   mux 拨号帧协议。转发请求 = 与客户端一样的 CONNECT，只是目标 hub 变为对端。这
   复用全部既有认证（SproxySig）、拨号策略、错误语义（200=数据面就绪、502/504/508）。

2. **转发器放在 `pkg/server` 而非 `pkg/tunnel/hub`**：hub 是低层包，转发需要发 HTTP
   出站请求。独立实现 CONNECT 拨号器（`relayForwardDialer`）而非复用 `pkg/client`——
   **`pkg/client` 的 e2e 测试反向依赖 `pkg/server`，若 server 生产代码 import client 会
   构成测试编译环**。这是本项目的一个真实约束：低层 SDK 的 e2e 测试 import 高层 server。

3. **防环自洽（重要教训）**：第一版防环仅靠 `cfg.Hub.NodeID` 追加 self ID 到路径，
   但 `node_id` 默认空 → 路径恒空 → 防环退化为纯跳数上限。对抗式审查指出后改为
   **恒追加「下一跳 peer.ID」**：与对端解析目标时的 `peer.ID` 同一命名空间，防环
   检查自洽，不依赖跨 hub 配置一致。配置了 `node_id` 时额外追加 self ID（回源前一
   跳即拒绝，更严格）。**教训：防环/去重等机制若依赖「可选配置」，默认配置下会静默
   失效——要么给配置合理默认，要么让机制自洽。**

4. **TLS 握手超时缺口（重要教训）**：第一版 `relayForwardDialer.Dial` 用
   `tls.Dialer.DialContext(ctx)`，但 `net.Dialer.Timeout` 只约束 TCP 三次握手，TLS
   握手仅受 ctx deadline 约束；而转发传入的是客户端请求 ctx（可无 deadline）→ 对端
   TLS 黑洞可无限阻塞。审查后改为**先裸 TCP Dial + 设 socket deadline（min(ctx,30s)）
   再 `tls.Client.HandshakeContext(ctx)`**，握手阶段整体有界。**教训：`tls.Dialer` 的
   Timeout 不覆盖 TLS 握手；任何出站连接的手写拨号器都要显式约束 TLS 阶段。**

5. **多对端故障转移**：`PeersForNode` 返回全部候选，`serveForwarded` 按序尝试
   （404/502/508 均转移——508 不代表其它对端也会回源，每个 Forward 独立防环），
   对齐 `MeshConnect` 多候选回退。**教训：防环命中的 508 不应阻断故障转移。**

6. **复审第二轮的 5 条 Minor 教训**：
   - 508-break 阻断故障转移（要 continue）；
   - 日志属性误标（error message 当 peer 记）——日志 key 必须名副其实；
   - 类型断言 `.(*T)` 改 `errors.As`（防未来包裹静默退化）；
   - peer.id 需全局唯一（文档要求）；
   - TLS 握手消耗预算后，请求/响应阶段要重置 deadline（防慢连接误判 502）。

## 踩坑清单

- **import cycle**：`pkg/server` 生产代码不能 import `pkg/client`（client 的 e2e 测试
  import server）。若必须复用 client 逻辑，要么把逻辑抽到共享包，要么在 server 内独立
  实现（本项目选了后者）。**先查依赖方向再决定复用 vs 独立实现。**
- **防环路径命名空间**：self-ID（node_id）与 peer.ID（federation.peers[].id）是两套
  命名空间，混用路径需各自检查，否则不匹配导致检查失效。
- **`-race` 下 E2E 超时**：并发拨号/多 goroutine 测试在 `-race` 下显著变慢，deadline
  要留足余量（3 倍）。
- **`make test` 预存在失败**：`TestE2E_MeshNode_ServiceAccess`（webrtc 网关数据面）
  在 `origin/master` 上同样失败，与本改动无关，报告时需注明。
- **3+ hub 真实链式不可行**：联邦只同步各 hub 本地路由表（防同步环路），A 看不到
  C 的节点（经 B 中转），故真实多跳链式被设计排除（路线图 SKIP 项「多跳 chained
  relay」）。本子任务实现的是 2 hub（1 跳转发）链式，防环为多跳场景兜底。

## 验证矩阵

| 项 | 结果 |
|----|------|
| `go test -race -count=3`（跨 hub 中继相关） | 通过 |
| `go test -race`（client/hub/server 全量） | 通过 |
| `TestE2E_CrossHubRelay`（CLI 级真实二进制） | 通过 |
| `make lint` | 0 issues |
| `make build-all` / `make test-all` / `make check-loopback` | 通过 |
| 独立对抗式审查（2 轮） | 发现的 Critical/Important/Minor 全部修复 |
