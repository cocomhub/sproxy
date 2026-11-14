<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 路线图历史规划波次（v0.20+ ~ v0.23+ 及 secretdata 生产化）— 归档

> **归档说明**：原文为 `docs/roadmap.md` 的 §10–§13 与「secretdata 生产化待办」章节
> （2026-09-10 ~ 2026-10-19 分批规划波次，约 940 行），随 roadmap.md 收尾精简（Backlog 化）归档。
> **现行事实源**：[roadmap.md](../roadmap.md)（能力总览 + 演进原则 + Backlog）与
> [api.md](../api.md) / [config.md](../config.md) / [cli.md](../cli.md) / [architecture.md](../architecture.md) /
> [deploy.md](../deploy.md) / [secret-volume.md](../secret-volume.md)。
> 本文件只保留**设计意图 / 取舍 / 被否决方案 / 仍有效的约束与踩坑**；任务勾选、逐项验收、
> 已完成条目、与权威文档重复的细节均已移除（落地记录见文末索引）。

## 1. 这些波次是什么

2026-09-10 起，roadmap.md 以「分批盘点 → 逐面 grep 实证 → 矩阵排序 → 逐项设计 → 分片实施」的方式
推进 ~82 个功能项。每批产出一组规划章节：残余补齐、审查待办、新方向、优先级矩阵与设计批。
价值不在条目本身（多数已实现），而在两件事：

1. **判据与口径**：如何确认「缺 = 全仓无 grep 命中 / 部分 = 有基础形态缺完整能力」；「每项规划必须有
   设计文件依据」；「已实现但无设计文档的能力面须补设计」。
2. **设计决策**：落地路径上被否决的方案、被约束的语义、踩过的坑（本文件主体）。

## 2. 关键决策与取舍（仍有效）

### 2.1 状态层与集群化（§11.11–11.12）
- **状态抽象先行**：多节点集群的前置是「状态层」可插拔（`StateStore`），而非直接上 etcd/raft。
  先做 `pkg/state`（local 默认 + mongo 插件），再做选主（`LeaderElector`），最后才谈多副本形态。
- **绝不允许静默回落 local**：配置了未注册/未实现/连不上的后端 ⇒ 启动失败（fail-closed）。
  理由：回落会造成「以为多节点一致、实际各写各的」这一最坏情况，比启动失败危险得多。
  `raft` 只留注册位不实现，配置即报「未实现」。
- **后端隔离**：Mongo 实现放独立 module（`pkg/state/ext/mongo`），只有装配层引 `mongo-driver` ⇒
  核心 `pkg/state` 保持零外部依赖（同 `pkg/tunnel/xfer/ext/*` 模式）。
- **不做数据迁移工具**：`local → mongo` 由运维脚本完成；本期只保证**读旧格式兼容**——既有 JSON
  原样保留为「StateStore 之上的首版导入」，`NewLocalStateStore` 只扫 `<root>/state/`，既有 `meta/`
  路径由各 Store 适配器在**读路径**回退（双读单写；不搬迁、不双写、不改既有布局）。
- **key 规范三段式** `<owner>/<type>/<name>`，每段经 `validateSegment`（拒 `..`、绝对路径、空字节、
  Windows 非法字符）；段校验是**路径安全边界**，不因「值来自内部」而省略。
- **CAS 是核心原语**（配额/dedup 引用计数/凭据并发）：语义等价「读当前 → 比对 → 原子替换」；
  Mongo 侧靠版本号 + 单文档 findAndModify。**并发 CAS 正确性是验收红线**，非可选优化。
- **配额不迁 StateStore**：配额是高频内存账本，靠 `LeaderElector` 保写面唯一即可，不进 StateStore。
- **LeaderElector 双平台**：Unix flock / Windows `LockFileEx`（非「Windows 回落恒主 + Warn」，
  禁静默降级）。Windows 回落只为可观测兜底，不作默认。
- **Local flock 仅单机**：多节点共享存储场景的真选主靠 Mongo TTL 租约（TTL index 仅作崩溃兜底
  清理；主动续租周期 ttl/3；ttl 默认 30s / 续租 10s / 时钟偏移容忍 ±10s）。

### 2.2 只读副本与写面唯一（§11.11 §12.1）
- **推荐形态 A（共享外部卷 + 选主 + 只读副本）**：数据面共享（外部卷/federated）现成，控制面用
  LeaderElector 保写面唯一；不引入 etcd/raft（与轻量单文件架构冲突）。
- **写/读分离**：写面仅主节点（非主 503 + `Retry-After`）；读面水平扩展（复用 federated 只读形态）。
- **扩容/缩容**：扩容 = 加只读副本节点（挂同一外部卷）；缩容 = 节点下线 + 选主切换；写面仅主节点消
  除冲突。
- **索引一致性**：主节点写 → 事件流广播失效（`EventBus` + `/api/events` SSE 游标回放）→ 副本
  重载；索引快照经 `IndexEnvelope`（rev 校验，损坏回退重建）同步。

### 2.3 S3 / WebDAV 协议面（§11.5 §11.7）
- **S3 complete 加固**：complete 校验客户端 ETag 与落盘 part md5 匹配 + meta key 一致性
  （防 part 篡改/跨会话拼接）；ETag 去引号归一；PartNumber 范围/重复 400；失败清理半截目标。
- **S3 complete 配额记账**：complete 落盘前 `TryReserve`（owner Scope + 卷容量池）超限 507 +
  失败统一双 `Release` + 成功 `Commit`/覆盖写 `Adjust` 差分（至多一次，Reservation.done CAS）——
  对齐普通上传语义，防分块绕过配额。
- **S3 复合 ETag**：complete 响应加 `hex(md5(concat(md5(p1)...)))-N`（S3 分块标准形态）。
- **卷即桶**：`/s3/<卷名>/<key>`，splitS3Bucket 首段=卷名；**不做 CreateBucket/DeleteBucket**
  （避免双层命名空间，卷已有 ACL/配额隔离）；ListBuckets 枚举 ACL 可见本地卷名（外部卷不列）。
- **WebDAV `/dav/`**：owner 租户 user 桶 → LocalFS 桥接 `webdav.NewHandler`；LOCK/UNLOCK 现为
  进程内存（`NewMemLS`），单实例重启丢锁被协议容忍（RFC 4918 允许），多实例假互斥才是真缺口
  ⇒ 持久化锁系统为 P3 蓝图（见 [designs/2026-09-24-webdav-lock-persist.md](../designs/2026-09-24-webdav-lock-persist.md)）。

### 2.4 通知 / 告警 / 审计（§11.1 §11.4 §11.5）
- **通知统一入口**：事件源 = `RecordAudit`（全部审计事件统一入口）异步 dispatch → `NotifyCenter`
  （rules action glob 路由 + 去抖窗口 + 指数退避重试 + 有界历史）。
- **告警状态机**：`AlertEngine`（source + threshold + channels）+ FIRING→OK 状态机去抖 + 恢复通知；
  残余补齐：NAT 穿透失败、告警规则 SIGHUP 热加载（`ReloadRules` 锁下 `slices.Clone` 原子换规则；
  `alerts.enabled` 翻转需重启，装配期决策须 Warn 明示）。
- **审计轮转**：`audit.log` 原子 append + `audit.max_size`/`max_archives` 持锁轮转（重启只载当前
  文件，热历史有界）。
- **Webhook 出站签名**：`X-Sproxy-Signature: sha256=<ts>.<hex>` + 时间戳头（secret 空 = 不签名零回归）。

### 2.5 组网 / 出口 / 代理（§11.1 §13.3）
- **出口策略**：`--exit-group` 节点组 + 三模式负载均衡（failover 默认零回归 / round-robin /
  weighted）；**无论模式保留组内 failover 兜底**；`--route` 与 `--exit-only` 互斥 fail-closed。
- **分流规则**：`meshconn.Conn.Routes` + `SelectRoute`（域名后缀/cidr 网段，声明序首命中）；
  udp map 命中替换出口节点（单 mux 固定出口取组内第一）。
- **VPN 形态**：先做**用户态最小集**（`sclient mesh up` = 本地 SOCKS5 + 虚拟子网路由到 `--exit`，
  无需内核 tun/tap 特权）；内核 tun/tap（整网段透明路由）需特权 + 平台集成，属后续 P2/P3。
- **服务发现健康化**：`quality`（healthy/degraded/stale）基于 mux 重传/错误累计 + 节点连接时长；
  RTT 用 mux 心跳 Ping/Pong 采样（无在途 ping 跳过防冒充）。
- **跨 hub**：FederationForwarder 用 CONNECT 转发 + `X-Relay-Hop`/`X-Relay-Path` 防环
  （`DefaultRelayMaxHops=4`，超限/回源 508）+ 按序故障转移 + mesh 隔离。

### 2.6 跨墙 / 传输（§11 §13）
- **不做自实现协议混淆**：GFW 跟进快于迭代；只做「形态对齐 + 部署形态」（CDN 前置 / 正式证书 /
  复用现有协议形态）。
- **被动伪装保守默认**：`tls.cipher_order`/`tls.alpn` 形态对齐、`tunnel.idle_padding`、WS 路径/升级头——
  开关显式默认保守，禁静默降级。
- **QUIC/gRPC 装配**：`--transport quic|grpc` + `hub.transports.*`；QUIC 0-RTT 靠
  `ClientSessionCache`（LRU 64）+ 服务端 `Allow0RTT`（0-RTT 重放拒绝按 RFC 9001，ticket 过期回落）。

### 2.7 版本生命周期 / 高可用（§11.6）
- **sclient 自更新**：GitHub API 单次 + CDN 直链 + `checksums.txt` SHA-256 fail-closed + 原子替换
  （Windows 两段式：先退出自身再替换）。
- **sproxy 优雅重启（Unix-only）**：`USR2` → `ExtraFiles` 继承 listener → `/readyz` 就绪 →
  复用 `handleSignalShutdown` drain；Windows 特性关闭零变化。
- **多副本（Helm）**：`strategy: RollingUpdate {maxUnavailable:0}` + PDB（minAvailable:1）+
  readinessProbe 改 `/readyz`；**多副本写面限制声明**（只读面水平扩展 + 单写主）。

## 3. 被否决 / 不排期的方案及理由

| 方案 | 处置 | 理由 |
|------|------|------|
| etcd/raft 完全分布式（方案 B） | **不推荐** | 与轻量单文件架构冲突；引入外部依赖（etcd）；运维复杂度陡增——超出「文件服务 + 隧道」定位 |
| StateStore Raft 实现 | 只留注册位 | 集群深水区，`state_store.type=raft` 现 fail-closed 报「未实现」 |
| 自实现协议混淆层 | **不做** | GFW 跟进快；只做形态对齐 + 部署形态 |
| S3 生命周期策略 | **砍** | 过期删除 = 回收站 TTL / 版本 GC 已覆盖；转冷 = 冷热分层已覆盖（功能重叠无增量） |
| WebSocket 服务端推送 | **排除** | SSE（`/api/events`）已满足实时刷新，WS 推送无增量价值 |
| 分享 `allow_upload`（可上传分享） | **不排期** | 与「分享 = 消费方只读」语义冲突；目录级授权已有 mesh acl / RBAC 覆盖；无设计文档 |
| 数据迁移工具（local → mongo） | **不做** | 由运维脚本完成，本期只保读旧格式兼容（零回归关键：不搬迁/不双写/不改布局） |
| Windows LeaderElector 回落恒主 | **否决** | 用 `LockFileEx` 真实实现（禁静默降级） |
| 配额迁移 StateStore | **不做** | 高频内存账本；靠 LeaderElector 保写面唯一 |

## 4. 仍有效的约束（演进原则补充）

- **正确性优先于功能**：审查发现的正确性缺陷（P1/P2）优先于新功能。
- **残余补齐优先**：既有规划标记的残余（NAT 告警/分流/负载均衡）是已验证方向的收尾，低风险高价值。
- **巡检/备份是运维底座**：全仓 checksum 巡检 + 卷导出是生产可运维性基础，优先级高于增量功能。
- **审查发现 P3 多为演进约束而非缺陷**：VPN P1 骨架（真设备为 OS 集成后续片）、selfupdate 平台
  覆盖（Windows 两段式依赖外部重启时序）——记录为已知边界，不阻塞。
- **已实现无设计文档的能力面须补设计**：设计文档是「每项规划的依据」，实现先行不豁免。
- **集群化收尾顺序**：状态上移 → 只读副本接入 → Mongo 租约（State Store 装配是副本读面的前置）。
- **判据口径**：源码 `grep` 排除 `_test`；「缺」= 无实现命中；「部分」= 仅有基础形态缺完整能力。
- **测试纪律**：TDD + 变异验证；Web UI 改动带 `node --test` 单测 + Playwright e2e；测试只绑 loopback。

## 5. secretdata 生产化：秒传收敛与块级交错（背景，2026-10-18）

**领域约束（已取证）**：多分片上传后远端 md5 **非权威**（`MD5Unreliable`）；checksum 只在「同一文件
重复上传（秒传）」命中后收敛（`putAttempt` = 存在性检查 → 上传 → ETag 复核 → 不匹配则 `RapidUpload`
刷新；未命中码 31079）。

**两种策略对比**（本地临时物 = 加密后产物）：

| 维度 | S1 占本地临时空间（保留加密产物） | S2 重复计算（重读源 + 重加密） |
|------|-----------------------------------|-------------------------------|
| 本地峰值 | O(整个文件)（先全部加密落 temp 再上传） | O(1 块) |
| CPU/IO | 0（直接复用） | AES-GCM 重算 + 源重读 |
| 可重试性 | 强（秒传刷新只读本地文件） | 仅当源可重读；HTTP body / 纯流不可重读 ⇒ 无法重算 |
| 秒传收敛 | 同一文件重复上传即可刷新 | 需先重算再刷新（多一轮 CPU/IO） |

**关键洞察**：secretdata 是**分块存储**（每块一个对象）⇒ **秒传天然是按块的** ⇒ **无需保留整份
加密文件**；每块上传时其本地产物本来就在场 ⇒ 秒传刷新**零重算**。

**定案形态（混合 + 块级交错）**：
1. **块级交错**（encrypt 块 k → 上传块 k → 丢弃）：本地 staging = **O(1 块)**（默认块 1–200 MiB）；
   「空间不足」多数场景自然消失；秒传按块刷新；失败用既有 `rollbackWrite(uploaded…)` 回滚
   （远端孤儿由可选 GC/清理兜底）。
2. **回退链**：① 产物在场 ⇒ 复用（S1）→ ② 源**可重读**（本地文件/已暂存路径）⇒ 重算（S2）→
   ③ 都不可行 ⇒ **fail-closed**（明确错误，绝不静默接受不可校验的写）。
3. **空间治理**：沿用既有 staging 配额（本卷自管）⇒ 不足时按 ①→②→③ 回退，而不是直接失败。
4. **不推荐**：长期保留整份加密文件（峰值 O(file)、崩溃残留、与按需/流式方向相悖）。

**已落地要点**：`shardseal.EncryptShardsReaderToChunks`（sink 抽象，分块逐块交调用方，meta 由调用方
末尾加密一次）⇒ 本地分块副本峰值 O(file) → O(1 块)；所有分块上传点统一经 `WithStagedLocalPath`
（数据/parity/dedup）；回退链覆盖**落盘前**的可用性判定（临时目录不可用 / 配额拒绝）。

**已知残余（如实登记，勿当缺陷重提）**：
1. 崩溃（非错误返回）时远端可能留孤儿块 ⇒ 由既有可选 GC/清理兜底。
2. 内存写路径（`writeFileEncrypted`：非 seek 源 `io.ReadAll` 后整目录落 temp）未交错——明文已在
   内存（内存峰值 O(file) 才是主瓶颈）、parity 需全部明文分块；交错收益有限。
3. 回退链的「重算」（②）落在上层：本层不做单块重加密（无 per-block 加密 API）⇒ 本层保证
   「源不被消费/销毁 + 失败返回明确错误 + 完整回滚」，使上层重算可行。
4. 「先登记后上传」未覆盖全部上传点（目录 meta/同卷复制/copyContentBlobs/改名/dedup 命中路径仍
   「写后登记」）⇒ 遇内层 write-then-fail 会在远端留孤儿 blob（空间泄漏，无数据丢失）。
5. 远端「md5 非权威 + 未报 size」时零校验接受：属 R5-I1「无校验能力≠损坏」的既有裁定，改
   fail-closed 会拒绝合法上传 ⇒ 需产品裁定取向（当前保持接受 + Warn）。

## 6. 落地记录索引（波次 → PR）

| 波次 | 落地主干 | 备注 |
|------|----------|------|
| v0.20+（§11）残余补齐/审查待办/新方向 | 全仓 checksum 巡检、卷导出/导入、S3 complete 加固（#575）、联邦卷强一致性、SLO 指标、StateStore F1、LeaderElector（#578）、MCP server、OIDC/LDAP、WebUI i18n 框架、任务调度器 … | 多数在 §11.1–11.14 各表标「已落地」 |
| v0.22+（§12）集群收尾 | 状态上移（#651）、只读副本（#659）、Mongo TTL 租约（#656）、计量导出（#652）、限流 config（#650）、备份 P2（#664）、AI 事件流水线（#667） | F2 迁移顺序 = 凭据先行 |
| v0.23+（§13）Web UI + 五维深化 | V7 ①② 播放器 + 外部卷明文目录浏览、i18n 框架；V1–V6 与集群/组网/代理/架构深化的**未落地项见 roadmap Backlog §10** | Web UI 体验列为头等优先级 |
| secretdata 生产化（§secretdata） | ① 真实网盘 e2e + ② secrets 按卷 + ①-附 秒传/块级交错；**未落地项见 roadmap Backlog §10.5** | 用户裁定 2026-10-18 |

> 各波次同时产出的设计文档（17 份首批 + 45 份四批 + 补设计 7 份 = 62–82 份）已分别归档至
> `docs/archive/2026-09-24-*.md` / `docs/archive/2026-09-27-*.md`（现行设计见 `docs/designs/`）。
> 原「设计批」「优先级矩阵 v1/v2/v3」「能力缺失全景」等**过程产物**（子代理头脑风暴产物与逐项验收）
> 不再保留——其结论已并入上文决策与权威文档。
