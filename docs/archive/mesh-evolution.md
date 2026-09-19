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
