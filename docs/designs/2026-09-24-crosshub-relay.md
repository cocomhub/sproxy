# 跨 hub 数据面中继（11.1-⑥）

## 背景 / 目标
- 现状：联邦**发现面**已完整——`FederationClient.SyncServices`（跨 hub 服务发现）、`PeersForNode/PeerForNode`（按 mesh 严格匹配定位目标节点注册在哪个对端 hub，注释已声明用途："把 relay 拨号请求转发到该对端的 /api/relay/stream"）。`Handlers.SetFederationClient` 已联动装配 `relayStream.SetFederation(fc, h.hubID)`。**缺的是转发执行体**：本 hub 路由表未命中目标节点时，把 `RelayStreamRequest`（CONNECT 语义）经上游 hub 路由转发。
- 目标：跨 hub 数据面中继（CONNECT 转发）+ 环路防重（hop 路径记录 hubID 链）+ 故障转移（多对端按序尝试）。

## 组件与接口
- `pkg/tunnel/hub/federation.go` 增：`ForwardRequest(ctx, peer FederationPeer, req RelayStreamRequest) (io.ReadCloser, error)`——把 CONNECT 拨号帧 POST 到 `peer.URL + "/api/relay/stream"`（SproxySig 签名复用 `syncPeer` 的凭据模式：AK + skey-id + SK；响应 200 后返回流）。
- `pkg/tunnel/hub/relay.go`（或 relay_stream 所在包）增：
  - `RelayStreamHandler.SetFederation` 现有注入基础上，路由表未命中时：`PeersForNode(target, mesh)` 多对端按序尝试（首个失败→下一个，与 `MeshConnect` 多候选回退一致）；全部失败才返回 404。
  - 请求帧带 `HopPath []string`（本 hubID 链），转发时 append 本 hubID；对端 hub 收到后先查自身路由表，再查自身联邦，**环检测：HopPath 含自身 hubID → 直接拒绝**。
- mesh 严格隔离：转发目标必须 `n.Mesh == mesh`（复用 `PeersForNode` 语义，防跨 mesh 泄漏）。

## 数据流
1. L 拨 `mesh connect <svc>` → 本 hub A 路由表未命中 node X → `PeersForNode(X, mesh)` 命中对端 hub B。
2. A 把 CONNECT 帧（含 HopPath=[A]）POST 到 B 的 `/api/relay/stream`。
3. B 路由表命中 X → 本地出口拨号 X → 200 + 流建立，L ⇄ A ⇄ B ⇄ X 数据面贯通。
4. 多跳：B 也未命中且 B 有联邦对端 C → B 转发（HopPath=[A,B]），逐跳延伸到最深可达 hub。

## 错误处理
- 全部对端转发失败 → 404（与既有「本 hub 未命中」同语义，客户端回落/报错不变）。
- 环检测命中（HopPath 含自身）→ 403/拒绝 + 日志（HubIDLoopRejected），防 A→B→A 无限转发。
- 对端 HTTP 非 200 / 凭据失败 → 记错误并尝试下一对端；全部失败才报错。
- 对端 body 超限（复用 maxFederationResponseBytes 同量级防护，relay 流是数据面不走 JSON 解码，用 io.LimitReader 仅防响应头膨胀）。

## 测试 + 变异点
- `TestForward_CrossHubDial`：A 未命中 → 转发到 B → 200 流建立（mock 对端 hub）。
- `TestForward_LoopRejected`：HopPath 含本 hubID → 拒绝（变异：删环检测 → 红）。
- `TestForward_FailoverOrder`：首个对端 500 → 第二个成功（变异：不按序尝试/不尝试下一个 → 红）。
- `TestForward_MeshMismatchRejected`：目标节点 mesh 不匹配 → 不转发（变异：忽略 mesh 过滤 → 红）。
- 变异验证：转发分支缺失（直接 404）→ 红；环检测写成非严格（仅日志不拒绝）→ 红。

## 片划分
- P1：`ForwardRequest` + relay stream 未命中转发 + 多对端故障转移。
- P2：HopPath 环检测 + 拒绝计数 metric。
- P3：跨 hub 多跳深度受限（MaxHops 配置）+ e2e（双 hub 进程）。

## 风险与零回归
- 既有路径（路由表命中）先行判断，联邦转发仅在未命中后触发 → 零回归。
- `PeersForNode` mesh 严格匹配保证不跨 mesh 泄漏（延续发现面既有语义）。
- 新转发是可选能力：未装配 `SetFederationClient` 时行为与现状完全一致（路由表未命中即 404）。
- 环检测 fail-closed：宁可拒绝也不无限转发。
