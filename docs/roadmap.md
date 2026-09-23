<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 设计发展规划（Roadmap）

> 本文是 sproxy 的**权威路线图**：文件服务 / 多卷 / 云同步 / 跨墙可识别性 / 性能 /
> 通知与可观测 / Mesh 私有组网 七个方向的现状能力盘点、差距分析与演进路线。随实现演进同步更新
> （与各功能权威文档 [api.md](./api.md) / [config.md](./config.md) / [cli.md](./cli.md) /
> [architecture.md](./architecture.md) / [tunnel.md](./tunnel.md) 配套）。
>
> 里程碑口径：**P0（已规划/近期）→ P1（中期）→ P2（远期探索）**。
> 状态字段：`已落地` = 已合入 master；`已规划` = 设计或排期已定；`待设计` = 需先做方案再动工。
> 每条演进都带「验收标准」——合并功能 PR 时对照更新。

## 1. 总览

| 方向 | 现状定位 | 最大差距 | 首要里程碑 |
|------|----------|----------|------------|
| 文件服务 | 功能面完整（上传/下载/分块/版本/分享/搜索/审计/多用户/递归删除/索引） | 无服务端 WebDAV 挂载面；无服务端压缩 | P1 服务端 WebDAV + 服务端压缩 |
| 多卷 | 本地多盘 + 外部后端框架（baidupcs/webdav/s3/sftp）+ 镜像/分层/联邦已落地 | 联邦卷只读（无回写）；后端生态仍薄（缺 FTP/oss/cos） | P1 联邦卷回写 + 后端扩展 |
| 云同步 | 文件级增量 push/pull + mesh 载体 + 冲突策略 + 块级增量 v2 + 删除传播已落地 | 单向任务式（无双向连续同步）、无变化事件驱动（轮询）、无定时调度 | P1 连续同步 + 事件驱动 + P2 定时调度 |
| 跨墙可识别性 | 加密/指纹/pinning/多传输已落地，**流量伪装已落地** | DPI 特征已收敛（被动伪装）但 gRPC 传输未装配；无主动混淆 | P2 gRPC 装配 + 主动伪装 |
| 性能 | 并发分块/断点续传/流式窗口/基准套件/搜索索引已落地 | 无端到端带宽基准；无全链路吞吐量化 | P2 端到端带宽基准 |
| 通知与可观测 | 指标/审计/事件流/追踪骨架已落地，**通知外发已落地**（notify-center 实现中） | 无阈值告警引擎；WS/QUIC 传输指标待补 | P1 阈值告警引擎 + 传输指标补全 |
| mesh 私有组网 | 虚拟 IP/发现/多跳/E2E/联邦卷已落地 | 端口转发形态（非全虚拟网）；出口策略单一 | P1 出口策略 + P2 VPN 模式 |

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
| **无 at-rest 加密** | 传输加密（AES-256-GCM/ECDH）已有；落盘明文 | 存储介质泄露/备份泄露风险 |

### 2.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：搜索/列表索引** | 启动/写路径增量维护文件名索引（owner 维度，可扩展内容索引）；`search` 与列表走索引；索引损坏可重建 | **已落地**（#422 内存增量 + 持久化快照）：`search`/列表走索引（亚秒级）；写路径增量 + 失效全量重建；快照落盘 `<meta>/index/<owner>.json`（`index_save_interval` 周期保存，重启载入免全量 WalkDir）；损坏/缺失快照回退重建 |
| **P0：大文件上限演进** | 普通上传上限改为可配置（`max_upload_bytes` 恢复可配，默认保持 1 GiB 零回归）；`>1 GiB` 时服务端自动转分块会话 | **已落地**（#479）：`max_upload_bytes` 可配 + 超限 413 带 `X-Auto-Chunked: true` → 客户端自动转分块（直接 POST 10 GiB 走自动分块成功，调用方无感知）；配置显式可查（`/api/config`） |
| **P1：内容寻址去重** | 上传时按 checksum 查重（同 owner 同卷同内容 → 硬链接/引用计数，可选开关） | **已落地**（#426/#429）：`dedup` 段配置开启后上传按 checksum 查重（同 owner 同卷同内容 → 硬链接零拷贝 + `meta/dedup.json` 引用计数台账）；删除引用计数归零才删 inode + 配额释放；FAT/exFAT 无硬链接回退复制 |
| **P1：服务端事件通知** | 文件变更事件流（SSE/WebSocket）：`/api/events` 订阅 upload/delete/rename/move/version | **已落地**（#433+#437+#434）：事件源覆盖 upload/rename/delete/mkdir/rmdir/version/share（#437 补 version/share）；Web UI 由轮询升级为 EventSource 实时刷新（#434，断线重连+游标回放）；事件不丢（游标可回放） |
| **P1：审计落盘 + 查询** | 审计环形缓冲可选落盘（`audit.persist`）；`/api/audit` 支持 owner/动作/时间过滤 | **已落地**（#431）：审计默认落盘 `<默认卷根>/audit/audit.log`（原子 append，启动载入历史）+ `GET /api/audit` 支持 action/actor/since 过滤 + 导出带过滤 |
| **P1：递归删除** | `rmdir`/`delete` 补 `--recursive` 递归语义（`rm -rf`），删除目录树 | **已落地**：`POST /rmdir?dirname=&force=true` 递归删除整树（`root.RemoveAll`，os.Root 防符号链接逃逸，逐卷删除）+ 清理 checksum store 前缀；sclient `rmdir --force`（含内容）|
| **P1：服务端 WebDAV 挂载面（本地卷）** | `sproxy dav` 现仅支持远端卷（`remote://`）；补**本地卷**服务端：`/dav/` 路由挂 WebDAV 协议（复用 `pkg/gateway/webdav` + 凭据 Ring 认证），任意 WebDAV 客户端直接读写本服务存储 | **已落地**：`/dav/` 路由（srvMux + authMiddleware）+ owner 租户 user 桶映射 → LocalFS 桥接 webdav.NewHandler；全流程 PROPFIND/MKCOL/PUT/GET/DELETE 经真实凭据验证。残余：无（enabled 开关 + 多卷 owner 卷选择已落地） |
| **P2：上传管线扩展** | 可选服务端压缩/缩略图/转码插件（`RegisterTransform`） | **已落地**（#472+#475+#478）：`RegisterTransform` 注册表 + 图片缩略图按需生成（`?transform=thumb&width=N`，原文件不动）+ 派生缓存（meta/transform 原子落盘 + GC） |
| **P2：服务端压缩插件** | `RegisterTransform` 挂 gzip 等压缩变换（`?transform=gzip`），文本/JSON 类存储降膨胀 | **已落地**：`?transform=gzip` 命名变换（流式 gzip 压缩，任意扩展名）+ `Content-Encoding: gzip` 响应头 + 派生缓存；内建缩略图已就绪（#472/#475）。残余：无（Accept-Encoding + Content-Type 白名单自动 gzip 已落地） |
| **P2：回收站/软删除** | `delete` 改为软删除 → 回收站（`/api/trash` 列表/恢复/清空 + 保留期 TTL），防误删 | **已落地**：`POST /delete?soft=true` 软删（checksum 校验后移到 trash 桶，不释放配额可恢复）+ `GET /api/trash` 列表 + `POST /api/trash/{restore,empty}` + CleanupTrash TTL 清理（保留期参数化）。残余：无（周期 GC + WebUI 视图已落地） |
| **P2：配额预警** | `owner_quotas` 达 80%/95% 触发预警通知（联动通知中心，`/api/stats` 暴露水位） | **已落地**：AlertEngine 加 `quota_watermark` source（per-owner 水位轮询 80%/95% 双档 + 恢复通知，各 owner 独立去抖）；装配读 quotaScope Usage/MaxBytes + 租户列表。残余：无（/api/stats quota 段已暴露水位） |
| **P2：分享权限细化** | 分享链接补只读/下载次数上限/水印（现 password/expire/once/count 已有） | **已落地**（#504 + 本批）：创建参数 `readonly`（响应标志 + X-Share-ReadOnly 头）+ 下载次数上限已有（MaxDownloads）+ **图片水印**（`?transform=thumb&watermark=<seed>` 半透明点阵叠加，标准库无字体依赖）。残余：无（分享绑定水印种子已落地） |
| **P2：配额预警** | `owner_quotas` 达 80%/95% 触发预警通知（联动通知中心，`/api/stats` 暴露水位） | **已落地**：AlertEngine 加 `quota_watermark` source（per-owner 水位轮询 80%/95% 双档 + 恢复通知，各 owner 独立去抖）；装配读 quotaScope Usage/MaxBytes + 租户列表。残余：无（/api/stats quota 段已暴露水位） |
| **P2：分享权限细化** | 分享链接补只读/下载次数上限/水印（现 password/expire/once/count 已有） | **已落地（部分）**：创建参数 `readonly`（响应带标志 + 访问响应头 X-Share-ReadOnly 语义可见）+ 下载次数上限已有（MaxDownloads）。残余：图片水印（需 image 库叠加） |
| **P2：at-rest 加密** | 落盘静态加密：服务端卷级密钥（aesgcm 复用）/ 可选客户端 E2EE（零知识，上传前加密下载后解密） | **已落地（服务端卷级）**：`pkg/sync.EncryptedFS` 透明加密卷包装（WriteFile 流式 AES-256-GCM 分块加密 + OpenRead 解密；密文格式魔数头 + 逐块 nonce\|ct\|tag；目录/元数据转发）。残余：客户端 E2EE |
| **P2：加密归档插件化** | `RegisterCipher` 加密算法注册表（同 `RegisterTransform` 模式）：AES-256-GCM 流式 / 7z `-mhe=on` 头加密（`volumes[].extra.cipher` 选型 + 密钥引用）；`POST /api/archive` 加 `encrypt` 参数支持**双层加密归档**（内层归档再套外层加密，参考 cocom 7z double）；只读加密卷 = 透明解密读取 | **已落地（部分）**：`RegisterCipher` 注册表 + 流式 AES-256-GCM 分块加密（64KiB 块 + 每块随机 nonce + 魔数头）+ `POST /api/archive encrypt:true` → `.tar.gz.aes`（archive.key_file 密钥引用）。残余：只读加密卷透明解密、`volumes[].extra.cipher` 选型 |

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
| **外部后端生态薄** | baidupcs/webdav/s3/sftp 四类 | 缺常见后端（FTP、本地其它盘、oss/cos 等） |
| **外部卷一致性弱** | 同步视图（`sync.FS`）透传，无本地校验和缓存 | 网络盘元数据每次实时拉取，慢且依赖可用性 |
| **无卷健康/迁移仪表** | 卷状态只有容量；无读写失败/延迟指标 | 盘故障难发现；rebalance 无进度面板 |
| **无异地多活/联邦卷** | 卷都是单机物理根（外部后端也是直连） | 多副本容灾需自建 |
| **联邦卷只读无回写** | `volumes[] type=federated` 只读（写方法 ErrReadOnly） | 无法通过本地卷视图修改远端 |

### 3.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：跨卷复制/镜像** | `POST /api/volumes/copy`（复制不删源）+ `mirror` 定时复制策略（`volumes[].mirror_to` + `mirror_interval`） | **已落地**（#417）：复制后源/目标 checksum 全等；镜像策略周期执行可观测（审计 `volume_mirror`/`volume_copy`）；配置校验拒自指/不存在/成环镜像链 |
| **P1：冷热分层** | 卷属性 `tier`（hot/warm/cold）+ 按大小/访问时间自动降级任务（复用 rebalance 迁移语义）；读时按需回迁 | **已落地**（#452 基础 + #466 warm 档细化）：hot→warm→cold 两级降级 + warm 独立阈值 + 读时按需回迁（API 无感） |
| **P1：外部后端扩展** | 新增 SFTP 后端；s3 补充签名 v4 直传/分片；backend 健康探针 | **已落地**（#454 SFTP + #460/#473/#477 s3 直传 + 探针）：`GET /api/backends` 动态列类型（sftp/s3/baidupcs）；后端不可达时卷状态 `degraded` 可观测（HealthProbe 拨号探测） |
| **P1：卷健康/迁移仪表** | 卷级指标（读写延迟/失败率）入 `/metrics` + WebUI 卷仪表迁移进度条 | **已落地**（#432 指标 + #440 WebUI 健康仪表 + #448 rebalance 迁移进度入 /metrics + WebUI 进度条）：面板可见每卷健康（healthy/warning/degraded 徽标）+ 迁移进度（按卷对百分比） |
| **P2：多副本与联邦卷** | 卷复制策略升级为多副本（N 节点同步）+ 只读联邦卷（远端卷只读挂载，复用 mesh 载体） | **已落地**（#484 多副本镜像 + 联邦卷）：`volumes[].mirror_targets` N 副本周期复制 + `volumes[] type=federated` 只读挂载远端 mesh 节点卷（Extra node/volume/path，hub 中继数据面 + HealthProbe degraded 可观测 + 写方法 ErrReadOnly fail-closed） |
| **P2：S3 兼容服务端** | sproxy 自身作为 S3 端点（`/s3/` 路由，AWS SigV4 签名认证 → owner 卷），外部工具（aws s3 / rclone / S3 SDK）直接读写本服务存储 | **已落地**（#515/#519/#525 + 本批）：`/s3/<key>` + SigV4 验签 + GET/PUT/DELETE/HEAD + ListObjectsV2 + **分块上传 + abort**（init/part/complete/abort，ETag md5）。残余：无（多桶语义已落地：/s3/<卷名>/<key>） |
| **P2：联邦卷回写** | 联邦卷只读 → 可写（本地写面经 mesh 隧道写回远端卷，复用 remote 写面 `/remote/block` 会话） | **已落地**（#516）：federated 后端注入写面拨号器（volwrite）+ federated.FS.WithWriter（写方法转发，未注入恒 ErrReadOnly）。残余：本地配额统计 |
| **P2：S3 兼容服务端** | sproxy 自身作为 S3 端点（`/s3/` 路由，AWS SigV4 签名认证 → owner 卷），外部工具（aws s3 / rclone / S3 SDK）直接读写本服务存储 | **已落地**（#515/#519/#525）：`/s3/<key>` + SigV4 验签 + GET/PUT/DELETE/HEAD + ListObjectsV2 + 分块上传。残余：abort multipart、多桶语义 |
| **P2：S3 兼容服务端** | sproxy 自身作为 S3 端点（`/s3/` 路由，AWS SigV4 签名认证 → owner 卷），外部工具（aws s3 / rclone / S3 SDK）直接读写本服务存储 | **已落地（最小集）**：`/s3/<key>` 路由（纯标准库 SigV4 验签：Authorization AWS4-HMAC-SHA256，AccessKey = sproxy 凭据 AK → ring CoreEntry SK 64-hex 验签）+ GET/PUT/DELETE 映射 owner 卷 user 桶。残余：ListObjectsV2、分块上传、bucket 多桶语义 |
| **P2：联邦卷回写** | 联邦卷只读 → 可写（本地写面经 mesh 隧道写回远端卷，复用 remote 写面 `/remote/block` 会话） | **已落地**（#516）：federated 后端注入写面拨号器（volwrite）+ federated.FS.WithWriter（写方法转发，未注入恒 ErrReadOnly）；volumes[] type=federated + Extra.writable 写面生效。残余：冲突语义（LWW 默认）、本地配额统计 |
| **P2：S3 兼容服务端** | sproxy 自身作为 S3 端点（`/s3/` 路由，AWS SigV4 签名认证 → owner 卷），外部工具（aws s3 / rclone / S3 SDK）直接读写本服务存储 | **已落地**（#515 + #519）：`/s3/<key>` 路由 + 纯标准库 SigV4 验签 + GET/PUT/DELETE/HEAD（HeadObject）+ **ListObjectsV2**（`?list-type=2` XML + prefix）。残余：分块上传、多桶语义 |
| **P2：联邦卷回写** | 联邦卷只读 → 可写（本地写面经 mesh 隧道写回远端卷，复用 remote 写面 `/remote/block` 会话） | **已落地**：federated 后端 RegisterBackend 注入写面拨号器（volwrite 服务名）+ federated.FS.WithWriter（WriteFile/Rename/Delete/MakeDir 转发，未注入恒 ErrReadOnly 零回归）；`volumes[] type=federated + Extra.writable=true` 写面生效（配额归属对端 hub）。残余：冲突语义/一致性（LWW 默认）、本地配额统计 |

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
| **无删除传播** | 增量 diff 侧重新增/修改（删除传播已落地 #485：`--delete-policy propagate`） | 残余：双向连续同步下删除传播的冲突语义 |
| **无冲突双向合并** | 冲突策略是「选边」不是「合并」 | 文本类双向编辑无法合并 |
| **无后台常驻同步** | 任务一次性，无 daemon 模式 | 需要用户反复提交任务或脚本 cron |
| **大文件同步无专用优化** | 全量文件级复制（无块级增量/rsync 算法） | 大文件小改动全量重传 |
| **无定时调度同步** | 无 cron 表达式调度 | 周期性同步靠脚本 |

### 4.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：删除传播 + 双向增量** | 同步 diff 支持 delete 传播（默认 `skip`，策略可配 `propagate`）；push+pull 合并为一次双向任务（`sync --both`） | **已落地**：`sclient sync both`（一次任务 push+pull 两边一致）+ `--delete-policy propagate`（源删除传播到目标，幂等） |
| **P1：连续同步（watch）** | `sclient sync watch --remote <r>`：服务端事件通知（复用 2.3 事件流）驱动增量同步，替代轮询 | **已落地**（#441）：`sclient sync watch` 事件流驱动增量同步（去抖 500ms + 401/不可用退化轮询 + SIGINT 优雅退出）；变更秒级传播 |
| **P1：同步校验与统计** | 每次同步后校验和核对报告（成功/跳过/冲突/失败清单）；`/api/sync/tasks/{id}` 带文件级明细 | **已落地**（#435+#442）：`--verify` 重读 checksum 比对（不一致标 VerifyFailed）+ 汇总 + 失败清单；**失败单文件重试**（#442：POST /api/sync/tasks/{id}/retry + sclient sync retry）；`/api/sync/tasks/{id}` 已带 Results 明细 |
| **P2：块级增量同步** | 类 rsync 滚动校验块（强弱校验对），只传差异块 | **已落地**（v1 同 FS + v2 跨 FS）：同 FS 目标块级增量（BlockDiff 差异块，相同块免传输）+ **跨 FS 远端目标**（remoteFS 实现 BlockAccessor：OpenReaderAt 经 /remote/download+Range 读旧块、OpenWriterAt 写面会话 /remote/block/{open,write,close} 差异块落盘）——大文件小改动只传差异块 |
| **P2：冲突合并** | 文本冲突 3 方合并（base+ours+theirs）或冲突文件+索引 | **已落地**（#461 merge3 + #464 冲突索引 API）：diff3 纯 Go 自动合并 + `GET /api/sync/conflicts` + resolve（ours/theirs/manual 写回）；**已知限制**：单向 sync 无三方祖先，冲突标记不自动触发（自动合并可用） |
| **P2：多节点扇出** | 一次 push 到多个 `sync_remotes`（扇出），失败节点独立重试 | **已落地**（#459）：`sync_remotes` 多目标一次提交扇出，失败节点独立重试（单目标失败不影响其它） |
| **P2：定时调度同步** | `sync` 任务支持 cron 表达式调度（`--schedule "0 */6 * * *"`），周期自动执行 | 待设计（现状：`sync watch` 事件驱动连续同步已落地（#441），但无 cron 时间表调度） |

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
- **TURN REST 短期凭证**（#141）：coturn 标准动态凭据（`--turn-rest` + 可选 user/service 参数，REST 优先于静态 user/pass，日志脱敏）——NAT 打洞无需静态 TURN 密钥。
- **云端下载经 mesh 出口**（#395）：`cloud_download_exit_node` 指定出口节点 ID，下载器「本地直连优先 → 失败回退经出口（hub 中继 RelayStream）」；非空时需 mesh.hub_url + access_key/secret（fail-closed）。
- **服务端进程内 mesh 节点**（mesh.node）：`sproxy` 自身把本机 `remote_read`/`remote_write` 面宣告到 mesh（B 侧角色，免外部 sidecar），生命周期由 `pkg/tunnel/mesh.RunNode` 承担，默认关闭零回归。

### 5.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **流量特征明显（DPI 可识别）** | 自定义 TLS 证书 + 自定义帧协议（长度前缀 + 密文块）；WSS 也是自签证书 + `/ws` 固定路径 | GFW/企业 DPI 可凭 TLS 指纹（JA3/JA4）、证书来源、路径、流量模式识别并封锁（见 `docs/archive/architecture-decisions.md` §4：跨墙关键不是加密强度而是像不像正常流量） |
| **无 CDN 前置方案** | 架构决策已指出 CDN 前置比协议选择更重要（SNI 显示 CDN 域名），但无部署指南/反向代理配置 | 自建 TLS 端点 SNI 直接暴露 sproxy 域名 |
| **无协议伪装层** | 明确「不做自实现混淆」（GFW 跟进快）——但未提供「用现有协议形态」的方案 | 强管制网络下 WSS 裸奔 |
| **QUIC/gRPC 传输未装配** | QUIC 已装配（relay/hub `--transport quic`，#480）；`ext/grpc` 仍未装配 | gRPC 传输（HTTP/2 形态）在生产用不上 |
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
| **P2：gRPC 传输装配** | `relay`/hub 增加 `--transport grpc`（复用 `ext/grpc`，HTTP/2 形态抗 DPI）；文档登记 | **已落地**（#523 + 本批）：ext/grpc 真实实现（手写 ServiceDesc + grpc-go v1.84 + 字节直传）+ relay/hub 装配 + **TLS 传输**（SPROXY_GRPC_CERT/KEY/CA，自签回落同 quic）。残余：无（会话数上限 128 已加） |
| **P2：QUIC 0-RTT 恢复** | `ext/quic` 补 0-RTT 会话恢复（首次 1-RTT 建连缓存 session ticket，后续 0-RTT 直发） | 待设计（现状：QUIC 装配已落地（#480），0-RTT 未做） |

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

### 6.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **无索引导致搜索/列表 O(N)** | 搜索/列表走内存索引（#422 增量 + #483 快照持久化） | 残余：内容索引（全文/标签）未做 |
| **无基准基线门禁** | 有 benchmark 套件，无「回归即红」门禁（CI 只有超时/I/O 塌陷守卫） | 性能回归无人值守发现 |
| **无端到端带宽基准** | 只有服务端 handler 级基准 | 隧道/mux/传输全链路吞吐无量化 |
| **无上传/下载限速** | 有 rate_limit（tunnel handler 限流）但无文件级带宽控制 | 多租户大传输互相挤占 |
| **无内存/GC 调优观测** | 有 metrics 框架，无 pprof/内存分配指标端点 | 大传输内存峰值难定位 |
| **gRPC/QUIC 传输闲置** | QUIC 已装配（#480）；gRPC 仍未装配 | gRPC（HTTP/2 形态）潜在不可用 |
| **无客户端带宽/进度统计** | sclient 有进度显示，无速率/耗时统计输出 | 脚本化场景无法评估传输质量 |
| **无端到端全链路基准** | 无隧道/mux/传输全链路吞吐基准 | 全链路性能回归不可量化 |

### 6.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：搜索/列表索引**（与 2.3 P0 同源） | 文件名索引 + 目录物化计数 | **已落地**（同 2.3 P0：#422 内存增量 + #483 快照持久化） |
| **P0：基准基线门禁** | `benchstat` 基线入库（`benchmarks/baseline/<GOOS>.txt`，git 跟踪），`make bench-gate` 对比历史基线，回归超阈值（默认 ±15%，`BENCH_GATE_THRESHOLD` 可配）即红 | **已落地**（#420）：`benchmarks/baseline/` git 跟踪基线 + `make bench-gate`（±15% 默认，`BENCH_GATE_THRESHOLD` 可配）+ CI Benchmark job 输出基线对比 |
| **P1：传输质量指标入 metrics**（与 5.3 P1 同源） | mux 重传/丢包/流控等待、xfer 各传输层延迟指标 | **已落地**（#430）：`sproxy_mux_retransmits_total` / `retransmit_queue_full_total` / `retransmit_exhausted_total` 等入 /metrics（面板可见） |
| **P1：文件级带宽限速** | upload/download 可选带宽上限（`--bwlimit`/配置），token 桶实现 | **已落地**（#425）：`rate_limit.bandwidth` per-owner token 桶（独立桶互不影响）+ 限速生效可观测（/metrics + 审计） |
| **P1：QUIC 传输装配**（与 5.3 P0 同源） | relay/hub `--transport quic` | **已落地**：`sclient relay --transport quic` + `hub.transports.quic`（UDP 形态，自带 TLS/ALPN `sproxy-quic`）；xfertest 套件全绿 |
| **P2：内存观测 + 自动调优** | `/debug/pprof` 端点（受认证保护）+ 分配指标；大传输缓冲水位自动调整（复用 mux buffered 统计） | **已落地**（#463 pprof/分配指标 + #470 缓冲水位）：`/debug/pprof` 受认证保护（`debug_pprof_enabled` 显式开关默认关）+ `/metrics` 分配指标（heap_alloc/objects/gc）+ mux 缓冲水位自动调整（`mux.buffer_watermark` 阈值自适应 + 防抖 + `BufferAdjustments` 指标） |
| **P2：客户端传输统计** | sclient `--json` 输出补速率/耗时/分块成功率 | **已落地**（#436）：upload/download/cloud-download 表格追加统计行（耗时/速率/文件数/分块成功率）+ `--json` 补 `stats` 字段（脚本可解析） |
| **P2：内容索引（全文/标签）** | 文件名索引已落地（#422/#483），补内容全文索引/标签（可选开关） | 待设计 |
| **P2：端到端带宽基准** | 隧道/mux/传输全链路吞吐基准（xfertest 跨传输套件挂 bench），量化 relay/quic/ws 全链路吞吐 | **已落地**：xfertest.BenchmarkXferThroughput（跨传输 harness 建链双向吞吐）+ tcp/ws/quic 各传输 bench_test 登记（实测 tcp 369 / ws 245 / quic 135 MB/s）+ 自动纳入 bench-baseline 门禁；mux 吞吐/并发流基准已有（BenchmarkMuxThroughput/ConcurrentStreams）。残余：relay 全链路（经 hub 中继段） |

---

## 7. 通知与可观测性

### 7.1 现状（已落地）

- **指标**：40+ `sproxy_*` Prometheus 指标入 `/metrics`（文件/云下载/mux 重传与流控/卷 I/O 与迁移进度/带宽限速/拨号回退/缓冲水位调整）。
- **追踪骨架**：telemetry span + slog + `traceparent` 传播 + OTLP 导出配置（`telemetry.enabled` + `otlp_endpoint`）。
- **事件流**：`/api/events` EventSource 文件变更流（upload/delete/rename/version/share，游标可回放），Web UI 实时刷新。
- **审计**：环形缓冲 + 落盘（`audit.log` 原子 append）+ 过滤查询/导出。
- **到期提醒**：SK 轮换提前量（`credentials.rotation.notify_before`）——但仅进程内日志告警，无主动外发。

### 7.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **无主动通知外发** | 无微信/邮箱/Webhook 任何外发渠道（通知中心实现中，notify-center 分支） | 磁盘将满、卷 degraded、同步失败等只能人盯日志/指标 |
| **无阈值告警引擎** | 有指标无规则 | 不能「指标越过阈值 → 触发通知」 |
| **`/metrics` 无认证** | 端点裸奔（metrics_token 认证实现中，metrics-auth 分支） | 指标暴露给未授权方 |
| **传输层指标缺失** | 仅 mux 层有指标；TCP 指标实现中（metrics-auth 分支），WS/QUIC 待补 | 跨墙链路劣化难定位是 DPI 限速还是网络抖动 |
| **无链路质量视图** | 有拨号指标，无端到端各 hop 延迟/丢包 | 多跳路径排障困难 |
| **无现成告警/仪表资产** | 有 helm 无 Grafana dashboard JSON | 部署方需自建面板 |

### 7.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：通知中心框架** | `RegisterNotifier` 插件注册表：事件/告警 → 通知路由（`notify.rules[]` 事件类型 → 渠道映射）；去抖/合并/失败重试/通知历史（`/api/notify/history`） | **已落地**（#496）：NotifyCenter（Register 注册表 + rules action glob 路由 + 去抖窗口 + 指数退避重试 + 有界历史 `/api/notify/history` + 渠道自检 `/api/notify/test`）；事件源 = RecordAudit（全部审计事件统一入口）异步 dispatch |
| **P0：微信通知插件** | 企业微信机器人 Webhook（`notify.channels.wecom.webhook`）/ Server 酱（`sct_key`） | **已落地**（#496）：wecom（markdown webhook）+ serverchan（sct_key）双渠道；`/api/notify/test` 渠道自检。残余：邮箱（SMTP）、Webhook 通用、阈值告警引擎（P1 后续片） |
| **P1：邮箱通知插件** | SMTP + TLS（`notify.channels.email.{smtp,from,to[]}`），HTML 摘要 | **已落地**（#498）：email 渠道（net/smtp + 465 隐式 TLS + PlainAuth + RFC 822 头 + HTML 摘要体 + RFC 2047 主题编码），失败重试复用通知中心指数退避 |
| **P1：阈值告警引擎** | 告警规则配置（`notify.alerts[]`：磁盘水位/卷 degraded/同步失败/认证暴力破解/NAT 穿透失败）+ 状态机去抖（恢复自动发恢复通知） | **已落地**：AlertEngine（rules source+threshold+channels + FIRING→OK 状态机去抖 + 恢复通知）；事件源挂点 = 磁盘水位轮询（60s）/ 外部卷探针 degraded / syncmgr 任务 failed / 登录锁定；分发复用 NotifyCenter 渠道。残余：NAT 穿透失败、规则热加载 |
| **P1：指标深化** | 传输层（TCP/WS/QUIC）指标入 `/metrics`；`/metrics` 加认证（`metrics_token` 或独立端口） | **已落地**（#497）：`metrics_token`（query/Bearer 常量时间比较，仅门 /metrics 零回归）+ xfer TCP 连接级指标（sproxy_xfer_tcp_conns/messages/bytes_*）；mux 流级已聚合。残余：无（WS/QUIC 指标 + 独立指标端口已落地） |
| **P2：Webhook 通用插件 + 外部集成** | 通用 Webhook（任意 JSON 模板）+ Alertmanager/Grafana 对接；通知渠道测试端点（`POST /api/notify/test`） | **已落地**：webhook 通用渠道（POST `{title,text,object,action}` JSON）+ `/api/notify/test` 渠道自检（#496）。残余：Alertmanager/Grafana 对接适配 |

---

## 8. Mesh 私有组网深化

### 8.1 现状（已落地）

- **虚拟 IP**：hub 权威分配 CGNAT 子网（`hub.virtual_subnet`）+ VipTable 防注入 + REG_OK 下发 VIP + 出口 NAT + 端口白名单；sclient `mesh connect` 支持 VIP 寻址。
- **发现与组网**：mDNS/DHT 发现、WebRTC 打洞（STUN/TURN）、hub 中继、SmartDial 竞速（直连超时回退出口 + 质量加权）。
- **多跳与安全**：via-relay/via-direct 多跳、端到端加密字节流（X 只透传密文）、Ed25519 指纹 pinning。
- **联邦卷**：mesh 载体只读挂载远端卷（federated 后端，roadmap 3.3 P2）。
- **多 hub 联邦**：`FederationClient`（跨 hub 节点/路由表交换，peer 周期同步，可持久化）——多 hub 集群互联。
- **出口应用形态**：SOCKS5 出口代理（`sclient socks --exit`）、UDP 端口映射（`sclient udp map --exit --remote`）、
  正向 HTTP 代理（`http-proxy`，http_proxy 环境变量开箱即用）、TCP 端口转发（`mesh connect`/`relay`）。
- **P2P 手动打洞**：`sclient p2p --manual` 手工 SDP 信令（无 hub 兜底，直接交换 offer/answer）。

### 8.2 差距分析

| 差距 | 现状 | 影响 |
|------|------|------|
| **端口转发形态，非全虚拟网** | mesh 是「服务代理」（connect 端口映射），无 tun/tap | 无法像 Tailscale 一样整网段直达（ping/组播/内网地址） |
| **出口策略单一** | `--exit` 单节点 | 无按域名分流/多出口负载均衡/故障自动切换 |
| **服务发现无质量排序** | 服务列表无健康/延迟 | 多节点同服务时无法选最优 |
| **无节点级状态仪表** | 有拨号指标，无 per-hop 延迟/丢包视图 | 跨节点排障靠手动逐跳测 |
| **SOCKS/UDP/P2P 形态分散** | socks/udp/http-proxy/mesh connect 四命令已收敛 meshconn，但无统一出口策略层 | 出口选择逻辑在命令内重复 |

### 8.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P1：出口策略管理** | exit 节点组（`--exit-group`）+ 按域名/网段分流规则 + 多出口负载均衡 + 故障自动切换（统一 socks/udp/http-proxy/mesh connect 的出口选择） | **已落地（部分）**：`--exit-group` 出口节点组（mesh.NewExitGroupDial 组内按序 failover + 本地直连优先；与 --exit/--exit-auto 互斥校验）+ AutoDial 接线。残余：域名/网段分流规则、多出口负载均衡、mesh connect 出口组 |
| **P1：服务发现健康化** | 服务列表带健康状态/延迟/RTT（复用链路质量指标），按质量排序 | **已落地**：`/api/hub/services` 响应加 `quality`（healthy/degraded/stale：基于 mux 重传/错误累计 + 节点连接时长）并按质量排序（健康在前）。残余：延迟/RTT 实时指标、WebUI 展示 |
| **P2：VPN 模式（tun/tap）** | `sclient mesh up`：虚拟子网路由进 tun/tap，整网段直达（ping/任意端口），非端口转发 | **已落地（用户态最小集）**：`sclient mesh up`（本地 SOCKS5 代理 + 虚拟子网路由到 --exit 出口；无需内核 tun/tap 特权——curl --socks5-hostname / 系统代理指向即接入）。残余：tun/tap 内核虚拟网卡（整网段透明路由，需特权 + 平台集成）、虚拟 IP 分配 |
| **P2：VPN 模式（tun/tap）** | `sclient mesh up`：虚拟子网路由进 tun/tap，整网段直达（ping/任意端口），非端口转发 | **已落地（用户态最小集）**（#522）：`sclient mesh up`（本地 SOCKS5 代理 + 虚拟子网路由到 --exit；无需内核 tun/tap 特权）。残余：tun/tap 内核虚拟网卡、虚拟 IP 分配 |
| **P2：节点级状态仪表** | per-hop 延迟/丢包/带宽入 `/metrics` + WebUI 节点拓扑图 | **已落地（部分）**：`/metrics` 输出 per-node 质量明细（sproxy_hub_node_quality{node} 0/1/2 分档 + retransmits/errors/connected_seconds{node}）+ `/api/hub/nodes` 带 quality 分档（复用 #501 判据）。残余：延迟/RTT 实时指标、WebUI 节点拓扑图 |
| **P2：mesh 集群化深化** | 多 hub 联邦已有基础（FederationClient 节点/路由交换），补跨 hub 服务发现 + 跨 hub 数据面中继（经上游 hub 路由） | **已落地（F1 跨 hub 服务发现）**：FederationClient 加服务交换（SyncServices 拉 /api/hub/federation/services + CandidateServices 跨 peer 去重 + Start 周期同步）+ 服务端 federationServicesHandler + /api/hub/services 聚合联邦服务（mesh 过滤 + node+name 去重）。残余：跨 hub 数据面中继 |

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
7. **文档与代码同 PR 收敛**：本路线图条目落地时同步更新对应权威文档；条目状态在本表
   标记，避免「路线图与实现脱节」。
8. **插件化通知与幂等**：通知渠道用注册表扩展（`RegisterNotifier`，同 `RegisterBackend`/
   `RegisterTransform` 模式）；发送须去抖/合并/失败重试，禁刷屏（告警风暴）。
9. **生态兼容优先**：对外协议（S3/WebDAV）复用成熟标准而非自研；「先消费方、后服务方」
   演进（先接外部后端，再开放自身为端点）。

---

## 10. 待设计演进详设（实现 agent 输入）

> 本章为 15 项待设计演进提供设计要点（架构/组件/数据流/错误处理/测试 + 实施片划分），
> 供实现 agent 直接照做。每项落地后把状态改「已落地」并把设计要点并入对应权威文档。

### 10.1 服务端 WebDAV 挂载面（本地卷）【P1】

- **组件**：`pkg/gateway/webdav`（现有 `NewHandler(sync.FS)`）+ 新增 `pkg/server/dav_routes.go`
  + `authMiddleware`（凭据 Ring 认证）
- **数据流**：WebDAV 客户端 → `/dav/` 路由 → authMiddleware 认证 actor → owner 卷定位
  → `pkg/sync.NewLocalFS(root)` → `webdav.NewHandler(fs)` → 读写
- **认证**：SproxySig / Bearer token（复用现有 authMiddleware），owner 从 actor 派生
- **错误处理**：认证失败 401；卷不存在 404；读写错误映射 WebDAV 状态码（403/409/412）
- **测试**：`pkg/server/dav_routes_test.go`（httptest + PROPFIND/PUT/GET 往返）；
  e2e：curl/rsync 挂载真卷验证
- **片划分**：F1 路由 + 认证装配；F2 owner 卷映射 + 只读/写权限；F3 e2e + 文档

### 10.2 服务端压缩插件（gzip）【P2】

- **组件**：`pkg/files/transform.go` 扩展（现有 `RegisterTransform` 注册表）
- **设计**：`RegisterTransform(".txt", gzipTransform)`——下载 `?transform=gzip` 时对文本类
  文件流式 gzip；派生缓存（复用 `meta/transform` 原子落盘 + GC）
- **错误处理**：不支持的扩展 400；转换失败 500
- **测试**：`transform_test.go` 补 gzip 往返（压缩→解压→内容全等）+ 变异命中
- **片划分**：F1 gzipTransform 注册；F2 缓存 + 文档

### 10.3 回收站/软删除【P2】

- **组件**：新增 `pkg/server/trash.go` + `pkg/files/trash_store.go`
- **数据流**：`DELETE /api/trash/{rel}`（软删除：移入 `<meta>/trash/<owner>/`，记录原路径
  + 删除时间）→ `GET /api/trash` 列表 → `POST /api/trash/restore` 恢复 → `DELETE /api/trash`
  清空（保留期 TTL 自动过期）
- **配额**：软删除文件仍占配额（防绕过）；恢复后配额不变
- **错误处理**：恢复时原路径被占 409；TTL 过期自动清理
- **测试**：`trash_test.go`（软删→列→恢复→清空往返 + 变异：恢复路径错误红）
- **片划分**：F1 软删/恢复/清空 API；F2 TTL 自动清理 + 配额；F3 sclient `trash` 命令

### 10.4 配额预警【P2】

- **组件**：新增 `pkg/server/quota_alert.go`（复用 NotifyCenter `Dispatch`）+ `quota.Scope`
  水位读取
- **数据流**：写路径 TryReserve 时检查水位 → 80%/95% 触发预警事件 → NotifyCenter 路由通知
- **错误处理**：预警不阻塞写（仅告警）；去抖防刷屏（复用 notify.debounce）
- **测试**：`quota_alert_test.go`（水位阈值触发 + 去抖 + 变异：阈值错误红）
- **片划分**：F1 水位检查 + 事件；F2 联动通知中心 + 文档

### 10.5 分享权限细化【P2】

- **组件**：`pkg/server/share.go` 扩展（ShareMeta 补 `permission` 字段）
- **设计**：分享补 `readonly`（只读禁传）/`max_downloads`（下载次数上限，现 once 已有）/
  `watermark`（下载水印）；`GET /s/{token}` 下载时校验
- **错误处理**：超次数 403；只读分享禁 PUT 409
- **测试**：`share_test.go` 补权限用例（变异：权限校验删除红）
- **片划分**：F1 readonly + max_downloads；F2 watermark + 文档

### 10.6 at-rest 加密【P2】

- **组件**：新增 `pkg/files/cipher_fs.go`（透明加解密 sync.FS 包装）+ 复用
  `pkg/accesskey.EncryptWithKey`
- **设计**：`volumes[].extra.encryption`（`enabled` + `key_ref`）；写路径包
  `Cipher.Encrypt`，读路径包 `Cipher.Decrypt`；密钥经凭据 Ring / Vault 托管
- **错误处理**：密钥缺失 fail-closed（拒写）；解密失败 500
- **测试**：`cipher_fs_test.go`（写→读→内容全等 + 变异：加密关闭红）
- **片划分**：F1 Cipher FS 包装；F2 卷级配置 + 密钥托管；F3 e2e + 文档

### 10.7 加密归档插件化（RegisterCipher）【P2】

- **组件**：新增 `pkg/files/cipher_registry.go`（`RegisterCipher` 注册表）+ `pkg/server/archive_cipher.go`
- **接口**：`type Cipher interface { Encrypt(w io.Writer) (io.WriteCloser, error); Decrypt(r io.Reader) (io.Reader, error); Name() string }`
- **数据流**：`POST /api/archive?encrypt=<name>&password=...` → tar.gz 后接 Cipher.Encrypt
  → 双层加密（内层 7z + 外层 Cipher，参考 cocom double）→ 下载解密
- **错误处理**：密码错 401；算法不存在 400；7z 缺失 500
- **测试**：`cipher_registry_test.go`（aesgcm 往返 + 7z double + 变异：加密绕过红）
- **片划分**：F1 Cipher 接口 + aesgcm 内建；F2 7z-double 内建 + archive 接线；F3 卷级透明解密 + 文档

### 10.8 S3 兼容服务端【P2】

- **组件**：新增 `pkg/server/s3_routes.go`（AWS SigV4 验证）+ `pkg/server/s3_handler.go`
- **数据流**：aws s3 / rclone / S3 SDK → `/s3/` 路由 → SigV4 签名验证（access_key → owner）
  → 映射 owner 卷（ListObjectsV2/GetObject/PutObject/DeleteObject）
- **错误处理**：签名错 403；桶不存在 404；权限不足 403
- **测试**：`s3_routes_test.go`（SigV4 签名请求往返 + 变异：签名验证删除红）；
  e2e：aws s3 CLI 真实验证
- **片划分**：F1 SigV4 验证 + 基础对象操作；F2 multipart/分片 + 文档

### 10.9 联邦卷回写【P2】

- **组件**：`pkg/volume/federated/federated.go` 写面扩展（复用 `/remote/block` 写面会话）
- **设计**：`federated.FS` 写方法不再恒 `ErrReadOnly`——WriteFile → `/remote/block/open|write|close`
  会话（块级增量写面）；Rename/Delete → `/remote/write` 操作
- **冲突语义**：写回冲突时 LWW / 版本检查（复用 sync 冲突策略）
- **错误处理**：远端不可达 fail-closed；冲突 409
- **测试**：`federated_write_test.go`（写→远端读一致 + 变异：写面绕过红）
- **片划分**：F1 写面接入；F2 冲突语义 + 配额归属；F3 e2e + 文档

### 10.10 定时调度同步【P2】

- **组件**：`pkg/syncmgr` 扩展（cron 表达式调度器）
- **设计**：`sync --schedule "0 */6 * * *"` → 服务端 SyncManager 挂 cron → 周期自动执行
  （复用现有任务状态机）；sclient `sync --schedule` 提交定时任务
- **错误处理**：非法 cron 400；执行冲突串行
- **测试**：`cron_schedule_test.go`（表达式解析 + 周期触发 + 变异：表达式校验删除红）
- **片划分**：F1 cron 解析器 + 调度器；F2 CLI 接线 + 文档

### 10.11 gRPC 传输装配【P2】

- **组件**：`pkg/tunnel/xfer/ext/grpc`（现有实现）+ `cmd/sclient/relay.go` + hub 装配
- **设计**：`relay --transport grpc` + `hub.transports.grpc`——复用 `ext/grpc`
  （HTTP/2 形态抗 DPI）；进 xfertest 跨传输套件
- **错误处理**：grpc 拨号失败回落；传输不可用 503
- **测试**：`xfertest` 套件全绿（grpc 传输）+ e2e
- **片划分**：F1 relay/hub 装配；F2 xfertest + 文档

### 10.12 QUIC 0-RTT 恢复【P2】

- **组件**：`pkg/tunnel/xfer/ext/quic`（现有实现）
- **设计**：首次 1-RTT 建连缓存 session ticket（TLS 会话恢复）→ 后续 0-RTT 直发
  （QUIC EarlyData，抗 DPI 干扰 + 建连提速）
- **错误处理**：0-RTT 重放拒绝（RFC 9001）；ticket 过期回落 1-RTT
- **测试**：`quic_0rtt_test.go`（首次建连→0-RTT 复用 + 变异：ticket 缓存关闭红）
- **片划分**：F1 session ticket 缓存；F2 0-RTT 装配 + 文档

### 10.13 内容索引（全文/标签）【P2】

- **组件**：`pkg/server/index*.go` 扩展（现有文件名索引）
- **设计**：索引加 `content`（全文：文本类提取 + 倒排）/`tags`（标签：`POST /api/tags`
  打标）；搜索合并文件名 + 内容命中
- **错误处理**：索引损坏重建（复用现有）；大文件跳过全文（上限可配）
- **测试**：`index_content_test.go`（全文搜索命中 + 变异：内容索引关闭红）
- **片划分**：F1 内容倒排；F2 标签 + 搜索合并；F3 Web UI + 文档

### 10.14 端到端带宽基准【P2】

- **组件**：`pkg/tunnel/xfer/xfertest` 扩展（Benchmark 挂进基准套件）
- **设计**：xfertest 跨传输套件补 Benchmark（relay/quic/ws 全链路吞吐）；
  产出入 `benchmarks/baseline/`（bench-gate 门禁）
- **错误处理**：基准超时取消（复用 benchmark-ci 超时守卫）
- **测试**：`make bench-gate` 对比基线；CI Benchmark job 输出
- **片划分**：F1 xfertest Benchmark；F2 基线入库 + 门禁

### 10.15 mesh 集群化深化【P2】

- **组件**：`pkg/tunnel/hub/federation.go`（现有 FederationClient）扩展
- **设计**：跨 hub 服务发现（FederationClient 已交换节点/路由表）+ 跨 hub 数据面中继
  （经上游 hub 路由；复用 CONNECT 而非新协议）
- **错误处理**：上游 hub 不可达降级本地路由；环路防重
- **测试**：`federation_e2e_test.go`（双 hub 互联 + 跨 hub 拨号 + 变异：路由交换关闭红）
- **片划分**：F1 跨 hub 服务发现；F2 数据面中继 + 文档
