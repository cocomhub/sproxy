<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 设计发展规划（Roadmap）

> 本文是 sproxy 的**权威路线图**：文件服务 / 多卷 / 云同步 / 跨墙可识别性 / 性能
> 五个方向的现状能力盘点、差距分析与演进路线。随实现演进同步更新（与各功能
> 权威文档 [api.md](./api.md) / [config.md](./config.md) / [cli.md](./cli.md) /
> [architecture.md](./architecture.md) / [tunnel.md](./tunnel.md) 配套）。
>
> 里程碑口径：**P0（已规划/近期）→ P1（中期）→ P2（远期探索）**。
> 状态字段：`已落地` = 已合入 master；`已规划` = 设计或排期已定；`待设计` = 需先做方案再动工。
> 每条演进都带「验收标准」——合并功能 PR 时对照更新。

## 1. 总览

| 方向 | 现状定位 | 最大差距 | 首要里程碑 |
|------|----------|----------|------------|
| 文件服务 | 功能面完整（上传/下载/分块/版本/分享/搜索/审计/多用户） | 元数据无索引（搜索=全量扫描）；单文件 1 GiB 上传上限 | P0 内容寻址索引 + 大文件上限演进 |
| 多卷 | 本地多盘 + 外部后端框架（baidupcs/webdav/s3）已落地 | 无跨卷复制/镜像、无分层存储（冷热）、后端生态少 | P0 卷复制/镜像 + P1 冷热分层 |
| 云同步 | 文件级增量 push/pull + mesh 载体 + 冲突策略已落地 | 单向任务式（无双向连续同步）、无变化事件驱动（轮询）、远程无删除传播 | P0 删除传播/双向增量 + P2 连续同步 |
| 跨墙可识别性 | 加密/指纹/pinning/多传输已落地，**流量伪装为零** | DPI 特征明显（自定义 TLS/帧协议）、无 CDN 前置指南 | P0 传输伪装白皮书 + P1 被动伪装层 |
| 性能 | 并发分块/断点续传/流式窗口/基准套件已落地 | 无索引导致搜索/列表 O(N)；无基准基线与门禁；gRPC/QUIC 传输未装配 | P0 搜索索引 + 基准基线门禁 |

---

## 2. 文件服务

### 2.1 现状（已落地）

- **完整 REST 面**：上传（`X-File-Checksum` 强校验 + 幂等）、下载（Range/分块）、删除（checksum 匹配）、
  重命名/移动、批量操作、目录、`/api/files` 列表、`/api/files/search` 搜索、stat 单文件元信息
  （见 [api.md](./api.md)）。
- **分块上传/下载**：`/upload/init|chunk|status|complete` + `/download/chunk`；默认 4 MiB 块、
  并发 4、断点续传（会话 TTL 24h）、服务端块计划上界 65536（单文件默认上限 ≈ **3.999 TiB**）。
- **数据完整性**：全链路 SHA-256 checksum 强制（上传必填 `X-File-Checksum`、下载可校验、rename/delete 匹配）。
- **版本管理**：`versioning.enabled`（可选），按卷目录独立计数，restore/删除/GC。
- **分享**：token 分享 + 密码 + 过期 + 一次性/计数，原子持久化（重启恢复）。
- **多租户 + 配额**：租户自包含六桶布局（`user/cloud/archive/chunk/version/meta`），每租户
  `*os.Root` 防穿越 + `quota.Scope` 双账本（reserve→Commit/Adjust/Release，重启扫描校准）。
- **用户体系**：凭据 Ring（SproxySig v2 签名）+ TOTP 注册/登录（session SK）+ AK/SK 轮换 +
  静态加密存储（aesgcm / Vault Transit 后端）。
- **审计**：`audit.buffer_size` 有界内存环形缓冲（默认 2048）+ `GET /api/audit` + `/api/audit/export`
  JSON 导出；审计行独立 JSON logger 机器可检索。
- **备份/恢复**：整根 tar.gz + manifest 版本校验（拒绝跨版本恢复），`make backup/restore`。

### 2.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **元数据无索引** | `search` 递归扫目录名；列表按目录实时枚举 | 大目录/大文件库下搜索与列表 O(N)、慢 |
| **单文件 1 GiB 上传上限（普通路径）** | `UploadBodyLimit = 1 GiB` 硬编码；>1 GiB 必须分块 | 客户端/第三方不感知分块时大文件失败；上限不可配 |
| **无内容寻址/去重** | checksum 只做完整性校验，不做重复文件检测 | 相同内容多次上传占多份配额 |
| **无实时同步语义** | 无 fsnotify 类变更事件（服务端进程内） | Web UI/客户端需轮询才见新文件 |
| **目录操作无递归删除** | `rmdir` 需要空目录或 `force`；无 `rm -rf` 语义 | 大目录清理繁琐 |
| **审计不落盘** | 仅内存环形缓冲 + stdout JSON | 重启丢审计；无检索/过滤 API（按 owner/动作/时间） |
| **上传无服务端压缩/转码** | 原样落盘 | 文本/图片类存储膨胀 |

### 2.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：搜索/列表索引** | 启动/写路径增量维护文件名索引（owner 维度，可扩展内容索引）；`search` 与列表走索引；索引损坏可重建 | **已落地**（#422 内存增量 + 持久化快照）：`search`/列表走索引（亚秒级）；写路径增量 + 失效全量重建；快照落盘 `<meta>/index/<owner>.json`（`index_save_interval` 周期保存，重启载入免全量 WalkDir）；损坏/缺失快照回退重建 |
| **P0：大文件上限演进** | 普通上传上限改为可配置（`max_upload_bytes` 恢复可配，默认保持 1 GiB 零回归）；`>1 GiB` 时服务端自动转分块会话 | **已落地**（#479）：`max_upload_bytes` 可配 + 超限 413 带 `X-Auto-Chunked: true` → 客户端自动转分块（直接 POST 10 GiB 走自动分块成功，调用方无感知）；配置显式可查（`/api/config`） |
| **P1：内容寻址去重** | 上传时按 checksum 查重（同 owner 同卷同内容 → 硬链接/引用计数，可选开关） | **已落地**（#426/#429）：`dedup` 段配置开启后上传按 checksum 查重（同 owner 同卷同内容 → 硬链接零拷贝 + `meta/dedup.json` 引用计数台账）；删除引用计数归零才删 inode + 配额释放；FAT/exFAT 无硬链接回退复制 |
| **P1：服务端事件通知** | 文件变更事件流（SSE/WebSocket）：`/api/events` 订阅 upload/delete/rename/move/version | **已落地**（#433+#437+#434）：事件源覆盖 upload/rename/delete/mkdir/rmdir/version/share（#437 补 version/share）；Web UI 由轮询升级为 EventSource 实时刷新（#434，断线重连+游标回放）；事件不丢（游标可回放） |
| **P1：审计落盘 + 查询** | 审计环形缓冲可选落盘（`audit.persist`）；`/api/audit` 支持 owner/动作/时间过滤 | **已落地**（#431）：审计默认落盘 `<默认卷根>/audit/audit.log`（原子 append，启动载入历史）+ `GET /api/audit` 支持 action/actor/since 过滤 + 导出带过滤 |
| **P2：上传管线扩展** | 可选服务端压缩/缩略图/转码插件（`RegisterTransform`） | **已落地**（#472+#475+#478）：`RegisterTransform` 注册表 + 图片缩略图按需生成（`?transform=thumb&width=N`，原文件不动）+ 派生缓存（meta/transform 原子落盘 + GC） |

---

## 3. 多卷

### 3.1 现状（已落地）

- **本地多卷**：`volumes[]` 配置（`pkg/volume` 纯域 + `pkg/server/volumes.go` 装配）；每卷独立
  物理根 + `LAYOUT_VERSION` 校验 + 容量池（`pkg/quota.Pool`）；placement `prefer-default`/`spread`；
  卷 ACL（deny/allow）；meta 单点权威在默认卷（checksums/凭据/任务状态）。
- **卷操作**：`POST /api/volumes/move`（跨卷流式复制原子迁移）、`rebalance`（大小降序逐文件迁移）、
  `GET /api/volumes`（per-owner 视图）；sclient `volumes`/`--volume`/`mv --to-volume`；WebUI 卷 badge/仪表/上传下拉。
- **外部后端框架**：`pkg/volume/registry.RegisterBackend(type, factory)` 可插拔；`GET /api/backends`
  动态列出；已注册 `baidupcs`（独立 module 二进制优先+库兜底）、`webdav`、`s3`。
- **用户自有卷**：per-owner 网盘盘（`POST/GET/DELETE /api/volumes/user`，`<owner>/meta/volume/*.json`
  原子持久化 + 重启扫描恢复）；同步任务 `remote.volume` 寻址，跨用户 404 防枚举。
- **容量账本**：外部卷容量 = 本系统可用限额（`UserVolume.Capacity`），backend 级 Total/Used 查询。

### 3.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **无跨卷复制/镜像** | 只有 move（迁移，源删） | 无法做冗余镜像/多副本/迁移演练 |
| **无分层存储** | 所有卷同权，无冷热区分 | 热数据占 SSD、冷数据占对象存储无法编排 |
| **外部后端生态薄** | baidupcs/webdav/s3 三类；s3 是 `ext/s3` 实现 | 缺常见后端（SFTP、FTP、本地其它盘、oss/cos 等） |
| **外部卷一致性弱** | 同步视图（`sync.FS`）透传，无本地校验和缓存 | 网络盘元数据每次实时拉取，慢且依赖可用性 |
| **无卷健康/迁移仪表** | 卷状态只有容量；无读写失败/延迟指标 | 盘故障难发现；rebalance 无进度面板 |
| **无异地多活/联邦卷** | 卷都是单机物理根（外部后端也是直连） | 多副本容灾需自建 |

### 3.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：跨卷复制/镜像** | `POST /api/volumes/copy`（复制不删源）+ `mirror` 定时复制策略（`volumes[].mirror_to` + `mirror_interval`） | **已落地**（#417）：复制后源/目标 checksum 全等；镜像策略周期执行可观测（审计 `volume_mirror`/`volume_copy`）；配置校验拒自指/不存在/成环镜像链 |
| **P1：冷热分层** | 卷属性 `tier`（hot/warm/cold）+ 按大小/访问时间自动降级任务（复用 rebalance 迁移语义）；读时按需回迁 | **已落地**（#452 基础 + #466 warm 档细化）：hot→warm→cold 两级降级 + warm 独立阈值 + 读时按需回迁（API 无感） |
| **P1：外部后端扩展** | 新增 SFTP 后端；s3 补充签名 v4 直传/分片；backend 健康探针 | **已落地**（#454 SFTP + #460/#473/#477 s3 直传 + 探针）：`GET /api/backends` 动态列类型（sftp/s3/baidupcs）；后端不可达时卷状态 `degraded` 可观测（HealthProbe 拨号探测） |
| **P1：卷健康/迁移仪表** | 卷级指标（读写延迟/失败率）入 `/metrics` + WebUI 卷仪表迁移进度条 | **已落地**（#432 指标 + #440 WebUI 健康仪表 + #448 rebalance 迁移进度入 /metrics + WebUI 进度条）：面板可见每卷健康（healthy/warning/degraded 徽标）+ 迁移进度（按卷对百分比） |
| **P2：多副本与联邦卷** | 卷复制策略升级为多副本（N 节点同步）+ 只读联邦卷（远端卷只读挂载，复用 mesh 载体） | **部分落地**：多目标镜像 `volumes[].mirror_targets`（一源 → N 副本周期复制，冗余副本就绪）；跨节点联邦卷（mesh 载体读远端）记后续 |

---

## 4. 云同步

### 4.1 现状（已落地）

- **任务式单向同步**：`sclient sync push/pull`；服务端 `SyncManager`（`pkg/syncmgr`）托管执行；
  `--remote` 指 `sync_remotes[]` 配置的远端；任务状态机 pending/syncing/completed/failed/retrying/
  cancelled，带超时与重试（`sync.max_retries`）。
- **增量能力**：文件级增量（`pkg/sync` diff/engine/filter）、glob include/exclude、
  `--recursive`、`--follow-symlinks`、`--sync-empty-dirs`；冲突策略 `skip|overwrite|lww|conflict-rename`。
- **载体分组**：`sync_remotes[].kind` 支持 `direct`（HTTP 直连，url+AK/SK）与 `mesh`
  （mesh 隧道：node/volume/peer_pins/transport）；`transport` 选路矩阵 relay/auto/webrtc。
- **跨节点授权**：`mesh_readers` 卷 ACL 授权（owner 维度，`GET /api/mesh/acl` 只读视图）；
  出口拨号策略（loopback/私网拒绝，除非宣告服务地址）。
- **B 侧形态**：进程内 mesh 角色（推荐）或独立 `remote_read`/`remote_write` 面宣告服务。
- **任务 API**：`/api/sync/tasks` CRUD + cancel + delete（被活跃任务引用的用户卷 409 保护）；
  用户卷寻址（`remote.volume`，跨用户 404 防枚举）；Web UI 任务面板。

### 4.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **单向任务式** | push/pull 单方向、单次执行 | 双向同步需两条任务手动编排；无「持续保持一致」语义 |
| **无变化事件驱动** | `--poll-interval` 轮询（默认 2s） | 轮询有延迟与空转开销；无 fsnotify 级实时性 |
| **无删除传播** | 增量 diff 侧重新增/修改 | 源端删除不会自动反映到目标（需冲突策略兜底，语义不明确） |
| **无冲突双向合并** | 冲突策略是「选边」不是「合并」 | 文本类双向编辑无法合并 |
| **无后台常驻同步** | 任务一次性，无 daemon 模式 | 需要用户反复提交任务或脚本 cron |
| **大文件同步无专用优化** | 全量文件级复制（无块级增量/rsync 算法） | 大文件小改动全量重传 |

### 4.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：删除传播 + 双向增量** | 同步 diff 支持 delete 传播（默认 `skip`，策略可配 `propagate`）；push+pull 合并为一次双向任务（`sync --both`） | **已落地**：`sclient sync both`（一次任务 push+pull 两边一致）+ `--delete-policy propagate`（源删除传播到目标，幂等） |
| **P1：连续同步（watch）** | `sclient sync watch --remote <r>`：服务端事件通知（复用 2.3 事件流）驱动增量同步，替代轮询 | **已落地**（#441）：`sclient sync watch` 事件流驱动增量同步（去抖 500ms + 401/不可用退化轮询 + SIGINT 优雅退出）；变更秒级传播 |
| **P1：同步校验与统计** | 每次同步后校验和核对报告（成功/跳过/冲突/失败清单）；`/api/sync/tasks/{id}` 带文件级明细 | **已落地**（#435+#442）：`--verify` 重读 checksum 比对（不一致标 VerifyFailed）+ 汇总 + 失败清单；**失败单文件重试**（#442：POST /api/sync/tasks/{id}/retry + sclient sync retry）；`/api/sync/tasks/{id}` 已带 Results 明细 |
| **P2：块级增量同步** | 类 rsync 滚动校验块（强弱校验对），只传差异块 | 大文件小改动带宽开销与改动量成正比 |
| **P2：冲突合并** | 文本冲突 3 方合并（base+ours+theirs）或冲突文件+索引 | **已落地**（#461 merge3 + #464 冲突索引 API）：diff3 纯 Go 自动合并 + `GET /api/sync/conflicts` + resolve（ours/theirs/manual 写回）；**已知限制**：单向 sync 无三方祖先，冲突标记不自动触发（自动合并可用） |
| **P2：多节点扇出** | 一次 push 到多个 `sync_remotes`（扇出），失败节点独立重试 | **已落地**（#459）：`sync_remotes` 多目标一次提交扇出，失败节点独立重试（单目标失败不影响其它） |

---

## 5. 跨墙可识别性

### 5.1 现状（已落地）

> 本方向分三层：**① 跨墙可用性**（能不能通）→ **② 流量伪装**（像不像正常流量）→ **③ 服务自识别**
> （是谁/什么状态可观测）。当前 ①③ 已较完整，**② 基本空白**——加密不解决「可识别性」。

- **可用性（①）**：多传输层可插拔（TCP/WS/QUIC/gRPC/WebRTC，`xfer.Register`）；
  hub 中继（WS 挂主 HTTP 端口，与文件服务同端口）穿透 NAT；WebRTC 打洞（STUN/TURN +
  ICE 凭据缓存单飞续期）+ hub 信令；mDNS/DHT 发现；mesh 多路径自动选路（SmartDial：
  直连 3s 超时回退出口，race 并行竞速）；正向 HTTP 代理（http-proxy，http_proxy 环境变量
  开箱即用）；via-relay/via-direct 多跳。
- **自识别（③）**：`GET /version`、`/livez`/`/readyz`/`/healthz`、`/metrics`（含 mesh 拨号
  载体/回落/授权拒绝指标）、`/api/hub/nodes|services|stats`、mesh 状态 `/api/mesh/status`、
  审计日志；SIGHUP 软配置热重载（log_level 等）。
- **加密与身份**：AES-256-GCM 隧道 + ECDH(X25519) 会话密钥（前向保密）+ 公开指纹派生静态密钥
  防降级 + Ed25519 身份双向指纹 pinning（`sclient identity` / `--peer-pins`）；端到端加密
  字节流（L⇄T 应用层 E2E，X 只透传密文）；mTLS 客户端证书。

### 5.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **流量特征明显（DPI 可识别）** | 自定义 TLS 证书 + 自定义帧协议（长度前缀 + 密文块）；WSS 也是自签证书 + `/ws` 固定路径 | GFW/企业 DPI 可凭 TLS 指纹（JA3/JA4）、证书来源、路径、流量模式识别并封锁（见 `docs/archive/architecture-decisions.md` §4：跨墙关键不是加密强度而是像不像正常流量） |
| **无 CDN 前置方案** | 架构决策已指出 CDN 前置比协议选择更重要（SNI 显示 CDN 域名），但无部署指南/反向代理配置 | 自建 TLS 端点 SNI 直接暴露 sproxy 域名 |
| **无协议伪装层** | 明确「不做自实现混淆」（GFW 跟进快）——但未提供「用现有协议形态」的方案 | 强管制网络下 WSS 裸奔 |
| **QUIC/gRPC 传输未装配** | `ext/quic`、`ext/grpc` 实现 + 测试存在，但 `relay` 只支持 `ws/tcp`、hub 只装配 ws/tcp | 已实现的抗识别传输（QUIC 0-RTT/UDP 形态）在生产用不上 |
| **无网络级可观测** | 有拨号指标，无传输层丢包/重传/往返时长指标 | 跨墙链路劣化难以定位是 DPI 限速还是网络抖动 |
| **多级 fallback 无策略** | 架构决策提过「直连→隧道→中继三级 fallback」，实际有 relay/auto 两档 | 无按丢包率/延迟动态切换的传输策略 |

### 5.3 演进路线

> 遵循既有架构决策（`docs/archive/architecture-decisions.md` §4）：**不做自实现协议混淆层**；
> 优先用「现有协议形态」与部署形态解决可识别性。

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：抗识别部署白皮书** | 文档：CDN 前置（Cloudflare/自建 Nginx + 反向代理 WS/TLS 终结）、证书管理（ACME 正式证书替代自签，消除证书来源指纹）、`/ws` 路径自定义、流量形态建议（WSS 混入正常 Web 流量） | **已落地**（#453 清单 + #476 [cdn.md](./cdn.md) 拓扑）：[stealth.md](./stealth.md) 完整白皮书（TLS 指纹成因/收敛配置/检测方法/残余差异）+ CDN 前置拓扑 + ACME 证书替换步骤 + WS 路径形态；CLI 支持 `--ca-file`/`tls.cert_file` |
| **P0：QUIC 传输装配** | `relay`/hub 增加 `--transport quic`（复用 `ext/quic`，UDP 形态抗 DPI 干扰）；文档登记 | **已落地**：`sclient relay --transport quic` 与 `hub.transports.quic` 端到端可用（[cli.md](./cli.md) 登记）；xfertest 套件全绿 |
| **P1：被动伪装层** | 不引入新混淆算法，做「形态对齐」：TLS 握手参数贴近主流 HTTP 栈（可配置 cipher 顺序/ALPN）；WS 路径与升级头可配置；连接空闲填充可开关 | **已落地**（#453/#459/#469）：`tls.cipher_order`/`tls.alpn` 形态对齐 + `tunnel.idle_padding` 空闲填充 + `hub.transports.ws.path`/`upgrade_header` WS 形态 + 质量触发动态切换（#469）；开关显式默认保守（见 [stealth.md](./stealth.md) JA3 清单） |
| **P1：传输质量感知选路** | 传输层丢包/重传/RTT 指标（复用 mux 统计）入 `/metrics`；SmartDial 候选加入质量加权（不只是超时） | **已落地**（#430 指标 + #446 质量加权选路：`mesh connect --quality-routing` 显式开关，候选按重传率加权降序启动、同 RTT 质量高者先胜） |
| **P2：CDN WebSocket 官方指南 + 多级 fallback 策略** | 部署文档给出 CDN（含 WS 支持）前置完整拓扑与排障；传输策略从「超时回退」升级为「质量触发动态切换」（防抖 + 手动锁定） | **已落地**（#469 动态切换 + docs/cdn.md）：[cdn.md](./cdn.md) 给出 CDN/Nginx 前置完整拓扑（ACME 证书消除指纹、WS 路径形态对齐、升级头校验）与排障清单；动态切换有日志与指标证据、可关闭 |

---

## 6. 性能

### 6.1 现状（已落地）

- **传输管线**：分块上传/下载并发（默认 4）、断点续传、流式窗口（mux `FrameWindowUpdate`
  流控 + 单流 buffered 上限 fail-closed）、64 KiB 密文块恒定内存、gzip 响应中间件。
- **mux 热路径优化**：快路径 2 次原子 load + 1 次 Add + 无竞争锁配对、零分配（见
  `pkg/tunnel/mux/stream.go`）。
- **基准套件**：`pkg/server/benchmark_test.go`（Upload/Download/ConcurrentUploads/ChunkedUpload
  1 MiB 基准）；CI Benchmark job（含 10 分钟超时取消重试 + I/O 塌陷守卫，见
  `docs/archive/benchmark-ci.md`）。
- **隔离与连接池**：`netutil.DefaultTransport` 共享工厂（生产装配层）+ `IsolatedTransport`
  （SDK/测试隔离）；全仓显式 Transport（R19/R20 门禁）；`pkg/testutil.IsolatedClient` 收敛。
- **安全可观测**：telemetry 追踪骨架（span + slog + `traceparent` 传播）、OTLP 导出骨架。

### 6.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **无索引导致搜索/列表 O(N)** | 搜索全量扫描；列表实时枚举 | 大库性能随文件数线性退化 |
| **无基准基线门禁** | 有 benchmark 套件，无「回归即红」门禁（CI 只有超时/I/O 塌陷守卫） | 性能回归无人值守发现 |
| **无端到端带宽基准** | 只有服务端 handler 级基准 | 隧道/mux/传输全链路吞吐无量化 |
| **无上传/下载限速** | 有 rate_limit（tunnel handler 限流）但无文件级带宽控制 | 多租户大传输互相挤占 |
| **无内存/GC 调优观测** | 有 metrics 框架，无 pprof/内存分配指标端点 | 大传输内存峰值难定位 |
| **gRPC/QUIC 传输闲置** | 实现存在未装配（见 5.2） | 潜在更快传输（QUIC UDP 无队头阻塞）不可用 |
| **无客户端带宽/进度统计** | sclient 有进度显示，无速率/耗时统计输出 | 脚本化场景无法评估传输质量 |

### 6.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：搜索/列表索引**（与 2.3 P0 同源） | 文件名索引 + 目录物化计数 | **已落地**（同 2.3 P0：#422 内存增量 + #483 快照持久化） |
| **P0：基准基线门禁** | `benchstat` 基线入库（`benchmarks/baseline/<GOOS>.txt`，git 跟踪），`make bench-gate` 对比历史基线，回归超阈值（默认 ±15%，`BENCH_GATE_THRESHOLD` 可配）即红 | **已落地**（#420）：`benchmarks/baseline/` git 跟踪基线 + `make bench-gate`（±15% 默认，`BENCH_GATE_THRESHOLD` 可配）+ CI Benchmark job 输出基线对比 |
| **P1：传输质量指标入 metrics**（与 5.3 P1 同源） | mux 重传/丢包/流控等待、xfer 各传输层延迟指标 | **已落地**（#430）：`sproxy_mux_retransmits_total` / `retransmit_queue_full_total` / `retransmit_exhausted_total` 等入 /metrics（面板可见） |
| **P1：文件级带宽限速** | upload/download 可选带宽上限（`--bwlimit`/配置），token 桶实现 | **已落地**（#425）：`rate_limit.bandwidth` per-owner token 桶（独立桶互不影响）+ 限速生效可观测（/metrics + 审计） |
| **P1：QUIC 传输装配**（与 5.3 P0 同源） | relay/hub `--transport quic` | **已落地**：`sclient relay --transport quic` + `hub.transports.quic`（UDP 形态，自带 TLS/ALPN `sproxy-quic`）；xfertest 套件全绿 |
| **P2：内存观测 + 自动调优** | `/debug/pprof` 端点（受认证保护）+ 分配指标；大传输缓冲水位自动调整（复用 mux buffered 统计） | **部分落地**（#463）：`/debug/pprof` 受认证保护（`debug_pprof_enabled` 显式开关默认关）+ `/metrics` 分配指标（heap_alloc/objects/gc）；**残余**：缓冲水位自动调整未做 |
| **P2：客户端传输统计** | sclient `--json` 输出补速率/耗时/分块成功率 | **已落地**（#436）：upload/download/cloud-download 表格追加统计行（耗时/速率/文件数/分块成功率）+ `--json` 补 `stats` 字段（脚本可解析） |

---

## 7. 演进原则（长期约束）

1. **零回归优先**：任何默认值改动以「单卷/单节点/旧配置不破坏」为前置（多卷/多传输均遵循）。
2. **安全开关可观测**：新安全/伪装/降级开关必须显式配置 + 生效状态可观测，禁静默降级
   （沿用端到端加密红线：显式 pinning/显式开关）。
3. **接口先行、可插拔**：新后端/传输/变换用注册表（`RegisterBackend`/`xfer.Register`/
   `RegisterTransform`）扩展，前端动态感知（`/api/backends` 模式）。
4. **不做自实现协议混淆**：跨墙方向只做「形态对齐 + 部署形态」（CDN 前置/正式证书/复用
   现有协议），不自研混淆算法（GFW 跟进快于迭代，既有架构决策）。
5. **测试纪律不变**：TDD + 变异验证；新传输进 `xfertest` 跨传输套件；Web UI 改动带
   node 单测 + Playwright e2e；测试只绑 loopback。
6. **性能演进带证据**：性能/传输改动必须挂基准（基线对比），无基准支撑的「优化」不合并。
7. **文档与代码同 PR 收敛**：本路线图条目落地时同步更新对应权威文档；条目状态在本表
   标记，避免「路线图与实现脱节」。
