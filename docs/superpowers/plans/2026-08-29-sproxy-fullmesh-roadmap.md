# sproxy 完全组网发展规划——实现计划

> **面向 AI 代理的工作者：** subagent-driven-development 或 executing-plans。每阶段独立特性分支，步骤用 `- [ ]` 语法追踪。

**目标：** 从当前可用最小组网闭环（hub 星形信令+WS 中继、WebRTC srflx 打洞、mesh node 全连通发现+loopback 网关、per-node HMAC 认证）演进到**完全组网**（普通家庭/办公/蜂窝 NAT 可直连、局域网/去中心化可发现、TCP 之外协议通达、hub 重启不失忆）。

**架构：** 复用现有分层（hub/mux/tunnel/mesh/xfer）；**阶段1 就抽「路由决策」单点**，阶段2 只填实现，避免重写架构。TURN 复用已引入的 pion/turn，不去发明新协议。扩展协议/依赖全部留在各自独立 go.mod，核心 go.mod 零新增三方依赖。

**技术栈：** Go 1.26；stdlib + `gopkg.in/yaml.v3` + `golang.org/x/*`；pion/webrtc（已含 pion/turn）、quic-go、coder/websocket 等重依赖仅存在于 `ext/*` 独立 module。

---

## 文件结构

| 动作 | 文件 | 职责 |
|------|------|------|
| 新建 | `docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md` | 路线图 spec（能力盘点 + 阶段划分 + DoD）（本 plan 的配套 spec，已在 PR #116） |
| 修改 | `pkg/tunnel/xfer/ext/webrtc/webrtc.go` | `--turn` 静态长时凭证写 `ICEServer.Credential`/`CredentialType` |
| 修改 | `cmd/sclient/{p2p,mesh,mesh_node,p2p_manual}.go` | `--turn` flag 解析与透传（默认空 = 不改变现状） |
| 新建 | `pkg/tunnel/hub/persist.go` | hub 状态 JSON 原子写：节点注册 `NodeInfo`、per-node secrets、信令收件箱、路由表；启动加载、变更落盘 |
| 修改 | `pkg/tunnel/hub/{router,signaling,mesh_route_table,route_table}.go` | 埋持久化钩子（注册/注销/信令入队出队时写盘） |
| 修改 | `cmd/sproxy/root.go`、`pkg/server/config.go` | 配置项 `hub.persist_file`（空 = 不启用）；装配时 `NewPersister` 加载/回写 |
| 新建 | `pkg/tunnel/mesh/mdns.go` | mDNS/DNS-SD：节点同网段广播自身（node-id + 服务+webrtc 信息），同段互发现，无需 hub 先连线 |
| 修改 | `pkg/tunnel/mesh/discovery.go` | 发现源从「仅 hub 列表」扩展为 hub 列表 + mDNS（`--mdns` 默认关或按网段开） |
| 修改 | `pkg/tunnel/hub/registry.go` | `DHTRegistry` 接线：允许外部把 `ext/kad` 挂上；路由表仍 hub 权威，DHT 只做节点发现（不改状态） |
| 修改 | `pkg/tunnel/hub/core.go` | DHT 接口稳定 + 文档指向 `ext/kad` |
| 修改 | `cmd/sproxy/root.go` | 入口装配 Kademlia（读配置/环境变量决定启用） |
| 新建 | `pkg/socks5/` | SOCKS5 server（相对独立、可测）；握手 + 转发走 mesh/relay 链路 |
| 修改 | `pkg/tunnel/mesh/gateway.go` | 网关暴露 SOCKS5（`--socks` addr）；`sclient socks` CLI 起代理 |
| 新建 | `pkg/tunnel/mux/datagram.go` | UDP datagram 帧类型（`FrameDatagram`）与端到端映射 |
| 修改 | `pkg/tunnel/mesh/node.go`、`pkg/tunnel/relay/leaf.go` | datagram 帧的路由/出口（`sclient udp map`） |
| 修改 | `pkg/tunnel/relay/`、`cmd/sclient/relay.go`、`cmd/sproxy/root.go` | TCP relay 独立使能（当前仅 WS） |
| 修改（阶段3） | 新增 `pkg/tunnel/hub/federation.go`、`pkg/server/hub_handler.go` | hub 联邦：多 hub 节点列表同步 + 跨 hub 转发 |
| 修改（阶段3） | `pkg/tunnel/ecdh.go` 附近 | 证书身份 + 指纹 pinning（不改变现有共享密钥+HMAC 架构） |
| 修改（阶段4） | `pkg/server/transfer*`（未来） | 文件同步/复制，接续 in-flight Web 传输管理器 |

---

## 任务清单（按阶段）

### 阶段1：P0 打通 Trunk（分支 `feature/mesh-p0-trunk`）

#### 任务 1.1：TURN 凭证注入

**文件：** `pkg/tunnel/xfer/ext/webrtc/webrtc.go`、`cmd/sclient/{p2p,mesh,mesh_node}.go`

- [ ] 步骤 1：`webrtc.go` 提供 `SetTURNServers(urls []string)`（解析 `turn:/turns:` URL + `?username=&credential=` 或独立凭证 flag，写 `ICEServer{URLs, Credential, CredentialType}`；凭据为空时回退当前仅 STUN 行为）
- [ ] 步骤 2：`cmd/sclient/*` 增加 `--turn <turn://user:pass@host:3478?...>`（可多次）与 `--turn-credential`；`defaultConfig` 组装 ICEServers 时并入
- [ ] 步骤 3：测试：`webrtc` 单测构造含凭据 ICEServer（不实际打 TURN）；CLI 解析测试

#### 任务 1.2：hub 状态持久化最小版

**文件：** 新建 `pkg/tunnel/hub/persist.go`、修改 `router.go`/`signaling.go`/`mesh_route_table.go`

- [ ] 步骤 1：`Persister` 封装 JSON 快照（原子写：tmp + rename；启动 load、变更 save；`sync.Mutex`/RWMutex 串行化）
- [ ] 步骤 2：`NodeInfo`（含 Secret）+ 服务声明 持久化；`RemoveIfOwned`/`Remove` 同步删账
- [ ] 步骤 3：信令收件箱（`SignalQueue`）持久化 —— 入队/出队/过期都落盘，hub 重启不丢 pending 信号
- [ ] 步骤 4：`pkg/server/config.go` 加 `hub.persist_file`（空=不启用）；`cmd/sproxy/root.go` 接通
- [ ] 步骤 5：测试：启动加载恢复节点、重启后信令不丢、`-race` 通过；快照文件损坏回退空态不 panic

#### 任务 1.3：验证 + 文档

- [ ] `make lint` 0 issues（主 + 每个子 go.mod 分别 lint）、`make build-all`、`make test-all`
- [ ] 双机（本地起 hub + 两台模拟对称 NAT + 自建/公共 TURN）`sclient p2p connect` 成功且数据面不落 hub
- [ ] PR：分支完成 → CI 通过 → 人工 squash 合并，合并信息聚焦功能维度

---

### 阶段2：P0/P1 去中心化发现 + 协议通达（分支 `feature/mesh-p1-protocols`）

#### 任务 2.1：mDNS/DNS-SD 局域网发现

**文件：** 新建 `pkg/tunnel/mesh/mdns.go`、修改 `discovery.go`

- [ ] 步骤 1：mdns 广播/监听自身（node-id + 服务 + webrtc 信令端信息），TTL/剔除
- [ ] 步骤 2：`discovery.go` 发现源扩展（hub 列表 + mDNS），`--mdns` 默认按网段开
- [ ] 步骤 3：测试：同段两节点不带 `--hub` 互发现并 `mesh connect` 直连

#### 任务 2.2：DHT 接线 `ext/kad` → `hub.DHTRegistry`

**文件：** `pkg/tunnel/hub/{registry,core,memdht}.go`、`cmd/sproxy/root.go`

- [ ] 步骤 1：稳定 DHT 接口（`bootstrapping func` 增加种子节点注入），`memdht` 保留为默认回退
- [ ] 步骤 2：`cmd/sproxy/root.go` 装配 Kademlia（读配置/环境变量）；路由表仍 hub 权威，DHT 只提供候选节点
- [ ] 步骤 3：测试：100 节点内存压力 + 候选节点去重/正确性

#### 任务 2.3：SOCKS5 代理

**文件：** 新建 `pkg/socks5/`、修改 `pkg/tunnel/mesh/gateway.go`、`cmd/sclient/gateway.go`

- [ ] 步骤 1：`pkg/socks5` server（握手 + CONNECT + BIND/UDP ASSOCIATE 按需），可单测
- [ ] 步骤 2：mesh 网关暴露 SOCKS5；`sclient socks [--gateway]` CLI
- [ ] 步骤 3：测试：`curl --socks5-hostname` 走对端出口；集成

#### 任务 2.4：UDP 隧道

**文件：** 新建 `pkg/tunnel/mux/datagram.go`、修改 `node.go`、`leaf.go`

- [ ] 步骤 1：mux datagram 帧（长度前缀 + 五元组 key），流式窗口不受影响
- [ ] 步骤 2：mesh/relay 出口映射 UDP `sclient udp map`（TCP 骨架复用）
- [ ] 步骤 3：测试：双向 UDP 转发确认

#### 任务 2.5：TCP relay 独立使能

**文件：** `pkg/tunnel/relay/`、`cmd/sclient/relay.go`、`cmd/sproxy/root.go`

- [ ] 步骤 1：hub 额外监听裸 TCP relay（复用已有注册/中继逻辑，仅换 transport `tcp`）
- [ ] 步骤 2：`relay start --transport tcp` 走裸 TCP
- [ ] 步骤 3：测试：无 WS 场景 relay dial 通

#### 任务 2.6：验证 + 文档

- [ ] `make lint` 0 issues、`make build-all`、`make test-all`
- [ ] 手测：mDNS 互发现直连、`curl --socks5-hostname`、`sclient udp map` 双向通

---

### 阶段3：P1 核心稳固（分支 `feature/mesh-p2-decentral` 等，按需）

- [ ] 证书身份 + 指纹 pinning（不改变现有共享密钥+HMAC 架构；现有 ECDH 握手保留）
- [ ] hub 联邦：多 hub 节点列表同步 + 跨 hub 转发（用户已确认保留）

### 阶段4：P2 铺开（按需）

- [ ] 虚拟 IP/子网分配（基于阶段1 预留扩展点）
- [ ] 文件同步/复制（接续 in-flight Web 传输管理器）
- [ ] 明确 SKIP：多跳链式中继（仅 hub 联邦时需要，defer 到 P3，不实现）

---

## 验证

- 各阶段完成时跑：`make lint`（0 issues，主 + 每个子 go.mod 分别）、`make build-all`、`make test-all`（-race）、`make test`（核心 fast）
- 阶段1 手测：本地起 hub + 两台模拟对称 NAT（自建/公共 TURN），`sclient p2p connect` 成功、数据面不落 hub；hub 重启节点自动重连、信令不丢；快照文件损坏回退空态不 panic
- 阶段2 手测：两节点同网段不带 `--hub` 仅 mDNS 互发现并 `mesh connect` 直连；`sclient socks` 后 `curl --socks5-hostname` 走对端出口；`sclient udp map` 双向通
- 文档化：每阶段实现时按 SDD 各出 `plans/` + `specs/` 与 `learnings/`；本文件与 spec 配对（`docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md`）已入 PR #116
