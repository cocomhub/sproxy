---
title: sproxy 完全组网·阶段 4 复盘（P2 铺开：文件同步 + 虚拟 IP）——含全路线图回顾
status: review
---

# sproxy 完全组网·阶段 4 复盘

> 日期：2026-08-31
> 范围：PR #129–#134（master `7048018` → `863f84a`，6 个 squash 合并）
> 规模：95 files，+14451 / −130（阶段 4 delta；文件同步 5 PR + 虚拟 IP 1 PR）
> 前置：阶段 1（#117/#118）、阶段 2（#119–#123）、阶段 3（#125–#128）已完成
> 来源：文件同步 learnings（`2026-08-30-stage4-filesync.md`）+ 虚拟 IP 交接简报 + 设计文档 v2/v4 + 各 PR 审查/CI/安全记录

## 1. 阶段目标与范围

路线图阶段 4（P2 铺开）两个工作项：

| 工作项 | 说明 |
|--------|------|
| 文件同步 / 复制 | 与 #115 transfer mgr（上传/下载传输管线）接续，扩展为节点间文件同步 |
| 虚拟 IP / 子网分配 | Tailscale/ZeroTier 风格：node-id → 稳定唯一虚拟 IP，`mesh connect <vip>:<port>` 虚拟主机寻址 |

用户决策（2026-08-30）：虚拟 IP 分配双实现（hub 权威 + mDNS 确定性回落）、虚拟主机语义 + 端口白名单、REG_OK 下发自身 VIP、**文件同步 v1 即含服务端任务**、实现顺序文件同步先。

## 2. 交付总览

### 工作项 2：文件同步（5 PR）

| # | 子任务 | PR | 核心交付 |
|---|--------|----|----------|
| A | pkg/sync 同步引擎核心 | #129 | FS 抽象/LocalFS/差异/冲突/并发编排；LocalFS confine 防 symlink 逃逸 |
| B | HTTPTransport 传输层 | #130 | FS 的远程实现 + deadlineConn 活跃读写超时 + ListDir 分页拉全 |
| C | 服务端 SyncManager | #131 | 任务生命周期 + `/api/sync/tasks`；Executor 接口打破测试环；HTTP 直连远程 |
| D | sclient sync CLI | #132 | push/pull + `--wait` 轮询 + `--json` + 非零退出码 |
| E | Web sync_task 频道 | #133 | 传输页 sync 频道（sc.sync 领域 API + 3s 轮询 + 表单），Go 零改动 |

### 工作项 1：虚拟 IP / 子网分配（1 PR）

| # | 子任务 | 交付 |
|---|--------|------|
| A | hub 分配 + 下发 | Allocator/hubAllocator 递增分配 + 快照重建 + REG_OK 能力位下发（旧客户端兼容）+ 瞬态过滤 + `/api/hub/nodes` virtual_ip + config 三段式 |
| B | 出口 NAT | `NewVirtualIPDialPolicy`（ServiceAddrs 精确匹配优先 + 端口白名单 C-1 + selfVIP 改写 127.0.0.1）+ mesh node/relay start/p2p listen 装配 |
| C | mesh 路由 + CLI | VipTable（hub 权威原子重建 / mDNS 确定性校验 / 冲突拒绝 / 子网外拒绝）、`mesh connect <vip:port>`、`--virtual-subnet`/`--vip-allow-port`、mDNS TXT vip= |
| D | E2E + 安全红线 | `TestE2E_MeshConnect_VirtualIP`（闭环 echo）+ `TestE2E_MeshConnect_VirtualIP_UnannouncedPortRejected`（C-1：未宣告端口 9999 拒绝） |

## 3. 关键架构决策

### 文件同步

1. **模块边界打破测试环**：`pkg/sync.HTTPTransport` import `pkg/client`，而 `pkg/client` 的 e2e_test import `pkg/server` → `pkg/server` 不能直接 import `pkg/sync`。方案：`syncmgr` 定义 `Executor`/`QuotaStore` 接口，真实实现在独立包 `pkg/syncexec`，`cmd/sproxy` 装配注入。
2. **HTTP 直连远程（偏离设计 AD-1/AD-5）**：服务端不承担 mesh 客户端角色，v1 SyncManager 远程访问用 HTTP 直连（`sync_remotes` URL + SproxySig），fail-closed；mesh 通道为后续增强（`Dial` 注入点不变）。
3. **同步模型**：单向 + 文件级增量（checksum 相同跳过）+ 递归/过滤器 + 冲突策略（skip 默认/overwrite/lww/conflict_rename）+ 空文件轻量 Upload + 空目录/符号链接默认跳过。
4. **并发**：HTTPTransport 单连接串行分块 + 文件级并发（MaxConnsPerHost=1）。

### 虚拟 IP

1. **分配权在 hub**（SproxySig 准入 + HMAC proof），节点不可自选；mDNS 无 hub 回落用确定性哈希（机制自洽，不依赖可选配置）。
2. **虚拟主机语义 + 端口白名单（安全红线 C-1）**：`vip:port` 直通对端本机，但只放行 `--service` 宣告端口或 `--vip-allow-port`；`mesh connect <vip>:18085`（网关）/SOCKS/未宣告端口必须被拒。
3. **VipTable 注入面防护**：hub 模式每次刷新从签名 hub 列表原子重建（清陈旧）；mDNS 模式校验声明 VIP == deterministicAllocator(mesh, nodeID)；冲突/子网外拒绝。
4. **REG_OK 能力位下发自身 VIP**：不依赖 discovery 环，Discover=false 的 relay 出口节点不静默失效。
5. **出口 DialPolicy 命中顺序**：ServiceAddrs 精确匹配优先（真实 CGNAT 流量逃生口）→ 虚拟子网白名单 → 公网/CIDR 回落。

## 4. 安全闭环（自动安全审查 MEDIUM 全处理）

| 发现 | 工作项 | 处理 |
|------|--------|------|
| IDOR / 任务无 owner | 文件同步 C | 书面确认（server-global，与 CloudTask 一致）+ task.go 注释；未来多租户加 owner |
| Path Traversal (FollowSymlinks) | 文件同步 C | LocalFS.confine()（EvalSymlinks 逐级 + 前缀校验）覆盖全部 8 操作 + `TestEngineSync_FollowSymlinks_NoEscape` |
| Plaintext http | 文件同步 C | config.Validate 拒绝 sync_remotes 非 loopback http |
| VIP 注入面（可预测 + 后写覆盖） | 虚拟 IP | VipTable 冲突拒绝 → 精化：hub 权威原子重建 + mDNS 确定性校验 + first-writer-wins 防劫持 + 子网外拒绝 |

## 5. 跨子任务共性经验

1. **测试环**：`pkg/client` e2e_test import `pkg/server`，任何 `pkg/server→pkg/client` 依赖破坏 `go test ./pkg/client/...`——接口 + 实现包打破。
2. **CI flake 用确定性信号而非死等超时**：semaphore 测试 5s→15s 死等仍在 Windows -race+cover 超时 → 改 mock executor `started` channel 确定性等待。**死等固定超时必然 flake**（阶段 4 后半程 syncmgr 其它短超时测试仍复现此问题，见 §7）。
3. **http.Transport HTTP/1.1 不调用 SetDeadline**：写路径对端停读 → Write 永久阻塞 → 需活跃写超时（timer 监督 + 到点强制 Close）。
4. **MuxStreamConn.Close 可能阻塞**：到期强制关闭应探测 `Abort() error` 接口。
5. **deadline 拆双 timer**：单一共享 timer 在 persistConn readLoop/writeLoop 并发下跨方向串扰。
6. **CREATE 去重 TOCTOU**：findActive + 预留 + 插入整体在写锁内，防并发同 key 双任务写同一 dst。
7. **安全审查的书面确认也是闭环**：IDOR（无 owner 设计）注释书面确认即可通过。
8. **整体代码审核与聚焦审查的多轮价值**：虚拟 IP 经 A/B/C 轮 + 聚焦整 diff 轮 + 整体代码审核，捕获 dial 装配顺序缺陷、E2E 语义未锁定、TOCTOU——多轮审查不是形式，是真缺陷。
9. **子代理并行与分支隔离**：后台子代理工作树文件会随主流程切分支污染 PR；需隔离/确认不重叠。

## 6. 质量与依赖

- **CI**：6 个 PR 全部 Build×6 / Lint / Test（ubuntu+windows）/ Benchmark / UI E2E / SonarQube 全绿（含 flake 重跑确认）；前端 web-test 167 tests + 浏览器手动验证。
- **审查**：文件同步逐子任务 + 虚拟 IP 三轮 + 聚焦整 diff + 整体代码审核，Critical/Important/Minor/参考级全修复。
- **依赖**：核心 go.mod 保持 `yaml.v3 + x/sys + x/crypto + x/net`，零三方新增；全 module lint 0。
- **规模**：95 files，+14451 / −130。

## 7. 全路线图回顾与遗留

**阶段 1–4 全部完成**（master `863f84a`），sproxy 组网能力从"hub 星形 + webrtc 打洞"演进为完整 mesh：

| 维度 | 能力 |
|------|------|
| 发现 | hub 节点列表 / mDNS / DHT / hub 联邦（跨 hub 节点表） |
| 认证 | SproxySig + per-node HMAC + 证书身份指纹 pinning + fail-closed |
| 传输 | TCP / WS / QUIC / gRPC / WebRTC / UDP 隧道 / SOCKS5 / TCP relay / 跨 hub 链式中继 |
| 应用 | 文件上传下载 / 云端下载 / 文件同步 / 虚拟 IP 寻址 / Web UI |

**遗留事项**：
- **syncmgr 短超时测试统一去 flake**（master 预存在）：`TestCancelTask_Queued` 等仍用 5s 死等 waitForStatus，CI 随机失败——建议单独小 PR 统一改确定性信号。
- **虚拟 IP learnings 最终版未落盘**：`2026-08-31-stage4-virtualip.md` 缺失（agent 合并后未写），仅交接简报在；可补写。
- 域路由/负载均衡（未排期）；3+ hub 链式（设计排除，需扩展联邦拓扑可见性）；TCP 传输 TLS；`--xfer` 服务端 listener 接线。

## 8. 结论

阶段 4 两个工作项全部按 DoD 达成并合入 master（`863f84a`）：文件同步打通"节点间单向文件级增量同步"（核心引擎 → 传输 → 服务端任务 → CLI → Web UI 全链路），虚拟 IP 为 mesh 提供 Tailscale 风格寻址（hub 权威分配 + 端口白名单安全边界）。核心依赖纪律与安全红线全程守住，无未解决 Critical/Important。至此**完全组网路线图（阶段 1–4）全部完成**；后续按遗留事项与用户优先级推进。
