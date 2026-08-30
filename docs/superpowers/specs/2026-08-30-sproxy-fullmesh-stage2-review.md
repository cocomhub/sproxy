---
title: sproxy 完全组网·阶段 2 复盘（去中心化发现 + 协议通达）
status: review
---

# sproxy 完全组网·阶段 2 复盘

> 日期：2026-08-30
> 范围：PR #119–#123（master `837e094` → `614dc1a`，5 个 squash 合并）
> 规模：64 files，+7587 / −84（累计阶段 2 delta）
> 前置：阶段 1（#116 路线图、#117 TURN 凭证、#118 hub 持久化）已完成
> 来源：5 份子任务 learnings（`docs/superpowers/learnings/2026-08-30-stage2-{mdns,dht,socks5,udp,tcprelay}.md`）+ 各 PR 审查/CI 记录

## 1. 阶段目标与范围

路线图（`2026-08-29-sproxy-fullmesh-roadmap.md`）阶段 2 定义两路工作：

| 维度 | 工作项 | 优先级 |
|------|--------|--------|
| 去中心化发现 | mDNS/DNS-SD 局域网发现（不经 hub 互发现） | P0 |
| 去中心化发现 | DHT 接线：`ext/kad` → `hub.DHTRegistry`（路由表仍权威，DHT 只发现） | P0/P1 |
| 协议通达 | SOCKS5 代理暴露在 mesh 网关 | P1 |
| 协议通达 | UDP 隧道 / 端口映射（当前仅 TCP） | P1 |
| 协议通达 | TCP relay 独立使能（当前仅 WS 接入注册） | P1 |

**核心约束（全程守住）**：核心 `go.mod` 零新增三方依赖（仅 stdlib + `golang.org/x/*` + `gopkg.in/yaml.v3`）；扩展协议/依赖留在各自独立子 module。

## 2. 交付总览

| # | 子任务 | 分支 | PR | 核心能力交付 | DoD 达成 |
|---|--------|------|----|--------------|----------|
| 1 | mDNS/DNS-SD 局域网发现 | `feature/mesh-p2-mdns` | #119 | 纯 mDNS 无 hub 节点互发现 + 直连信令（TCP SDP 交换替代 hub）+ `mesh connect --mdns` | ✅ 两节点 --mdns 自动互发现 + 出口拨号 echo |
| 2 | DHT 接线 | `feature/mesh-p2-dht` | #120 | 死代码 `ext/kad` 接入 `hub.DHTRegistry`；注册喂 DHT、`/api/hub/nodes` 合并候选 | ✅ 100 节点压力通过；发现不再单依赖路由表 |
| 3 | SOCKS5 代理 | `feature/mesh-p2-socks5` | #121 | `pkg/socks5` 库 + `sclient socks --exit` + `mesh node --socks` | ✅ 官方 x/net/proxy 客户端经 mesh 到出口 echo + SSRF 边界 |
| 4 | UDP 隧道 | `feature/mesh-p2-udp` | #122 | mux `FrameDatagram` + `sclient udp map` 双向 UDP 端口映射 | ✅ 双向转发 -race 稳定 + 真实二进制 echo |
| 5 | TCP relay 独立使能 | `feature/mesh-p2-tcprelay` | #123 | hub 裸 TCP 中继 + `relay start --transport tcp` | ✅ 无 WS 纯 TCP relay dial 通（自动 + 真实二进制） |

路线图阶段 2 DoD 全量核对：

- ✅ 局域网内不连 hub 也能全连通（mDNS）——#119 两节点互发现直连；
- ✅ DHT 接线后节点发现不再单依赖 hub；100 节点内存压力测试通过——#120 kad + 内存 DHT 双压力；
- ✅ SOCKS5 / UDP / TCP-relay 各有 CLI 命令 + 集成测试——#121/#122/#123；
- ✅ 扩展协议/依赖全部留在各自独立 go.mod；核心 go.mod 零新增三方依赖——仅 `golang.org/x/net` 提升为直接依赖（x/net 系列属可用，非三方）。

## 3. 各子任务复盘

### 3.1 mDNS/DNS-SD 局域网发现（#119）

**能力**：`_sproxy-mesh._tcp.local.` 服务类型下组播广播 node-id + 服务宣告 + 直连信令端点；同网段节点不经 hub 互发现，经直连信令（TCP `[4B len][JSON]` 交换 SDP offer/answer）建立 webrtc 数据面。纯 stdlib + `x/net/dns/dnsmessage` 实现，不引第三方 mDNS 库。

**关键决策**：
- 发现源解耦：mDNS 广播/浏览独立于 hub 节点列表，`--mdns` 纯无 hub 模式与 hub 模式共存；
- mDNS 发现拨号沿用 `disc-` 临时身份 + `parseDiscoveryPeerID` 恢复真实 base（full-mesh 对称）；LAN 信任模型下 base 无 hub 强制校验，保留半拨号序校验作纵深；
- `--mdns-secret` 复用 AK/SK（SK），两个 HMAC 加协议域前缀（`sproxy-mdns-signal/v1`、`sproxy-mdns-txt/v1`）防跨协议混淆。

**审查发现（修复）**：
- F1（必须）畸形 TCP 连接杀整节点（远程 DoS）→ 哨兵 `errDirectSignalConn` + per-connection 非致命 + ctx 感知读 + 关闭遗留连接；
- F2（必须）信令端口默认 `:0` 全接口绑定、暴露面与广播不一致 → `resolveSignalListenAddr` 收敛到主局域网 IP；
- S1/S3/S5 + 复审 N1/N2/N4/N5（建议/参考，全部修复）；N3（slowloris 有界阻塞）记录不修。

**安全闭环**：4 条 MEDIUM 全处理——`--mdns-secret` 签名认证（防未授权 peer 借节点出站）、`ValidateSignalAddr` 拒 SSRF（loopback/link-local/169.254.169.254/multicast/unspecified）、TXT HMAC 防伪造、listener 绑 LAN IP 而非 loopback（mDNS 语义必需）。

**Windows 收敛**：`SetMDNSLoopbackOnly` 测试钩子使组播回环收敛，消除防火墙弹窗；组播回环默认关经 setsockopt `IP_MULTICAST_LOOP` 开启。

### 3.2 DHT 接线（#120）

**能力**：`ext/kad`（独立 go.mod 的 Kademlia）实现 `hub.DHT` 接口接入插件注册表；`HubServer`/`Handlers` 经 `SetDHT` 显式注入；注册喂入 `PeerInfo{ID, Addrs, Meta{mesh,addr}}`，`/api/hub/nodes` 按 mesh 过滤 + 去重合并候选（路由表权威优先）。

**关键决策**：
- DHT **只作候选节点来源**，不改路由表状态（转发/信令/持久化仍路由表权威）；
- 默认不启用（`dht=nil`），仅 `hub.dht: kad` 由 root.go 装配；测试用显式注入避免全局单例污染；
- `ext/kad` 留在独立 go.mod，`cmd/sproxy`（独立 module）经 require + replace 接线，核心 go.mod 零污染。

**审查发现（修复）**：
- #1（必须）mesh 隔离破坏：`mergeDHTNodes` 默认 mesh（`cm==""`）候选泄漏给所有 mesh → 严格相等 `cm != mesh`；
- #2（必须）disc-/mesh-/p2p- 瞬态身份喂入 DHT 成幽灵节点挤占 k-bucket → `isTransientNodeID` 过滤；
- #3（建议）断开/踢出不从 DHT 移除 → `DHT.Remove(ctx, nodeID)` 三实现补齐；
- #4（建议）`dht_seeds` Bootstrap 插假 ID 幽灵节点（多 hub 未实现）→ 移除调用，配置预留 + 启动 Warn；
- #5/#6/#9（建议/参考）`DHTRegistry.Active()` 作实际选择机制、未知 `hub.dht` 值拒绝、`Connected` 加 `omitempty`。

### 3.3 SOCKS5 代理（#121）

**能力**：`pkg/socks5` 库（RFC 1928 握手 + RFC 1929 认证 + CONNECT + 双向泵送，`Dial`/`Auth` 注入解耦）；`sclient socks --exit` 经 mesh 到出口节点拨号；`mesh node --socks` 本地 SOCKS5 出口。DoD 用官方 `x/net/proxy` 客户端验证服务端。

**关键决策**：
- 安全边界对齐 mesh 网关：监听默认 loopback（`NormalizeListenAddr`），可选 RFC 1929 认证，SSRF 边界在出口节点 dial 策略（`NewServiceDialPolicy`）；
- mesh 路由 Dial：`--gateway` 优先复用已建链路 → 回落常规拨号；`--mdns` 走直连信令；否则 hub 模式（webrtc + relay 回落）；
- `iostream.Pump(conn, target, PumpGrace)` 复用既有泵送（Abort 优先收尾 + grace 强制关闭）。

**审查发现（修复）**：
- M1（必须）手写 pump 无限阻塞（keep-alive 连接永久持有）→ 复用 `iostream.Pump` + grace；
- S1（建议）`--webrtc=false`/注册失败时 relay-only 回落被 guard 阻断 → 改 `svc==nil`；
- S2（建议）`--socks` 端口冲突崩溃 → Warn + 降级继续运行；
- S3（建议）root go.mod 被 tidy 引入 webrtc/pion → 还原 + 手动仅提升 x/net；
- I1/I3/I2/I5（参考）nmethods==0 回 0xFF、RSV 校验、`ConstantTimeCompare`、user/pass 任一设即启用认证。

**安全闭环**：未认证开放代理风险 → 默认 loopback + 可选认证 + 出口 dial 策略把关目标。

### 3.4 UDP 隧道（#122）

**能力**：mux `FrameDatagram`（0x08，负载 `[4B flowID][datagram]`，isRaw 直发不重传）正交于流；`sclient udp map` 本地 UDP 端口 → mesh（webrtc mux + datagram）→ 出口节点 `handleUDPMap` 转发到远程 UDP 目标，响应原路回传。

**关键决策**：
- 出口 UDP 处理在 `relay.Serve`（与 dial 帧同属出口模式，`--dial-allow` 门控），单 UDP 协程串行化消除 handler/读/关闭并发竞态；
- 数据报经 writeCh 串行化（与流共享单写者），`SendDatagram` 加 default 丢弃（UDP 语义）；
- hub 中继回落暂不支持 UDP（webrtc 路径满足 DoD）。

**审查发现（修复）**（本轮最重，多轮对抗 + 整体 code-review）：
- M1（阻断提交）`datagram.go` 被 `.gitignore` 的 `data*` 规则吞掉（提交后编译失败）→ 改名 + 收窄 `.gitignore` 为 `data/`；
- M2（必须，SSRF）出口 `handleUDPMap` 未过 `DialAllowed`/`NewServiceDialPolicy`，`--dial-allow` 节点可被当任意内网 UDP 代理 → `dialPolicy` 传入 + 解析校验目标；
- M3（必须，泄漏）UDP 协程优雅关闭不退出（+2 goroutine +1 FD）→ `stop` 信号；
- H1（必须）Ctrl+C 收尾死锁 → `control.Abort()` 非阻塞 + defer 顺序 m.Close 先；
- H2（必须）客户端静默永久挂起 → 监听 `m.Done()` 报错退出；
- H3（必须）`SendDatagram` writeCh 满阻塞（双向头阻塞）→ default 丢弃 + log+continue；
- H4（必须）出口 sendCh 饿死 → 单协程阻塞读吞写路径 → 并发读协程 + 写路径 + mutex 串行化 Close；
- H5（必须）`sendFrame` isRaw 失败关整个 mux → `writeMsg.datagram` 标记，数据报失败只丢弃；
- H6/H7/H8/H9/H10（建议）多源响应丢包（DialUDP→ListenUDP+WriteToUDP）、读错误白名单不对称、udp map 收尾无界、svcErr 被吞、帧字段嗅探歧义（统一解析 + 仅命中一种分派）。
- 补回归测试 H5/H6/H7/H10 + `mockxfer.MockConn` 计数器加锁。

### 3.5 TCP relay 独立使能（#123）

**能力**：hub 在 WS 中继之外支持裸 TCP 中继——`HubServer.ListenTCP/AcceptTCP`（复用 HandleConn 注册/鉴权/中继，ws/tcp 共用同一 HubServer）；`relay start --transport tcp`；无 WS 场景 `relay dial` 通。注册/鉴权/中继/信令协议零改动，仅 `xfer.Conn` 载体不同。

**关键决策**：
- `xfer/internal/tcp` 的 internal 可见性 → 新建 `pkg/tunnel/xfer/builtin` 对外注册桥（blank import），任何包 `import _ "pkg/tunnel/xfer/builtin"` 即得 `xfer.Get("tcp")`；
- 裸 TCP 网络面健壮性三补：Receive 尊重 ctx deadline、单条 1 MiB 上限、Send 写超时 60s；
- 安全边界：`hub.transports.tcp.listen` **默认 loopback `127.0.0.1:18084`**（远程可达需显式配置）+ 与主 HTTP addr 端口冲突校验 + fail-closed 认证（未配置 access_keys 拒绝注册）。

**审查发现（修复）**：
- I1（必须，帧对齐）超长帧错误不断连 → mux readLoop 重试但残余字节破坏帧对齐 → `failConn()` 断连 + 返回 `xfer.ErrConnClosed`；
- I2（必须，flaky）并发注册测试与异步移除竞争 → 保持连接存活再断言；
- S1（建议）accept 孤儿连接 FD 泄漏 → `select { connCh <- c; <-closeCh: c.Close() }`；
- S2（建议）`--transport tcp` 传 ws:// URL 报错难懂 → 显式检测 + 清晰提示；
- S3（建议）拨号写阻塞叶子挂起 → `targetMux.Open` 受 12s 超时约束；
- S4/S5（建议）阻塞读测试改 cancel-only ctx、配置注释更新。

**安全闭环（协调者 MEDIUM 反馈）**：`DefaultHubTCPListen = ":18084"` 全接口绑定 → 改默认 loopback + 端口冲突校验 + 注释/文档说明安全边界。

**未修复（文档化取舍）**：明文 TCP 无 TLS（WS 可用 WSS；loopback 默认缓解本机暴露，远程 WAN 应 WSS 或 VPN）；未认证连接占槽 10s（loopback + fail-closed 已缓解）。

## 4. 能力矩阵更新（对照路线图 §2）

### 已实现（阶段 1，本轮前置）

| 能力 | 说明 |
|------|------|
| TURN 凭证注入 | `--turn` 静态长时凭证写 `ICEServer.Credential`，对称 NAT 可直连 |
| hub 状态持久化 | 节点/路由/信令收件箱/per-node secrets 写 JSON，启动加载、变更落盘、重启不失忆 |

### 已实现（阶段 2，本轮新增）

| 能力 | 说明 |
|------|------|
| mDNS / DNS-SD 局域网发现 | `_sproxy-mesh._tcp.local.` 组播广播/浏览，同网段不经 hub 互发现 |
| DHT / Kademlia 接线 | 死代码 `ext/kad` 接入 `hub.DHTRegistry`，节点发现不再单依赖 hub（路由表仍权威） |
| SOCKS5 代理 | `sclient socks --exit` + `mesh node --socks`，RFC 1928/1929 |
| UDP 隧道 / 端口映射 | `sclient udp map`，mux FrameDatagram 双向转发 |
| TCP relay 独立使能 | hub 裸 TCP 中继，`relay --transport tcp`，无 WS 场景可中继 |

### 剩余缺口

| 能力 | 状态 | 排期 |
|------|------|------|
| 证书身份 / 指纹 pinning / mTLS | 无 | 阶段 3（可选） |
| hub 联邦 | 无 | 阶段 3（较大，建议拆子任务） |
| 虚拟 IP / 子网分配 | 无 | 阶段 4 |
| 文件同步 / 复制 | 规划中 | 阶段 4（与 Web transfer mgr 接续） |
| 域路由 / 负载均衡 | 无 | 未明确排期 |
| 多跳 chained relay | 无 | SKIP（P3 按需） |
| TCP 传输 TLS | 无 | 取舍文档化，未来工作 |

## 5. 跨子任务共性经验

1. **核心 go.mod 零三方依赖纪律**：`go mod tidy` 会因预存测试把 webrtc/pion 等拉进根 go.mod（#121/#123 两次踩坑）。正确做法：手动编辑 go.mod，仅把 `golang.org/x/net` 提升为直接依赖，不跑 tidy。
2. **`.gitignore` 吞源码**：宽泛 `data*` 规则吞掉 `datagram.go`（提交后编译失败，#122）。新文件命名避开宽泛模式，push 前 `git status` 确认新文件被跟踪。
3. **Windows 生态**：测试监听一律 127.0.0.1（`make check-loopback` 扫 `0.0.0.0` 字面量，改 `net.IPv4zero.String()`）；组播回环默认关需 setsockopt；mDNS 组播测试用 `SetMDNSLoopbackOnly` 收敛免防火墙弹窗。
4. **Go 1.26 陷阱**：`omitempty` 对 `time.Time` 无效 → `omitzero`；`dnsmessage` TTL 在 `ResourceHeader` 不在 body。
5. **并发模式沉淀**：单协程串行化 + 短 deadline 让出（UDP 单协程死锁）、RWMutex 保护 handler 槽、`atomic.Bool` 防 closed 竞态、goroutine 必须监听 ctx.Done() 防泄漏（收尾死锁/静默挂起是本阶段审查高频缺陷）。
6. **对抗式审查价值极高**：每子任务都实证复现必须级缺陷——畸形连接 DoS（#119）、mesh 隔离破坏 + DHT 幽灵节点（#120）、pump 无限阻塞（#121）、SSRF 绕过拨号策略 + 帧对齐破坏 + 双向头阻塞 + 饿死单向化（#122/#123）。流程不可省。
7. **安全边界模式**：默认 loopback、fail-closed 认证、SSRF 边界在出口 dial 策略、共享密钥 HMAC 加协议域前缀防跨协议混淆。
8. **DoD 双保险**：自动化（in-process + CLI 级 + `-race` 连跑 3 次）+ 手动真实二进制验证（curl/UDP echo/relay dial 往返）。
9. **子代理开发模式**：全新上下文子代理 + 交接报告 + 踩坑清单传递；同一文件的修改不并行。

## 6. 质量与依赖

- **CI**：5 个 PR 全部 Build×6 / Lint / Test（ubuntu+windows）/ UI E2E / SonarQube 全绿（#123 仅 Benchmark 为慢速信息性 job）；
- **独立对抗式审查**：每子任务必跑，必须级 + 建议级 + 参考级全部修复复验，无未解决 Critical/Important；
- **自动化安全审查**：MEDIUM 发现全部闭环（mDNS 认证/SSRF、SOCKS5 开放代理、UDP 出口策略、TCP 默认 loopback）；
- **依赖**：核心 go.mod 仅 `yaml.v3 + x/sys + x/crypto + x/net`，零三方新增；全模块（10 子 module）`make build-all`/`test-all`（-race）绿；
- **规模**：64 files，+7587 / −84；每子任务独立 feature 分支 → PR → CI 通过 → 人工 squash 合并，合并信息聚焦功能维度。

## 7. 对阶段 3 的启示与建议

阶段 3（P1 核心稳固）剩余两项，规模差异大，建议分拆：

1. **证书身份 + 指纹 pinning**（2–3d，可选）：
   - 不改变现有共享密钥 + HMAC 架构，保留 ECDH 握手；
   - 引入长时密钥对 + 身份指纹，pinning 到对端公钥，防 MITM；
   - 直接复用本阶段沉淀的安全边界模式（默认 loopback、fail-closed、协议域 HMAC）。

2. **hub 联邦**（5–10d，较大）——建议拆两个子任务，各自独立 PR：
   - 子任务 A：联邦节点列表同步（hub-to-hub peering + 节点表交换 + 去重）；
   - 子任务 B：跨 hub 转发路径（A→hub1→hub2→B 链式中继，此时多跳 chained relay 才需要，路线图 SKIP 项按需激活）。
   - 前置依赖：`dht_seeds` 预留的多 hub 引导正好承接；注意联邦节点接入同样走 fail-closed 认证 + 默认 loopback 安全面。

3. **流程沿用**：每子任务同款「全新上下文子代理 + TDD + 对抗式审查 + 修复全部含 Minor + CI + 人工 squash 合并 + learnings」；阶段 3 各子任务从 `origin/master` 拉分支。

## 8. 结论

阶段 2 五路工作全部按路线图 DoD 达成并合入 master（`614dc1a`），核心依赖纪律与安全边界全程守住，无未解决 Critical/Important。组网能力从「hub 星形 + webrtc 打洞」扩展到「多发现源（hub/mDNS/DHT）+ 多协议通达（TCP/WS/UDP/SOCKS5/TCP-relay）」。阶段 2 复盘为阶段 3（证书 pinning + hub 联邦）提供衔接基础；阶段 3 启动时机与拆分由用户确认。
