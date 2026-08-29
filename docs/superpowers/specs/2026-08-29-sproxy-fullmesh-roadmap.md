---
title: sproxy 完全组网发展规划（能力盘点 + 阶段划分）
status: planning
---

# sproxy 完全组网发展规划

> 日期：2026-08-29
> 分支：feature/mesh-fullmesh-roadmap
> 来源：由 plan `docs` 阶段（探索 + 用户决策）固化为长期路线图

## 1. 背景与目标

sproxy 经 #107–#115 多个迭代，已具备一套**可工作的最小组网闭环**：

- hub 星形信令 + WebSocket 中继（`pkg/tunnel/hub/`）
- WebRTC 打洞（`pkg/tunnel/xfer/ext/webrtc/`，仅 STUN srflx，默认 Google/Tencent/Xiaomi，`--stun` 覆盖）
- `mesh node` 常驻组网（全连通发现 + loopback 网关 + 直连链路池，`pkg/tunnel/mesh/`）
- SproxySig + per-node HMAC 认证（#111 已废除 relay_token）

**本规划的目标**（用户 2026-08-29 通过 `/plan` 确认方向与范围）：

1. 盘点当前组网能力（已实现 vs 缺口）；
2. 划分**近期（P0）**/**中期（P1）**/**远期（P2）** 阶段；
3. 明确每一阶段的 Definition of Done 与验证方式。

用户决策（AskUserQuestion）：

- 阶段1 全选：TURN 凭证注入 + hub 状态持久化 + mDNS 局域网发现 + DHT 接线；
- 阶段2 全选：SOCKS5 代理 + UDP 隧道 + TCP relay 独立；
- P2 保留：hub 联邦 + 虚拟 IP/子网分配（不 SKIP，已分别排进阶段3/4）；
- SKIP：多跳 chained relay（理由见 §4.4）。

---

## 2. 能力盘点

### 2.1 已实现

| 能力                           | 说明                                                                                                                 |
| ------------------------------ | -------------------------------------------------------------------------------------------------------------------- |
| hub 星形信令 + WS 中继         | `pkg/tunnel/hub/` + `xfer/ext/ws`；节点注册 `POST /api/signal/{offer,answer,candidate}` 信令桥                       |
| WebRTC p2p 打洞                | `ext/webrtc`（pion/webrtc v4）srflx + host candidate；默认 STUN Google/Tencent/Xiaomi，`--stun` 覆盖                 |
| mesh 全连通发现                | `pkg/tunnel/mesh/discovery.go` 周期拉 hub 节点列表、并行 webrtc 打洞、半拨号去重、已建链路池复用                     |
| mesh node 常驻 + loopback 网关 | `pkg/tunnel/mesh/node.go` + `gateway.go`（token 认证、已建链路复用）                                                 |
| per-node HMAC 密钥             | hub 在 `REG_OK` 签发 32B secret（存 `NodeInfo.Secret`），信令 `X-Node-Secret` 恒时比较、discovery 临时身份 HMAC 证明 |
| 加密层                         | 共享密钥 AES-256-GCM 隧道 + ECDH X25519 握手（无长时密钥对）                                                         |
| hub 管理 + 指标                | `GET /api/hub/*`（处理器 `pkg/server/hub_handler.go`）、Prometheus `sproxy_hub_nodes_connected`                      |

### 2.2 缺口

| 能力                                 | 状态   | 说明                                                                                                                         |
| ------------------------------------ | ------ | ---------------------------------------------------------------------------------------------------------------------------- |
| **TURN**                             | 无     | pion/turn 已在 `ext/webrtc` go.mod，但 `ICEServer.Credential` 从未赋值 → 对称 NAT 无法直连，只能回落 hub 中继（SPOF + 瓶颈） |
| **mDNS / DNS-SD**                    | 无     | hub 节点列表是唯一发现源；局域网内不连 hub 无法互发现                                                                        |
| **DHT / Kademlia**                   | 死代码 | `ext/kad` 完整实现 + 独立 go.mod，但无人 import；`hub.DHTRegistry` 默认 memdht 内存 map                                      |
| **多跳路由**                         | 无     | hub 是星形，mesh 是全连通直连图，无中间节点链式转发                                                                          |
| **hub 联邦**                         | 无     | 单 hub；无 hub-to-hub peering / 同步                                                                                         |
| **证书身份 / pinning / mTLS**        | 无     | 身份 = node-id 字符串 + HMAC；无长时密钥对                                                                                   |
| **UDP / SOCKS5 / 域路由 / 负载均衡** | 无     | relay 与 mesh 仅 TCP over WS（frp/chisel 对比已列）                                                                          |
| **虚拟 IP / 子网分配**               | 无     | Tailscale/ZeroTier 特点                                                                                                      |
| **文件同步 / 复制**                  | 规划中 | in-flight Web 传输管理器（2026-08-27 spec 未合并）承接                                                                       |
| **持久化**                           | 无     | hub 路由表 / 节点注册 / 信令收件箱 / per-node secrets 全内存，重启即清（share store 走独立文件，不在本表范围）               |

---

## 3. 阶段划分

**排序逻辑**：先修 TURN 而非发明新协议 —— TURN 是让现有 WebRTC + hub 中继架构走通普通家庭/办公/蜂窝 NAT 的唯一缺口，投入产出比最高。**阶段1 就抽「路由决策」单点，阶段2 只填实现** —— 避免重写架构（预留扩展点，不提前实现）。

| 阶段 | 优先级 | 主题         | 工作项                                                                                          | 目标文件                                                                                    | 粗估        |
| ---- | ------ | ------------ | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------- | ----------- |
| 1    | P0     | 打通 Trunk   | TURN 凭证注入（`--turn` 静态长时凭证写 `ICEServer.Credential`）                                 | `pkg/tunnel/xfer/ext/webrtc/webrtc.go`、`cmd/sclient/{p2p,mesh,mesh_node}.go`               | 1–2d        |
| 1    | P0     | 打通 Trunk   | hub 状态持久化最小版（节点注册/路由表/信令收件箱/per-node secrets 写 JSON，启动加载、变更落盘） | 新增 `pkg/tunnel/hub/persist.go`、`pkg/tunnel/hub/router.go`、`pkg/tunnel/hub/signaling.go` | 2–3d        |
| 1    | P0     | 打通 Trunk   | 持久化接入 `cmd/sproxy/root.go` hub 装配 + 配置项                                               | `cmd/sproxy/root.go`、`pkg/server/config.go`                                                | 1d          |
| 2    | P0     | 去中心化发现 | mDNS/DNS-SD 局域网发现                                                                          | `pkg/tunnel/mesh/discovery.go` + `pkg/tunnel/mesh/mdns.go`（新增）                          | 1–2d        |
| 2    | P0/P1  | 去中心化发现 | DHT 接线 `ext/kad` → `hub.DHTRegistry`（路由表仍 hub 权威; DHT 只发现不改状态）                 | `pkg/tunnel/hub/registry.go`、`core.go`、`cmd/sproxy/root.go`                               | 3–5d        |
| 2    | P1     | 协议通达     | SOCKS5 暴露在 mesh 网关                                                                         | `pkg/tunnel/mesh/gateway.go` + 新增 `pkg/socks5`                                            | 2–4d        |
| 2    | P1     | 协议通达     | UDP 隧道 / 端口映射（当前仅 TCP）                                                               | `pkg/tunnel/mux/` 新增 datagram 帧 + `pkg/tunnel/mesh/node.go`                              | 2–4d        |
| 2    | P1     | 协议通达     | TCP relay 独立使能（当前仅 WS 接入注册）                                                        | `pkg/tunnel/relay/` + `cmd/sclient/relay.go` + `cmd/sproxy/root.go`                         | 1–2d        |
| 3    | P1     | 核心稳固     | 证书身份 + 指纹 pinning                                                                         | `pkg/tunnel/ecdh.go` 附近新增; `cmd/sclient`                                                | 2–3d        |
| 3    | P1     | 核心稳固     | hub 联邦（用户要求保留）                                                                        | 新增 `pkg/tunnel/hub/federation.go`、`pkg/server/hub_handler.go`                            | 5–10d       |
| 4    | P2     | 铺开         | 虚拟 IP / 子网分配（用户要求保留）                                                              | 基于阶段1 预留扩展点                                                                        | 5–10d       |
| 4    | P2     | 铺开         | 文件同步/复制 → 与 in-flight Web transfer mgr 接续                                              | `pkg/server/transfer*`（未来）                                                              | 3–5d        |
| —    | SKIP   | —            | 多跳 chained relay                                                                              | 全连通 mesh 已覆盖; 仅 hub 联邦时需要                                                       | defer 到 P3 |

---

## 4. 说明

### 4.1 为什么 P0 先修 TURN 而非发明新协议

TURN 是让现有 WebRTC+hub 架构走通对称 NAT 的唯一缺口，复用已引入的 pion/turn 依赖，投入产出比最高。

### 4.2 为什么 mDNS/DHT 放阶段2

两者只对「不经过中心 hub 的节点间发现」有意义; 阶段1 先把 hub 状态持久化 + 单机最小值，阶段2 把节点表从 hub 解放（DHT 只做发现，路由表仍 hub 权威）。

### 4.3 SOCKS5/UDP/TCP-relay 为何排阶段2

属于 last-mile 的传输多态，不做多跳 P0/P1 也能成立（frp/chisel 对标）。

### 4.4 SKIP 理由：多跳 chained relay

- 全连通 mesh 已是最优拓扑（任何两节点间直接 P2P 连接拥有一条最短路）;
- hub 中继兜底已覆盖「无法直连」场景; 多跳只在 hub 联邦 + 「A→hub→B→hub→C」的链式路径时需要（P3）。
- 价值/复杂度比低，defer 到 P3 明确不实现。

---

## 5. Definition of Done（每阶段）

### 阶段1

- 双机跨对称 NAT 直连成功（TURN 生效）;
- hub 重启后节点自动重连、信令收件箱不丢消息;
- 持久化测试过 `-race`; `make lint` 0 issues;
- 产物：code（feature/mesh-p0-trunk）+ roadmap 文档核对。

### 阶段2

- 局域网内不连 hub 也能全连通（mDNS）;
- DHT 接线后节点发现不再单依赖 hub; 100 节点内存压力测试通过;
- SOCKS5 / UDP / TCP-relay 各有 CLI 命令 + 集成测试;
- 扩展协议/依赖全部留在各自独立 go.mod; 核心 go.mod 零新增三方依赖.

### 阶段3-4

- 阶段3（证书 pinning + hub 联邦）按需实现; 阶段4 与 in-flight Web transfer mgr 接续。

## 6. 文档与分支

- 每个里程碑（阶段 1/2/3/4）各自独立**特性分支**;
- 分支完成 + 本地 `make lint` 0 issues + `make test-all` 全绿 → 开 PR; **CI 通过后由人工 squash 合并回 master**;
- 合并信息聚焦**具体功能维度**（如 `feat: P0 trunk——TURN 凭证 + hub 持久化`），不罗列 commit 数量等过程数据; 风格对齐现有 PR 标题（如 `feat: ...（#116）`).

## 7. 验证

每个阶段完成时跑 `make lint` + `make build-all`（所有子 module）+ `make test-all`（-race）+ `make test`（核心 fast）;
手测样例:

- 阶段1：本地起 hub + 两台模拟对称 NAT（自建/公共 TURN），`sclient p2p connect` 成功，且数据面不再落 hub;
- 阶段2: mDNS — 两节点同网段不带 `--hub` 互发现并 `mesh connect` 直连;
  SOCKS5 — `sclient socks` 后 `curl --socks5-hostname` 走对端出口; UDP — `sclient udp map` 双向确认.
