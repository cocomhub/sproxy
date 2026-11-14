<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 设计发展规划（Roadmap）

> 本文是 sproxy 的**权威路线图**：文件服务 / 多卷 / 云同步 / 跨墙可识别性 / 性能 / 通知与可观测 /
> Mesh 私有组网 七个方向的**现状能力盘点**、**演进原则**与**未落地 Backlog**。随实现演进同步更新
> （与各功能权威文档 [api.md](./api.md) / [config.md](./config.md) / [cli.md](./cli.md) /
> [architecture.md](./architecture.md) / [tunnel.md](./tunnel.md) 配套）。
>
> 结构：§1–§8 按方向给「现状」与「演进状态」；§9 演进原则（长期约束）；§10 Backlog（未落地规划项）。
> 历史规划波次（v0.20+ ~ v0.23+ 及 secretdata 生产化）的**设计意图/取舍/被否决方案/落地记录**已归档至
> [docs/archive/roadmap-planning-waves-2026-09-10.md](./archive/roadmap-planning-waves-2026-09-10.md)。
>
> 状态口径：`已落地` = 已合入 master；Backlog 优先级沿用原文 S1/S2/S3（价值×投入档）与 P0–P3（里程碑）。

## 1. 总览

| 方向 | 现状定位 | 未落地（残余） |
|------|----------|----------------|
| 文件服务 | 上传/下载/分块/版本/分享/搜索+索引/事件流/审计/多用户/递归删除/回收站/服务端 WebDAV+S3/at-rest 加密 | 无 |
| 多卷 | 本地多盘 + 外部后端（baidupcs/webdav/s3/sftp/ftp）+ 镜像/分层/联邦/多副本/S3 服务端 | 无 |
| 云同步 | 文件级增量 + 双向（`sync both`）+ watch + 块级增量（跨 FS）+ 冲突合并 diff3 + 扇出 + cron | 无 |
| 跨墙可识别性 | 加密/指纹/pinning/多传输（TCP/WS/QUIC/gRPC/WebRTC）+ 被动伪装 + CDN 前置 + QUIC 0-RTT | 主动混淆（明确不做） |
| 性能 | 并发分块/断点续传/流式窗口/基准基线门禁/索引/带宽限速/内存观测 | relay 全链路带宽基准（§10.4） |
| 通知与可观测 | 指标/审计/事件流/追踪 + 通知外发（wecom/serverchan/email/webhook）+ 阈值告警引擎 | SLO 错误预算联动（§10.4） |
| mesh 私有组网 | 虚拟 IP/发现/多跳/E2E/联邦卷/出口组/用户态 VPN（`mesh up`） | VPN 真设备 tun/tap（§10.1） |

---

## 2. 文件服务

### 2.1 现状（已落地）

- **完整 REST 面**：上传（`X-File-Checksum` 强校验 + 幂等）、下载（Range/分块）、删除（checksum 匹配）、
  重命名/移动、批量操作、目录、`/api/files` 列表、`/api/files/search` 搜索、stat 单文件元信息、
  `meta` 版本历史（见 [api.md](./api.md)）。
- **分块上传/下载**：`/upload/init|chunk|status|complete` + `/download/chunk`；默认 4 MiB 块、
  并发 4、断点续传（会话 TTL 24h）、服务端块计划上界 65536（单文件默认上限 ≈ **3.999 TiB**）。
- **数据完整性**：全链路 SHA-256 checksum 强制（上传必填 `X-File-Checksum`、下载可校验、rename/delete 匹配）。
- **版本管理**：`versioning.enabled`（可选），按卷目录独立计数，restore/删除/GC。
- **分享**：token 分享 + 密码 + 过期 + 一次性/计数，原子持久化（重启恢复）。
- **多租户 + 配额**：租户自包含六桶布局（`user/cloud/archive/chunk/version/meta`），每租户
  `*os.Root` 防穿越 + `quota.Scope` 双账本（reserve→Commit/Adjust/Release，重启扫描校准）。
- **用户体系**：凭据 Ring（SproxySig v2 签名）+ TOTP 注册/登录（session SK）+ AK/SK 轮换 +
  静态加密存储（aesgcm / Vault Transit 后端，token 支持 `token_env`/`token_file`）。
- **审计**：`audit.buffer_size` 有界内存环形缓冲（默认 2048）+ `GET /api/audit` + `/api/audit/export`
  JSON 导出；审计行独立 JSON logger 机器可检索。
- **备份/恢复**：整根 tar.gz + manifest 版本校验（拒绝跨版本恢复），`make backup/restore`。
- **归档**：`POST /api/archive` 压缩/解压任务 + sclient `archive`/`archive-dir`（zip 打包目录）。
- **批量离线下载**：`cloud-download-group`（组链式：创建组→等待→打包→下载→清理）。
- **服务端 WebDAV（远端卷挂载）**：`sproxy dav remote://node/vol[/path]` 把**远端卷**暴露为本地
  WebDAV 端点（RFC 4918：PROPFIND/PUT/GET/COPY/MOVE，任意工具 curl/rsync/文件管理器可挂载，
  见 [config.md](./config.md) WebDAV 网关节）。

### 2.2 演进状态（均已落地）

文件名/内容索引（#422 内存增量 + #483 快照持久化）、大文件上限可配 + 超限 413 自动转分块（#479）、
内容寻址去重（#426/#429）、服务端事件通知 + Web UI 实时刷新（#433/#437/#434）、审计落盘 + 过滤查询
（#431）、递归删除、服务端 WebDAV 挂载面（本地卷）、上传管线扩展缩略图（#472/#475/#478）、gzip 压缩
变换、回收站/软删除、配额预警（AlertEngine `quota_watermark`）、分享权限细化（readonly + MaxDownloads
+ 图片水印）、at-rest 加密（服务端卷级 `EncryptedFS` + 客户端 E2EE）、加密归档插件化
（`RegisterCipher`）。**本方向无未落地项。**

---

## 3. 多卷

### 3.1 现状（已落地）

- **本地多卷**：`volumes[]` 配置（`pkg/volume` 纯域 + `pkg/server/volumes.go` 装配）；每卷独立
  物理根 + `LAYOUT_VERSION` 校验 + 容量池（`pkg/quota.Pool`）；placement `prefer-default`/`spread`；
  卷 ACL（deny/allow）；meta 单点权威在默认卷（checksums/凭据/任务状态）。
- **卷操作**：`POST /api/volumes/move`（跨卷流式复制原子迁移）、`rebalance`（大小降序逐文件迁移）、
  `GET /api/volumes`（per-owner 视图）；sclient `volumes`/`--volume`/`mv --to-volume`；WebUI 卷 badge/仪表/上传下拉。
- **外部后端框架**：`pkg/volume/registry.RegisterBackend(type, factory)` 可插拔；`GET /api/backends`
  动态列出；已注册 `baidupcs`（独立 module 二进制优先+库兜底）、`webdav`、`s3`、`sftp`、`ftp`。
- **用户自有卷**：per-owner 网盘盘（`POST/GET/DELETE /api/volumes/user`，`<owner>/meta/volume/*.json`
  原子持久化 + 重启扫描恢复）；同步任务 `remote.volume` 寻址，跨用户 404 防枚举。
- **容量账本**：外部卷容量 = 本系统可用限额（`UserVolume.Capacity`），backend 级 Total/Used 查询。

### 3.2 演进状态（均已落地）

跨卷复制/镜像（#417）、冷热分层 hot→warm→cold（#452/#466）、外部后端扩展 SFTP/s3 直传/FTP
（#454/#460/#473/#477）、卷健康/迁移仪表（#432/#440/#448）、多副本与联邦卷（#484）、S3 兼容服务端
（#515/#519/#525）、联邦卷回写（#516，`extra.conflict_mode` 三分支）。**本方向无未落地项。**

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

### 4.2 演进状态（均已落地）

删除传播 + 双向增量（`sync both` + `--delete-policy propagate`）、连续同步 watch（#441）、同步校验与
统计（#435/#442）、块级增量同步（v1 同 FS + v2 跨 FS）、冲突合并 diff3（#461/#464）、多节点扇出
（#459）、定时调度同步（cron）。**本方向无未落地项。**

---

## 5. 跨墙可识别性

### 5.1 现状（已落地）

> 本方向分三层：**① 跨墙可用性**（能不能通）→ **② 流量伪装**（像不像正常流量）→ **③ 服务自识别**
> （是谁/什么状态可观测）。当前 ①②③ 均已有实现（② 见 5.2 被动伪装 + CDN 前置），主动混淆明确不做。

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
- **TURN REST 短期凭证**（#141）：coturn 标准动态凭据（`--turn-rest` + 可选 user/service 参数，REST 优先于静态 user/pass，日志脱敏）——NAT 打洞无需静态 TURN 密钥。
- **云端下载经 mesh 出口**（#395）：`cloud_download_exit_node` 指定出口节点 ID，下载器「本地直连优先 → 失败回退经出口（hub 中继 RelayStream）」；非空时需 mesh.hub_url + access_key/secret（fail-closed）。
- **服务端进程内 mesh 节点**（mesh.node）：`sproxy` 自身把本机 `remote_read`/`remote_write` 面宣告到 mesh（B 侧角色，免外部 sidecar），生命周期由 `pkg/tunnel/mesh.RunNode` 承担，默认关闭零回归。

### 5.2 演进状态（均已落地）

抗识别部署白皮书（#453/[stealth.md](./stealth.md)）、CDN 前置拓扑（#476/[cdn.md](./cdn.md)）、QUIC
传输装配（#480）、被动伪装层（#453/#459/#469）、传输质量感知选路（#430/#446）、多级 fallback 动态
切换（#469）、gRPC 传输装配（#523）、QUIC 0-RTT 恢复。架构决策「不做自实现协议混淆」持续有效。
**本方向无未落地项。**

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
- **多实例协调限流**（#334/#427）：`rate_limit.bandwidth.coord_backend`（local 进程内 token 桶 / file 文件原子计数共享配额）+ `coordinated` 开关——多实例部署共享字节配额、等待不拒绝。
- **内存观测**：`/debug/pprof` 受认证保护（`debug_pprof_enabled` 显式开关）+ `/metrics` 分配指标
  （heap_alloc/objects/gc）+ mux 缓冲水位自动调整（`mux.buffer_watermark` 阈值自适应 + 防抖 + `BufferAdjustments` 指标）。

### 6.2 演进状态

搜索/列表索引（同 2.2）、基准基线门禁（#420）、传输质量指标入 metrics（#430）、文件级带宽限速
（#425）、QUIC 装配（同 5.2）、内存观测 + 缓冲水位自动调优（#463/#470）、客户端传输统计（#436）、
内容索引（`WithContentIndex`）、端到端带宽基准（xfertest，tcp/ws/quic）。**残余**：relay 全链路
（经 hub 中继段）吞吐基准——见 §10 Backlog。

---

## 7. 通知与可观测性

### 7.1 现状（已落地）

- **指标**：40+ `sproxy_*` Prometheus 指标入 `/metrics`（文件/云下载/mux 重传与流控/卷 I/O 与迁移进度/带宽限速/拨号回退/缓冲水位调整）。
- **追踪骨架**：telemetry span + slog + `traceparent` 传播 + OTLP 导出配置（`telemetry.enabled` + `otlp_endpoint`）。
- **事件流**：`/api/events` EventSource 文件变更流（upload/delete/rename/version/share，游标可回放），Web UI 实时刷新。
- **审计**：环形缓冲 + 落盘（`audit.log` 原子 append）+ 过滤查询/导出。
- **到期提醒**：SK 轮换提前量（`credentials.rotation.notify_before`）；通知外发见 7.2。

### 7.2 演进状态（均已落地）

通知中心框架（#496）、微信/Server 酱插件（#496）、邮箱插件（#498）、阈值告警引擎（AlertEngine +
`quota_watermark`/`nat_failure`/`checksum_mismatch` source）、指标深化（#497 `metrics_token` +
xfer 传输层指标）、Webhook 通用插件 + `/api/notify/test`、Grafana 官方 dashboard。**本方向无未落地项。**

---

## 8. Mesh 私有组网深化

### 8.1 现状（已落地）

- **虚拟 IP**：hub 权威分配 CGNAT 子网（`hub.virtual_subnet`）+ VipTable 防注入 + REG_OK 下发 VIP + 出口 NAT + 端口白名单；sclient `mesh connect` 支持 VIP 寻址。
- **发现与组网**：mDNS/DHT 发现、WebRTC 打洞（STUN/TURN）、hub 中继、SmartDial 竞速（直连超时回退出口 + 质量加权）。
- **多跳与安全**：via-relay/via-direct 多跳、端到端加密字节流（X 只透传密文）、Ed25519 指纹 pinning。
- **联邦卷**：mesh 载体只读挂载远端卷（federated 后端；`extra.conflict_mode` 支持回写冲突语义）。
- **多 hub 联邦**：`FederationClient`（跨 hub 节点/路由表交换，peer 周期同步，可持久化）——多 hub 集群互联。
- **出口应用形态**：SOCKS5 出口代理（`sclient socks --exit`）、UDP 端口映射（`sclient udp map --exit --remote`）、
  正向 HTTP 代理（`http-proxy`，http_proxy 环境变量开箱即用）、TCP 端口转发（`mesh connect`/`relay`）。
- **P2P 手动打洞**：`sclient p2p --manual` 手工 SDP 信令（无 hub 兜底，直接交换 offer/answer）。

### 8.2 演进状态

出口策略管理（`--exit-group` 三模式负载均衡）、服务发现健康化（`/api/hub/services` quality）、VPN
模式用户态最小集（#522 `sclient mesh up`）、节点级状态仪表（per-node metrics + `/api/hub/nodes`
quality/RTT + WebUI 拓扑）、mesh 集群化深化（跨 hub 服务发现 + 数据面中继）。**残余**：VPN 真设备
tun/tap（P2/P3）——见 §10 Backlog。

---

## 9. 演进原则（长期约束）

1. **零回归优先**：任何默认值改动以「单卷/单节点/旧配置不破坏」为前置（多卷/多传输均遵循）。
2. **安全开关可观测**：新安全/伪装/降级开关必须显式配置 + 生效状态可观测，禁静默降级
   （沿用端到端加密红线：显式 pinning/显式开关）。
3. **接口先行、可插拔**：新后端/传输/变换/加密用注册表（`RegisterBackend`/`xfer.Register`/
   `RegisterTransform`/`RegisterCipher`）扩展，前端动态感知（`/api/backends` 模式）。
4. **不做自实现协议混淆**：跨墙方向只做「形态对齐 + 部署形态」（CDN 前置/正式证书/复用
   现有协议），不自研混淆算法（GFW 跟进快于迭代，既有架构决策）。
5. **测试纪律不变**：TDD + 变异验证；新传输进 `xfertest` 跨传输套件；Web UI 改动带
   node 单测 + Playwright e2e；测试只绑 loopback。
6. **性能演进带证据**：性能/传输改动必须挂基准（基线对比），无基准支撑的「优化」不合并。
7. **文档与代码同 PR 收敛**：本路线图条目落地时同步更新对应权威文档；条目状态在 §10 Backlog
   标记，避免「路线图与实现脱节」。
8. **插件化通知与幂等**：通知渠道用注册表扩展（`RegisterNotifier`，同 `RegisterBackend`/
   `RegisterTransform` 模式）；发送须去抖/合并/失败重试，禁刷屏（告警风暴）。
9. **生态兼容优先**：对外协议（S3/WebDAV）复用成熟标准而非自研；「先消费方、后服务方」
   演进（先接外部后端，再开放自身为端点）。

---

## 10. Backlog（未落地规划项）

> 本章汇总历史规划波次（v0.20+ / v0.22+ / v0.23+ 与 secretdata 生产化）中**尚未落地**的规划项；
> 已落地条目已从本文件移除。历史规划的**设计意图 / 取舍 / 被否决方案 / 仍有效约束**见
> [docs/archive/roadmap-planning-waves-2026-09-10.md](./archive/roadmap-planning-waves-2026-09-10.md)。
> 优先级沿用原文：S1/S2/S3 = 价值×投入档；P0–P3 = 里程碑口径。设计依据指向 `docs/archive/`（已归档）
> 或 `docs/designs/`（仍为现行设计）。

### 10.1 集群 / HA 收尾

| 规划项 | 内容 | 设计依据 | 优先级 |
|--------|------|----------|--------|
| VPN 真设备（tun/tap） | Linux `ioctl TUNSETIFF` / Windows `wintun.dll` 真设备打开 + MTU/地址装配 + 真设备 e2e（P1 骨架 `pkg/vpn` 已落地） | [archive/vpn-tuntap](./archive/2026-09-24-vpn-tuntap.md) | P2/P3 |
| 集群真选主多节点 E2E | Mongo TTL 租约多节点集成 / 故障转移实测（#656 仅单测） | [archive/leader-elector](./archive/2026-09-24-leader-elector.md) | P2 |
| 写面收敛 Fortification | 非主节点写 503 + `Retry-After` + Helm 只读副本拓扑（#659 只读副本装配已落地） | — | P3 |
| StateStore Raft（F5） | etcd/raft 复制状态机（`state_store.type=raft` 现 fail-closed 未实现） | [archive/statestore](./archive/2026-09-24-statestore.md) | 不排期 |

### 10.2 Web UI 易用性（v0.23+ 头等优先级）

| 规划项 | 内容 | 设计依据 | 优先级 |
|--------|------|----------|--------|
| V1 可交互 Hub 拓扑 | 缩放/平移/自适应布局/节点详情面板/全屏（现 `topologySvg` 固定画布无交互） | [designs/webui-interactive-topology](./designs/2026-09-28-webui-interactive-topology.md) | S1 |
| V2 响应式布局 + 容器解放 | `.container` 自适应 + ≥1280 / 768–1279 / <768 断点 + 表格触屏友好（现 `max-width:1000px` 无断点） | [designs/webui-responsive-layout](./designs/2026-09-28-webui-responsive-layout.md) | S1 |
| V3 表格虚拟滚动 + 固定表头/首列 | 大目录（>1000）windowed 渲染，表头/文件名列 sticky | [designs/webui-virtual-scroll-table](./designs/2026-09-28-webui-virtual-scroll-table.md) | S2 |
| V4 统一对话框/确认组件 | 替换原生 `confirm()`/`prompt()`：`showConfirm` + 进度对话框 + ARIA/键盘 | [designs/webui-dialog-component](./designs/2026-09-28-webui-dialog-component.md) | S2 |
| V5 上传队列卡片 | 文件名/大小/进度/速度/剩余/暂停取消（拖拽放置高亮已落地） | [designs/webui-drag-upload-queue](./designs/2026-09-28-webui-drag-upload-queue.md) | S2 |
| V6 所见即所得反馈体系 | 危险操作分级确认、操作后定位/高亮、骨架屏、空态引导 | [designs/webui-wysiwyg-feedback](./designs/2026-09-28-webui-wysiwyg-feedback.md) | S2 |
| V7 残余 | 外部卷（egress/secretdata）holder_owner 专属浏览区 + 统一完善（①② 播放器/明文目录浏览已落地） | — | P3 |
| i18n 动态文案迁移 | app.js 全部动态文案迁 `i18n.js`（框架已落地，当前仅覆盖关键按钮/导航） | [archive/webui-i18n](./archive/2026-09-24-webui-i18n.md) | P3 |

### 10.3 组网 / 代理 / 第三方接入

| 规划项 | 内容 | 设计依据 | 优先级 |
|--------|------|----------|--------|
| MCP 工具面扩充 + SSE 加固 | 目录浏览/事件流/同步状态/分享撤销工具 + SSE 认证强化（现 9 工具） | [archive/mcp-server](./archive/2026-09-24-mcp-server.md) | P1 |
| SOCKS5 UDP-ASSOCIATE | 现仅 CONNECT；UDP 经 mesh 隧道 | — | P2 |
| 服务端 mesh 出口/代理入口 | sproxy 自身充当 mesh 出口/代理（现仅 sclient 侧发起） | — | P2 |
| 隧道 dial 强化 | 多类型目标 dial + 跨 hub 中继可配置并行通道 | — | P3 |
| http-proxy 全局路由 + 出口编排 | http-proxy 接入 `--route`/出口组策略编排 | [archive/mesh-route-rules](./archive/2026-09-24-mesh-route-rules.md) | P3 |
| OIDC/LDAP 演示收敛 + 协议加深 | OIDC/LDAP 演示一致性 + S3 兼容加深 + WebDAV 多卷接入 | [archive/oidc-ldap](./archive/2026-09-24-oidc-ldap.md) | P3 |

### 10.4 架构 / 可维护 / 其它能力

| 规划项 | 内容 | 设计依据 | 优先级 |
|--------|------|----------|--------|
| 服务端 WebDAV LOCK 持久化 | 现 `NewMemLS`（重启/多实例锁丢失） | [designs/webdav-lock-persist](./designs/2026-09-24-webdav-lock-persist.md) | P3 |
| 统一中台网关认证 | SproxySig/Bearer/OIDC/LDAP 收敛单一认证入口 | — | P3 |
| 门禁扩展 | StateStore 不可直写（TypeStore）+ 审计落盘扩展 + 指标统一 | — | P3 |
| 混沌注入扩展 | 集群场景（网络分区/延迟/Kill9）+ e2e 覆盖扩充 | [archive/chaos-testing](./archive/2026-09-24-chaos-testing.md) | P2 |
| 重复文件发现残余 | 服务端路由 + sclient + metrics（P1 域纯逻辑已落地） | [designs/duplicate-finder](./designs/2026-09-24-duplicate-finder.md) | P2 |
| sclient 并发批量进度条 | `--progress` 进度条（并发执行已落地） | [archive/sclient-concurrent-batch](./archive/2026-09-24-sclient-concurrent-batch.md) | P2 |
| SLO 错误预算联动 | 错误预算规则联动 AlertEngine（直方图/Apdex 已落地） | [archive/slo-metrics](./archive/2026-09-24-slo-metrics.md) | P1 |
| AI 事件流水线残余 | enqueue 落审计 + 周期扫描兜底（#667 主体已落地） | [archive/ai-event-pipeline](./archive/2026-09-24-ai-event-pipeline.md) | P3 |
| 凭据轮换深化 | 在途 drain / 跨实例协调（热替换已落地） | [archive/credential-rotation](./archive/2026-09-27-credential-rotation.md) | P2 |
| 多实例协调限流 E2E | `rate_limit.coordinated` 多副本一致性端到端验证 | — | P3 |
| 分享 ACL 细化 | `allow_upload`/目录级授权（分享天然只读；需先补设计） | — | P3（不排期） |
| RBAC operator 档 | operator 只读运维档（reader 档已落地） | [archive/rbac-roles](./archive/2026-09-24-rbac-roles.md) | P3 |
| 端到端带宽基准残余 | relay 全链路（经 hub 中继段）吞吐基准（tcp/ws/quic 已落地） | — | P3 |
| sclient 版本自检 | CLI 升级提示（低频，价值低） | [archive/sclient-version-check](./archive/2026-09-24-sclient-version-check.md) | P3 |

**已裁定不做**：WebSocket 服务端推送（SSE 已满足实时刷新）、S3 生命周期策略（回收站 TTL / 版本 GC + 冷热分层已覆盖）。

### 10.5 secretdata 生产化待办（用户裁定 2026-10-18）

| 规划项 | 内容 | 优先级 |
|--------|------|--------|
| ① 外部源嵌套接线收口 | `type: secretdata` + `extra.target: "<外部卷>/<子目录>"` **嵌套形态在外部源上的真实网盘 e2e**（装配层已支持、外部后端替身单测已覆盖，但 e2e 仅覆盖 local target） | P1 |
| ① 性能：消除分块二次本地拷贝 | secretdata 临时分块路径经 `WithStagedLocalPath`/`WriteFromStagedPath` 交给内层（峰值 1×） | P1 |
| ① 读性能量化 | `RangeReader` 段级快路径（每 4 MiB 窗口 HTTP 请求数 / 顺序读吞吐）实测 | P1 |
| ① 稳定性 | 远端超时/重试/断连集成测试（假外部 FS 覆盖整链） | P1 |
| ① 用户面 | `extra.target` 文档 + `config.example.yaml` 示例 + 嵌套建卷 API/Web UI 核对 | P2 |
| ③ 回收站各卷独立 + 汇总 | `listTrashHandler`/`EmptyTrash` 现仅默认卷根；补卷维度 API（`?volume=`）+ Web UI 卷筛选 + GC 覆盖各卷 | P1 |
| ④ 加密卷容量真值 | 本地加密卷容量接 root 物理扫描（消除 ~0.1% 逻辑偏差） | P2 |
| ④ presign per-volume 记账 | presign 直传 per-volume 记账/预检 | P2 |
| ④ 多 target 副本入账 | `targets` 实验能力的副本字节入账 | P2 |
| ⑤ 规格-实现缺口 | `TransferFSFor` 第三返回值语义 / `DisableDoubleDownloadVerify` | P3 |
| ⑤ Windows 文件锁 flake | `pkg/tunnel/hub` 持久化偶发 | P3 |
| ⑤ legacy meta 读退化 | 无 blocklet 索引时读内存退化为单块 | P3 |
| ⑤ `Decl.SourceKey(owner)` | 设计 §5；跨外部卷源封装前置 | P3 |
| ⑤ 锁定语义 | 卷非空但根不可解析时放行 + 两条写路径 size 口径差异（既有锁定） | P3 |

### 10.6 secretdata 已落地归档

① 真实网盘 e2e（`secretdata_real_e2e_test.go`：写入→读回逐字节一致 + 远端任一 blob 不含明文）、
② secrets 按卷格式存取（URI + 装配期读一次缓存）、①-附 秒传收敛与块级交错
（`EncryptShardsReaderToChunks`、本地峰值 O(1 块)、回退链）均已落地并销账；设计背景与已知残余见归档件。
