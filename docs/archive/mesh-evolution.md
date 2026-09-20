<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Mesh 组网与传输演进复盘（2026-08 阶段 2–4）

> 来源：`feature/mesh-p2/p3/p4` 系列子任务复盘（mDNS / DHT / SOCKS5 / UDP / TCP relay /
> hub 联邦 / 虚拟 IP / 文件同步），均已实现并合入 master。
> 本文记录**跨子任务复用的架构决策与踩坑**，具体实现细节以代码为准。

## 1. 子任务总览

| 子任务 | 关键交付 | 架构要点 |
|--------|----------|----------|
| 阶段 2·mDNS | 局域网发现 + 直连信令 | 纯 stdlib + x/net/dns/dnsmessage 实现；Windows 组播回环需 setsockopt |
| 阶段 2·DHT | ext/kad 接线 hub.DHTRegistry | **DHT 只作候选节点来源，路由表仍权威** |
| 阶段 2·SOCKS5 | `sclient socks` + 节点本地出口 | RFC 1928/1929；Dial/Auth 注入与传输解耦 |
| 阶段 2·UDP | `sclient udp map` 端口映射 | mux FrameDatagram 数据报帧（尽力而为不重传） |
| 阶段 2·TCP relay | 裸 TCP 中继独立使能 | xfer/builtin 注册桥；hub 复用 HandleConn |
| 阶段 3·联邦 A | hub-to-hub 节点表同步 | 联邦只做发现不改路由表；入站端点防同步环路 |
| 阶段 3·联邦 B | 跨 hub 链式中继（1 跳） | 复用对端 /api/relay/stream CONNECT；防环 + 故障转移 |
| 阶段 4·文件同步 | pkg/sync + HTTPTransport + SyncManager + CLI + Web | 单向文件级增量；Executor 接口打破测试环 |
| 阶段 4·虚拟 IP | Tailscale 风格寻址 + 端口白名单 | hub 权威分配 + mDNS 确定性回落；REG_OK 下发 VIP |

## 2. 反复出现且必须遵守的安全边界模式

（跨所有子任务验证有效的默认值，写新网络功能时直接套用）

1. **新监听默认 loopback**（`NormalizeListenAddr`：裸 :port → 127.0.0.1:port）；远程可达需显式配置
2. **fail-closed 认证**：未配置凭据时拒绝注册/访问，绝不静默放行
3. **SSRF 边界在出口拨号策略**：`NewServiceDialPolicy` 拒绝 loopback/私网目标，除非宣告为服务地址
4. **mesh 隔离严格相等比较**：`c.Mesh != mesh`（空 mesh 只对默认 mesh 放行——曾踩「默认 mesh 候选泄漏」）
5. **出站 TLS 阶段显式约束**：`tls.Dialer.Timeout` 不覆盖 TLS 握手 → 先裸 TCP Dial + socket deadline，再 `tls.Client.HandshakeContext`
6. **明文传输限制**：sync_remotes / 联邦 peer 的明文 http 仅限 loopback（AK/SK 明文上线不可远程）

## 3. 关键架构决策（含教训）

### 发现源与路由表解耦
- DHT / 联邦 / mDNS 都是**候选节点来源**，`MeshRouteTable` 仍权威（转发/信令/持久化）
- 入站联邦端点只返回本 hub 路由表节点（不合并候选），否则 A 拉 B、B 又拉 A 无限回声

### 防环 / 去重必须自洽，不依赖可选配置
- 联邦 B 第一版防环仅靠 `node_id` 追加 self ID，但 `node_id` 默认空 → 路径恒空 → 防环退化为跳数上限
- 修复：恒追加「下一跳 peer.ID」（与对端解析命名空间一致），配置了 node_id 时额外追加 self ID
- **教训：机制若依赖「可选配置」，默认配置下会静默失效——要么给合理默认，要么让机制自洽**
- 同类：虚拟 IP 的 REG_OK 能力位必须随注册 ACK 下发（不依赖 discovery 环，Discover=false 的出口节点才不静默失效）

### VipTable 防注入
- hub 权威模式每次刷新**原子重建**（清陈旧）；mDNS 模式校验声明 VIP == 确定性哈希计算值；first-writer-wins 防劫持；子网外拒绝
- 出口 DialPolicy 命中顺序：ServiceAddrs 精确匹配**先**于虚拟子网判断（否则真实 CGNAT 流量被遮蔽）

### 跨 hub 转发复用 CONNECT 而非新协议
- 转发请求 = 与客户端一样的 `/api/relay/stream` CONNECT，目标 hub 变为对端 → 复用全部认证/拨号策略/错误语义
- 防环命中的 508 **不阻断故障转移**（每个 Forward 独立防环，其它对端可成功）

### 模块边界与依赖方向
- pkg/server 生产代码不 import pkg/client（client 的 e2e_test import server 构成测试编译环）→ 接口 + 实现包打破
- 核心 go.mod 零三方新增；ext/kad 留在独立 go.mod（cmd/sproxy require + replace 接线）

## 4. 逐子任务关键坑速查

### mDNS
- Windows 组播回环默认关闭：`UDPConn.SetMulticastLoopback` 已移除 → SyscallConn + 平台 setsockopt
- 信令端口默认绑全接口暴露面 ≠ 广播地址 → 通配 host 收敛到主局域网 IPv4
- 信令端口收到畸形 TCP 连接不得杀整节点 → per-connection 瞬时失败 continue
- 直连信令/宣告加可选共享密钥（HMAC + 协议域前缀防跨协议混淆）；默认 LAN 信任模型
- 多网卡环境字典序选 IP 会广播不可达地址 → 优先默认路由出口 IP

### DHT
- kad 路由表有界（k-bucket 每桶 20）：100 节点随机分布会溢出 → 压力测试验证有界候选正确性而非全量驻留
- 瞬态节点（disc-/mesh-/p2p-）不得喂入 DHT（幽灵节点永久残留挤占 k-bucket）→ isTransientNodeID 过滤
- 断开/踢出节点需从 DHT 移除（接口加 Remove）

### SOCKS5
- mesh relay 无结果帧，握手先回「成功」再泵送 → SSRF 边界测试断言「CONNECT 后读 EOF」而非 Dial 报错
- pump 无 grace 强制收尾 → goroutine/FD 泄漏 → 复用 iostream.Pump（PumpGrace）

### 正向 HTTP 代理（http-proxy，2026-09-20）
- 协议库（pkg/httpproxy）与 pkg/socks5 同模式：Dial 注入解耦传输层，**转发用 http.Client 恒设
  Transport.Proxy=nil 防环回**（否则代理自身出站会去走 http_proxy 环境变量指向自己）——比
  「手动 req.Write + ReadResponse 不读环境变量」更隐式，但库形态须显式关闭
- 本地直连优先路由（mesh.NewLocalOrExitDial）：网络好直连（有界超时 3s）失败回退出口；
  **exitDial 错误必须向上传播**（吞错回退本地会导致 banner 显示出口但流量走本地，误导）；
  --exit-only 禁用本地先试
- 自动选出口（NewAutoExitDial）：候选判据用 **Capabilities 含 outbound-dial**（hub 已透出
  Capabilities 字段；设计初稿的 Tags:["exit"] 注册帧 Meta.Tags 未透出到 /api/hub/nodes，实证修正）
- exit-exclude 排除名单：被排除节点不作出口但仍可被 SmartDial via-node 选中转（中转≠出口，能力独立）
- Go 标准库 httpproxy.proxyForURL 对 host=="localhost" 与 loopback IP **恒直连不走代理**：
  e2e 验证 http_proxy 环境变量生效必须用非回环目标或断言 407（请求抵达代理的确定性证据），
  回环目标下 env 代理测试必假绿
- 多命令共享 mesh 连接参数组收敛到 cmd/sclient/internal/meshconn（socks/udp/mesh/http-proxy）：
  AddFlags 拆 AddExitFlags——mesh connect 是服务名寻址，--exit 族语义不同不暴露；
  从 FromFlags 按 Lookup(flag) 门控读取，零回归有测试守护
- mesh 集成测试改包级全局（SetHostOnly）不可并行 → serial_budgets.tsv 显式登记（对齐 mesh_socks5）

### UDP
- 单协程 `select{<-sendCh; default}` + 阻塞 conn.Read 死锁 → 读加短 deadline 周期性让出
- 数据报发送失败只丢弃**不关 mux**（isRaw 直发失败杀掉同 mux TCP 流）
- 出口 UDP 目标必须过 dial 策略（M2 曾绕过 → SSRF 转发代理）
- `.gitignore` `data*` 宽模式吞掉 datagram.go → 重命名文件 + 收窄 ignore 规则

### TCP relay
- internal 包无法被外部引用 → `xfer/builtin` 注册桥
- TCP Receive 原实现忽略 ctx：注册帧 10s 超时无效 → ctx deadline 映射 socket 读 deadline
- 超长帧错误不断连 = 帧对齐破坏 → failConn 关闭 + ErrConnClosed 包装

### 文件同步
- 同卷 HTTPTransport 单连接串行分块 + 文件级并发（MaxConnsPerHost=1）
- http.Transport HTTP/1.1 从不调 conn.SetDeadline → 写路径对端停读永久阻塞 → 活跃写超时（每次 Write timer 监督）
- MuxStreamConn.Close 可能经 writeCh 阻塞 → forceClose 探测 Abort() 接口
- 单一共享 timer 跨方向串扰 → 拆 rdTimer/wdTimer
- LocalFS confine（EvalSymlinks 逐级解析 + 前缀校验）防 symlink 逃逸，覆盖全部文件操作

### 联邦
- TLS 校验收紧连锁：NewFederationClient 返回 (client, error) 波及全部调用点；批量 sed 后必须 go build 兜底
- config 校验顺序陷阱：hub.enabled=true 先触发 transports 校验，联邦单测需 Hub.Enabled=false 隔离
- e2e 进程残留：sproxy 与 sproxy.exe 是两个可执行体，都要 taskkill 干净

## 5. 测试方法论（DoD 双保险）

- 每个子任务 = TDD（先失败测试）+ `-race -count=1` + golangci-lint 0（主 + 每个子 go.mod）+ make build-all/test-all + check-loopback
- **DoD 双保险**：自动（in-process + CLI 级 + `-race` 连跑 3 次）+ 手动真实二进制验证
- 对抗式审查（全新上下文只读）全部发现（含 Minor/参考级）修复
- 网络测试用 host-only 进程内回环替代真实 STUN（webrtc 测试 124s → 3.3s）
- 判断既有 flake：detached HEAD 切回 origin/master 复跑对比，而非凭直觉
- **死等固定超时必然 flake** → 用产品代码的标准同步（channel 确定性信号）

---

## 6. 2026-09 SmartDial 自动选路 + 多跳（#382/#385/#386/#388）

> 来源：SmartDial（#382，9b2315a2）、via-node 多跳（#385，4c0a8573）、候选索引（#386，2d6bdd1a）、
> via-direct-X 数据面直连（#388，236f22fb）。设计文档已归档删除，本文是**唯一留存经验**。
> 关键设计：`PathProvider` 候选展开模型（BREAKING CHANGE，v0.15.0 后 PathProvider 是外部 API）。

### 6.1 候选展开模型（#385，架构级重构）

- **粒度缺口**：原 `PathProvider` 粒度 = 路径类型（direct/relay/via-node），竞速单位 = 提供者实例；
  via-node 的真实候选是**动态多个中间节点 X**——把 X 遍历塞进单个 Dial 是错配（无法并行竞速/单独缓存/RTT 择优）。
- **决策（用户确认，去兼容层）**：直接重构 `PathProvider` 为 `Expand() []Candidate`——竞速核心只理解
  **候选**（最小单位），路径类型负责「如何展开候选」。无类型断言、无单候选回退分支、无 CandidateExpander 兼容层。
- **BREAKING CHANGE 标记**：`refactor!(mesh)` + `BREAKING CHANGE:` footer（release-please 识别；
  `feat!(scope)` 是错误格式，正确是 `type(scope)!:` 或 footer）。

### 6.2 竞速核心（#382）

- **并行竞速**：每候选一 goroutine 独立 Dial，首胜者胜出，其余**显式关闭**（drainOutcomes 收在途成功连接，
  防 webrtc PeerConnection / relay 流泄漏）。
- **胜者缓存**：key=目标 node，值=候选 ID；TTL 默认 30s（`--smart-ttl` 可调）——命中单路复用，链路变化自动重竞速。
- **缓存路径瞬时故障**：删缓存 → 重新竞速（其余候选故障转移，避免 TTL 窗口内持续失败）。
- **坑**：竞速 drain 计数须减已消费错误（goroutine 泄漏）；落败成功连接必须显式关闭。

### 6.3 候选索引（#386，缓存命中零 Expand）

- **问题**：缓存命中路径每次对全部提供者调 `Expand`——via-node 的 Expand 每次 `ListHubNodes` HTTP 往返 +
  全局锁内网络 I/O，「TTL 内纯内存复用」的初衷对 via-node 不成立。
- **方案**：`winnerCacheEntry` 升级为 `{CandidateID + RegistryGen + 候选快照}`——命中时 gen 未变**直接复用
  快照拨号**（零 Expand、零网络 I/O）；注册表 `Register/Delete` 递增 gen，gen 变化强制重竞速。
- **教训**：删除死代码 `smartCandidateByID`（pre-commit golangci-lint 拦 unused）。

### 6.4 via-node 双候选（#388，数据面直连）

- **双候选展开**：每个中间节点 X 生成 `via-relay:X`（数据面经 hub 中继）+ `via-direct:X`（数据面 webrtc
  直连 X），平级竞速端到端 RTT 择优。
- **via-direct-X 零新协议**：`DialWebRTC(HubSignaler(X))` 打洞 + `WebRTCStream` 写 `DialRequest(T)` +
  X 的 `relay.Serve` 出口拨 T——全部复用既有组件；hub 只承载信令控制面，数据面不经 hub 字节。
- **MaxCandidates 默认 5 → 8**（direct + relay + 3 X × 双候选）。
- **无信令器 fail-closed**：via-direct:X 在无 hub 信令桥时明确报错（via-relay:X 仍参与竞速）。

### 6.5 关键安全边界

- **X 出口拨号仍由 DialPolicy（--dial-allow 精确放行）把守**——webrtc 直连 mux 流与 hub 中继流走同一
  relay.Serve dOK 分支，无新暴露面。
- **hub 信令桥身份绑定**：msg.From 服务端从 X-Node-ID 派生（body 注入面防伪造）；from/to 必须同 mesh。
- **候选 ID 缓存 key**：`via-relay:X1` / `via-direct:X1` 可区分，候选失效（节点下线）删缓存重竞速故障转移。

### 6.6 测试方法论（本系列验证）

- **TDD 红灯先行 + 变异验证**：声称测试能抓 bug 前断言变异已命中（如删 gen 闸门 → 缓存命中复用旧快照红；
  打洞 peer 改错 → e2e 红）。
- **真实数据面 e2e 不 import pkg/server（R4 分层门禁）**：mesh 包内自建 in-process hub + 最小信令桥
  （SignalQueue Push + Peek/Confirm 长轮询）。
- **Windows 防火墙铁律**：webrtc 测试必须 `webrtctest.New(t)` + `SetHostOnly(true)` **成对使用**——
  SetHostOnly 只过滤候选类型，pion/ice 仍会全接口 `net.ListenUDP` 收集 host 候选 → Windows 触发防火墙
  授权弹窗（用户发现，立即修复）。只 SetHostOnly 不够！
- **评估「活跃连接迁移」= 不做**：mesh connect/socks 消费方全是「拨号→建连→用完关闭」短生命周期，
  TCP 无迁移语义，无长生命周期连接需迁移 → 新建连接时择优是正确决策（YAGNI）。
- **评估「纯 mDNS 无 hub 场景 via-direct-X」= 不做**：mDNS 场景本身是局域网直连，L→T 已有直连
  （LAN 打洞成功率高），经 X 多跳反而更慢；现有 DialDirect 已最优（YAGNI）。

### 6.7 端到端加密：SK 群密钥问题 → ECDH 解耦（2026-09-20）

> 来源：T1 端到端加密（分支 feat/mesh-e2e-encryption）。设计文档已收敛进 `docs/tunnel.md`
> （「端到端加密与 SK 解耦」）与 `docs/config.md`（「mesh 多跳端到端加密」），本文为经验沉淀。

- **问题根源**：via-node 候选的入选前提就是**持有 SK**（`ComputeRegisterProof` 以 SK 计算注册
  proof）——「X 是恶意中间人」时 X 几乎总是有 SK。而原数据面加密全部以 SK 派生
  （`DeriveTunnelKey(skHex, meshID)` 是公开函数）→ X 可用同一公式派生同款密钥解密 L⇄T 密文，
  「端到端加密」形同虚设。
- **结论**：SK 是「群准入凭证」，不是「端到端数据面密钥」。
- **解法（复用 tunnel.Tunnel）**：会话密钥 = ECDH（X25519，前向保密）+ 公开指纹派生的静态
  密钥（`DeriveRemoteStaticKey`，非 SK）+ Ed25519 身份双向指纹 pinning（`WithIdentity` /
  `WithPeerFingerprints`，fail-closed）。X 侧 `ServeE2ERelay` 只做 mux 流字节泵，不建隧道
  不解密、无 L/T 私钥 → 有 SK 也只能中转密文。
- **教训**：安全 API 参数要签名诚实——`StaticKey` 字段曾静默忽略（恒用公开指纹重派生），
  属误导性参数，审查后移除（显式传 SK 的预期被静默违背比没有该参数更危险）。

### 6.8 竞速度量演进：各段加法 → 整体链路就绪（2026-09-20）

> 来源：T2（分支 feat/mesh-e2e-encryption）。用户明确要求「竞速最好能用最终到达目标的整体
> 链路耗时进行比较」。

- **原实现**：各候选 Latency = 各段 `time.Since(start)` 加法近似（打洞耗时 + 中继建连耗时），
  不含「X 出口拨号到 T」——慢但整体更快的多跳会被低估。
- **统一语义**：Latency = **整体链路就绪（首字节可读）**。via-relay 用 I27 200 响应时点（hub
  写 200 前已读叶子拨号结果帧）；direct 用打洞完成时点（无中间出口，打洞即链路通）。
- **via-direct 条件回帧（方案 B）**：X 侧 `leaf.go` 回帧条件收窄为
  `DialResultFrames && d.AwaitResult`（原无条件回帧会污染 mDNS/普通直连数据面首字节）；
  via-relay 帧带 `AwaitResult: true`（relay_stream.go）、via-direct 首帧带同字段，仅显式请求
  回帧的拨号才回结果帧 → via-direct Latency 含出口段，mDNS 零污染，旧 X 零影响。
- **多跳窗口加权**：`SmartOptions.MultihopRaceExtend`（默认加倍）——统一 RaceWindow 会让
  「慢但最终更快」的多跳被提前放弃（短路径系统性偏袒）。
- **兼容路径三次读帧成本**：旧 X / mDNS 直连（不回帧）走超时 → Abort → 重开数据流路径，
  Latency 虚高固定 2s×N 等待（设计取舍，via-relay 竞速兜底）。
- **测试教训**：SlowEgress 慢出口 e2e 因 mock X 构造困难 Skip，替代验证链（Latency 含出口段
  语义 + 首字节一致）已覆盖；进程内延迟回帧 fake X 直打 `viaDirectXDial` 补强。

### 6.9 安全缺口修复：X 有 SK 也读不到明文（2026-09-20）

> 来源：T4/T5/T6/T7（分支 feat/mesh-e2e-encryption）。

- **mDNS 信任增强（T4）**：mDNS TXT 广播身份指纹 `fp=`（入 HMAC 签名内容防篡改），接受侧
  白名单 fail-closed（缺 fp / 不匹配拒绝）；未配置身份时 LAN 信任向后兼容。`fp=` 是声明指纹
  （HMAC 保护）非 Ed25519 proof——真正身份 proof 待端到端加密接入 mDNS。
- **--trust-x 白名单（T5）**：中间节点白名单收敛信任（减少攻击面）；白名单变化 → 缓存 gen
  失效（`slices.Equal` 幂等 Set 不递增，防缓存恒 miss）。
- **出口拨号审计（T6）**：`DialRequest.Path` 精确区分 via-relay/via-direct，`leaf.go` 三日志点
  补 path/addr/dial/remote——出口行为可追溯。
- **竞速结果可见性（T7）**：CLI 展示 Kind+Latency（整体链路就绪耗时），metrics path 维度声明式
  预留（CarrierReport.Path，RemoteDialer 接入 SmartDial 后产生数据）。
- **安全纵深结论**：P1（--trust-x）解决「谁可信」，端到端加密（T1）解决「即使选错 X 也读不到
  明文」——两层纵深防御。
