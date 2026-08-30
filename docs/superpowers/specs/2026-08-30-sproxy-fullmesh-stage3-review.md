---
title: sproxy 完全组网·阶段 3 复盘（核心稳固：证书 pinning + hub 联邦）
status: review
---

# sproxy 完全组网·阶段 3 复盘

> 日期：2026-08-30
> 范围：PR #125–#127（master `40420d9` → `176725e`，3 个 squash 合并）
> 规模：38 files，+6479 / −85（累计阶段 3 delta；#125 pinning +2855、#126 联邦A +1785、#127 联邦B +1845）
> 前置：阶段 1（#117/#118）、阶段 2（#119–#123）已完成，阶段 2 复盘 #124 已入库
> 来源：子任务 2/3 learnings（`2026-08-30-stage3-federation-{sync,relay}.md`）+ 子任务 1 lead 报告（pinning learnings 未单独落盘，见 §7）+ 各 PR 审查/CI 记录

## 1. 阶段目标与范围

路线图阶段 3（P1 核心稳固）两项：

| 工作项 | 优先级 | 说明 |
|--------|--------|------|
| 证书身份 + 指纹 pinning | P1（可选） | 长时身份密钥 + 对端指纹校验，防 MITM；不改变共享密钥+HMAC+ECDH 架构 |
| hub 联邦 | P1（用户保留） | hub-to-hub peering + 节点表同步 + 跨 hub 转发路径（按建议拆 2 子任务） |

**核心约束（全程守住）**：核心 `go.mod` 零新增三方依赖；联邦/转发复用既有认证（SproxySig）、mux 帧协议与拨号策略，**零新协议**。

## 2. 交付总览

| # | 子任务 | 分支 | PR | 核心能力交付 | DoD 达成 |
|---|--------|------|----|--------------|----------|
| 1 | 证书身份 + 指纹 pinning | `feature/mesh-p3-pinning` | #125 | Ed25519 长时身份 + 对端公钥指纹 pinning + proof-of-possession + fail-closed | ✅ pin 匹配通/不匹配拒 + 真实 CLI 接线 + 真实二进制 |
| 2 | hub 联邦 A：节点表同步 | `feature/mesh-p3-federation-sync` | #126 | hub-to-hub peering + 节点表周期同步 + `/api/hub/federation/nodes` + `/api/hub/nodes` 联邦候选合并 | ✅ 双 hub 节点表互见（真实双二进制）|
| 3 | hub 联邦 B：跨 hub 转发 | `feature/mesh-p3-federation-relay` | #127 | A→hub1→hub2→B 链式中继 + 防环/超时有界/故障转移/SSRF 边界 | ✅ 跨 hub relay dial echo 往返（真实双二进制）|

## 3. 各子任务复盘

### 3.1 证书身份 + 指纹 pinning（#125）

**能力**：`sclient identity generate/show/fingerprint` 生成 Ed25519 长时身份；指纹 `sha256:<hex>`；`tunnel --xfer <name> --hub <addr>` 真实接线（CLI 级 pinning 端到端校验）。**不改变共享密钥+HMAC+ECDH 架构，pin 为额外身份层**；未配置 pin 行为与现状一致。

**关键决策**：
- 身份用 Ed25519（X25519 无法签名）；指纹 = SHA-256(公钥)；
- **proof-of-possession**：对端用身份私钥签名 `"sproxy-identity-v1"||E_dialer||E_listener`（绑定本会话双临时 ECDH 公钥），先验签后 pin——MITM 转发公钥但无私钥 → 验签失败；
- fail-closed：配置 peer_fingerprints 后不匹配/对端无身份均拒绝；未配置 pin 行为与现状完全一致。

**安全发现闭环（2 条 MEDIUM）**：
1. ecdh.go Weak Identity Pinning → Ed25519 签名绑定双 ECDH 临时公钥（真实持有证明）+ 先验签后 pin + fail-closed；冒名测试证明被拒；
2. factory.go 缺 secret 时 pinning 静默跳过（fail-open）→ 改为 fail-closed：xfer 模式配置身份/指纹但缺 access_key_secret 报错、非 xfer 命令配置指纹拒绝运行。

**审查闭环**：对抗式审查 13 项（H-1 真实接线 + M-1/M-2/M-3 + m-1..m-9）→ 聚焦复审 N-1/2/3 → 用户要求全面审核 5 条建议/参考，全部修复。

**范围外（CI 门槛所需，独立 commit 便于 review）**：
- relay pump 测试去 flake（test-only：expectStreamClosed 独立超时）；
- **云下载组持久化竞态修复（真实产品可靠性 bug）**：`saveGroup` marshal/write 间隙陈旧快照覆盖最新状态（重启丢组状态）→ `groupSaveMu` 串行化（a040cc4，11 行）。

**已知限制（文档化）**：`--xfer` 当前对接 xfer/mux listener，真实 sproxy hub/relay/mesh 数据面协议待接线（与联邦数据面衔接）。

### 3.2 hub 联邦 A：节点表同步（#126）

**能力**：`FederationClient` 周期拉取对端 hub 节点表（SproxySig AccessKey 签名，stale-while-error）；入站 `/api/hub/federation/nodes`（authMiddleware 保护，按调用方 AK 派生 mesh 过滤，只返回路由表防同步环路）；`/api/hub/nodes` 合并本地→DHT→联邦候选（node-id 去重、本地优先、mesh 严格隔离）。

**关键决策**：
- **联邦只做发现，不改路由表**：联邦候选只进 `/api/hub/nodes` 合并，绝不写 `MeshRouteTable`（本 hub 无法直接转发远程节点，转发仍按路由表）；
- **mesh 隔离严格相等**（`c.Mesh != mesh`，空 mesh 只对默认 mesh 放行）——对齐 `mergeDHTNodes`（阶段 2 DHT 曾踩默认 mesh 泄漏）；
- **TLS 安全面（S-Medium 闭环）**：per-peer http.Client。`ca_file` → 专属证书池严格校验；`insecure_skip_verify` → **仅限 loopback**（远程配置 Validate 拒绝，fail-closed）；默认 → 系统根证书池严格校验；
- 默认 loopback：peer.URL 空回落 `127.0.0.1:18083`；远程 peering 必须显式 URL + 成对凭据。

**审查闭环**：必须 #1 per-peer TLS 隔离（InsecureSkipVerify 全局化）→ 修复；建议 #2-#6（凭据成对、flaky 测试、body 限流、mesh 文档、认证/mesh 补测）+ 参考 #7/#9/#10 全修复；#8 defer 顺序书面说明不改理由。复审确认可合入。

**DoD 3 项达成**：双 hub 节点表互见（`TestDualHubPeering_NodesVisible` + e2e 真实双二进制 + 手动验证）、fail-closed 认证 + 默认 loopback（认证两侧测试 + 真实签名 mesh 过滤）、联邦候选合并 + mesh 隔离（`_MeshIsolation`/`_MeshNotLeaked`）。

### 3.3 hub 联邦 B：跨 hub 转发路径（#127）

**能力**：hub-A 路由表未命中但节点是联邦候选时，把 `/api/relay/stream` 拨号请求转发到上报该节点的对端 hub——A→hub1→hub2→B 链式中继。`FederationForwarder` + 独立 CONNECT 拨号器；多对端故障转移（404/502/508 均转移）。

**关键决策**：
- **复用对端 `/api/relay/stream`（HTTP CONNECT 风格）**而非发明 hub-to-hub mux 拨号帧协议——复用全部既有认证/拨号策略/错误语义（200=数据面就绪、502/504/508）；
- **防环自洽（重要教训）**：恒追加下一跳 `peer.ID`（与对端解析目标的同一命名空间），不依赖 `node_id` 配置（node_id 默认空会让第一版防环静默失效）；配置 node_id 时额外追加 self ID 更严格。环路 508；
- **TLS 握手超时有界（重要教训）**：`tls.Dialer.Timeout` 只约束 TCP 三次握手、TLS 握手仅受 ctx 约束（可无 deadline → 黑洞无限阻塞）→ 改为裸 TCP Dial + socket deadline（min(ctx,30s)）+ `HandshakeContext(ctx)`，TLS 后重置计时；
- **故障转移不因 508 阻断**：508 不代表其它对端也回源（每个 Forward 独立防环），继续尝试剩余 peer；
- **转发器放 `pkg/server` 而非 `pkg/tunnel/hub`**：hub 是低层包，转发需发 HTTP 出站请求；且 `pkg/server` 生产代码不能 import `pkg/client`（client 的 e2e 测试反向依赖 server 构成测试编译环）→ 独立实现 CONNECT 拨号器。

**审查闭环（2 轮）**：Critical TLS 握手无界 + 2 Important（防环依赖 node_id 默认失效、测试覆盖）→ 修复；复审 5 Minor（508 阻断故障转移、日志属性误标、`errors.As`、peer.id 全局唯一文档、TLS 后 deadline 重置）→ 全修复。

**安全闭环**：转发不绕过认证（对端 SproxySig 401/403→502）与目标侧拨号策略（SSRF 边界）；mesh 隔离端到端不破（hub-A 按调用方 mesh 定位候选、hub-B 按对端 AK mesh 复查）；防环头 CRLF/控制字符防护；路径头上限 64KiB；TLS/CA fail-closed。

**DoD 3 项达成**：跨 hub 转发路径建立（in-process + CLI 级真实二进制 echo）、防环/防回源（跳数上限 + 路径回源拒绝 508）、自动 + 手动真实二进制双 hub 验证。

## 4. 能力矩阵更新（对照路线图 §2）

### 已实现（阶段 3 新增）

| 能力 | 说明 |
|------|------|
| 证书身份 + 指纹 pinning | Ed25519 长时身份 + `sha256:` 指纹 + proof-of-possession + fail-closed；可选，不改变 HMAC/ECDH 架构 |
| hub 联邦（节点表同步） | hub-to-hub peering + 周期同步 + 联邦候选合并（路由表仍权威） |
| 跨 hub 转发路径 | A→hub1→hub2→B 链式中继 + 防环/超时有界/故障转移（激活多跳按需形态） |

### 剩余缺口

| 能力 | 状态 | 排期 |
|------|------|------|
| 虚拟 IP / 子网分配 | 无 | 阶段 4 |
| 文件同步 / 复制 | 规划中 | 阶段 4（与 Web transfer mgr 接续） |
| 域路由 / 负载均衡 | 无 | 未明确排期 |
| 多跳 chained relay（3+ hub 链式） | 设计排除 | 联邦只同步各 hub 本地路由表（防同步环路），A 看不到经 B 中转的 C 节点；真实 3+ hub 链式需扩展联邦拓扑可见性（阶段 4+） |
| `--xfer` 服务端 listener 接线 | 待做 | 与联邦数据面衔接 |
| TCP 传输 TLS | 无 | 取舍文档化，未来工作 |

## 5. 跨子任务共性经验

1. **防环/去重机制若依赖「可选配置」，默认配置下会静默失效**——要么给配置合理默认，要么让机制自洽（#127 防环从依赖 node_id 改为恒追加下一跳 peer.ID）。
2. **出站连接的手写拨号器要显式约束 TLS 阶段**：`tls.Dialer.Timeout` 不覆盖 TLS 握手（#127 TLS 黑洞）。
3. **安全收紧必然打破旧测试**：TLS 校验收紧（insecure 限 loopback）让既有测试配置被拒绝——需同步改测试语义而非绕开校验（#126）。
4. **import 方向先查再决定复用 vs 独立实现**：`pkg/server` 生产代码不能 import `pkg/client`（client e2e 测试反向依赖 server → 测试编译环）（#127）。
5. **批量替换后必须 go build 兜底**：`fc := X(` → `fc, _ := X(` 的 sed 漏行（前缀不匹配）编译才暴露（#126）。
6. **既存 flaky 判断用 detached HEAD 切回 origin/master 复跑对比**，而非凭直觉（#126）。
7. **复审第二轮仍有价值**：508 阻断故障转移、日志属性误标、`errors.As`、TLS 后 deadline 重置——都是真实缺陷/隐患（#127）。
8. **自动化安全审查逐条闭环**：本阶段 3 条 MEDIUM（Weak Pinning、fail-open 缺 secret、TLS 跳过校验）全部升级为最优解修复（proof-of-possession / fail-closed / CA 池+loopback 限制），不满足于书面确认。
9. **CI 门槛引入的范围外修复要独立 commit 便于 review**（#125 云下载持久化竞态 a040cc4）。

## 6. 质量与依赖

- **CI**：3 个 PR 全部 Build×6 / Lint / Test（ubuntu+windows）/ UI E2E / SonarQube / Benchmark 全绿（#127 Benchmark 由用户确认合并）；
- **独立对抗式审查**：每子任务必跑（pinning 1 轮 + 复审 + 全面审核；fed-A 1 轮 + 复审；fed-B 2 轮），Critical/Important/Minor 全部修复复验，无未解决 Critical/Important；
- **自动化安全审查**：3 条 MEDIUM 全闭环（proof-of-possession、fail-closed、CA 池 + loopback 限制）；
- **依赖**：核心 go.mod 保持 `yaml.v3 + x/sys + x/crypto + x/net`，零三方新增；全模块 lint 0、build-all/test-all/check-loopback 全绿；
- **规模**：38 files，+6479 / −85；每子任务独立 feature 分支 → PR → CI → 人工 squash 合并。

## 7. 文档缺口与后续

- **pinning learnings 未单独落盘**：子任务 1 lead 被中断、返回时未写 `2026-08-30-stage3-pinning.md`（gitignored 本地工件）。本复盘 §3.1 已覆盖其内容；如需补全可后续补写。
- **`--xfer` 服务端 listener 接线**：作为独立工作项，与联邦数据面衔接。
- **测试遗留**：`TestE2E_MeshNode_ServiceAccess` 本机预存在 webrtc 网关 flaky（CI 通过，非本次引入）。

## 8. 对阶段 4 的启示与建议

阶段 4（P2 铺开）两项，建议分别推进：

1. **虚拟 IP / 子网分配**（5–10d）：基于阶段 1 预留的"路由决策"扩展点。建议先做子网划分 + 路由表虚拟地址映射（把 node-id 映射到虚拟 IP，网关层 NAT），再接 mesh 网关。可复用本阶段的安全边界模式（默认 loopback、fail-closed、mesh 隔离）。
2. **文件同步 / 复制**（3–5d）：与 in-flight Web 传输管理器（transfer mgr）接续。建议先确认 transfer mgr spec 状态再规划。

**流程沿用**：每子任务同款「全新上下文子代理 + TDD + 对抗式审查 + 修复全部含 Minor + CI + 人工 squash 合并 + learnings」。

## 9. 结论

阶段 3 三路工作全部按路线图 DoD 达成并合入 master（`176725e`）：证书 pinning 为组网补上身份层（proof-of-possession + fail-closed），hub 联邦打通跨 hub 发现与数据面（节点表同步 + 链式中继），多跳 chained relay 的按需形态已激活。核心依赖纪律与安全红线全程守住，无未解决 Critical/Important。阶段 3 复盘为阶段 4（虚拟 IP + 文件同步）提供衔接基础；启动时机与拆分由用户确认。
