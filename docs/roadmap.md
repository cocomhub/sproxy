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
| **P2：at-rest 加密** | 落盘静态加密：服务端卷级密钥（aesgcm 复用）/ 可选客户端 E2EE（零知识，上传前加密下载后解密） | **已落地（服务端卷级）**：`pkg/sync.EncryptedFS` 透明加密卷包装（WriteFile 流式 AES-256-GCM 分块加密 + OpenRead 解密；密文格式魔数头 + 逐块 nonce\|ct\|tag；目录/元数据转发）。残余：无（sclient --encrypt/--decrypt 客户端 E2EE 已落地） |
| **P2：加密归档插件化** | `RegisterCipher` 加密算法注册表（同 `RegisterTransform` 模式）：AES-256-GCM 流式 / 7z `-mhe=on` 头加密（`volumes[].extra.cipher` 选型 + 密钥引用）；`POST /api/archive` 加 `encrypt` 参数支持**双层加密归档**（内层归档再套外层加密，参考 cocom 7z double）；只读加密卷 = 透明解密读取 | **已落地（部分）**：`RegisterCipher` 注册表 + 流式 AES-256-GCM 分块加密（64KiB 块 + 每块随机 nonce + 魔数头）+ `POST /api/archive encrypt:true` → `.tar.gz.aes`（archive.key_file 密钥引用）。残余：无（只读加密卷透明解密 = SetEncryption 透明读写；`volumes[].extra.cipher` 选型已落地：aes-256-gcm 显式 + 未知算法 fail-closed） |

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
| **P2：联邦卷回写** | 联邦卷只读 → 可写（本地写面经 mesh 隧道写回远端卷，复用 remote 写面 `/remote/block` 会话） | **已落地**（#516）：federated 后端注入写面拨号器（volwrite）+ federated.FS.WithWriter（写方法转发，未注入恒 ErrReadOnly）。残余：无（本地配额统计经隧道 /remote/stats 展示远端配额） |
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
| **无删除传播** | 增量 diff 侧重新增/修改（删除传播已落地 #485：`--delete-policy propagate`） | 残余：无（删除传播冲突语义已落地：删除前二次 stat 目标，mtime 变化→保留 + skipped_conflict） |
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
| **P2：定时调度同步** | `sync` 任务支持 cron 表达式调度（`--schedule "0 */6 * * *"`），周期自动执行 | **已落地**：`sync schedule <cron>` 子命令（标准库 5 字段 cron 解析：分 时 日 月 周，支持 \*/N 步长/区间/列表；到点触发服务端 SyncManager 任务，与 watch 同语义）。残余：无 |

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
| **P2：QUIC 0-RTT 恢复** | `ext/quic` 补 0-RTT 会话恢复（首次 1-RTT 建连缓存 session ticket，后续 0-RTT 直发） | **已落地**：客户端 `DialTLSConfig` 装配 `ClientSessionCache`（LRU 64）+ 服务端 `Allow0RTT: true`——二次建连 0-RTT 直发（握手零往返）；E2E 二次建连用例 + 纯本地断言 + 变异命中（缓存删→红）。残余：无 |

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
| **P2：内容索引（全文/标签）** | 文件名索引已落地（#422/#483），补内容全文索引/标签（可选开关） | **已落地**：`WithContentIndex(true)` 可选开关（默认 false 零回归）+ 文本文件首 4KiB 抽样词元（写路径/全量构建同源）+ 搜索命中正文词元返回；大文件只索引头部采样。残余：无 |
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
| **无现成告警/仪表资产** | 有 helm 无 Grafana dashboard JSON | **已落地**：[sproxy-dashboard.json](./grafana/sproxy-dashboard.json) 官方面板（16 面板：请求/文件/云下载/卷 I/O/隧道/hub/传输层，Prometheus 数据源）+ [导入说明](./grafana/README.md)。残余：无 |

### 7.3 演进路线

| 里程碑 | 内容 | 验收标准 |
|--------|------|----------|
| **P0：通知中心框架** | `RegisterNotifier` 插件注册表：事件/告警 → 通知路由（`notify.rules[]` 事件类型 → 渠道映射）；去抖/合并/失败重试/通知历史（`/api/notify/history`） | **已落地**（#496）：NotifyCenter（Register 注册表 + rules action glob 路由 + 去抖窗口 + 指数退避重试 + 有界历史 `/api/notify/history` + 渠道自检 `/api/notify/test`）；事件源 = RecordAudit（全部审计事件统一入口）异步 dispatch |
| **P0：微信通知插件** | 企业微信机器人 Webhook（`notify.channels.wecom.webhook`）/ Server 酱（`sct_key`） | **已落地**（#496）：wecom（markdown webhook）+ serverchan（sct_key）双渠道；`/api/notify/test` 渠道自检。残余：邮箱（SMTP）、Webhook 通用、阈值告警引擎（P1 后续片） |
| **P1：邮箱通知插件** | SMTP + TLS（`notify.channels.email.{smtp,from,to[]}`），HTML 摘要 | **已落地**（#498）：email 渠道（net/smtp + 465 隐式 TLS + PlainAuth + RFC 822 头 + HTML 摘要体 + RFC 2047 主题编码），失败重试复用通知中心指数退避 |
| **P1：阈值告警引擎** | 告警规则配置（`notify.alerts[]`：磁盘水位/卷 degraded/同步失败/认证暴力破解/NAT 穿透失败）+ 状态机去抖（恢复自动发恢复通知） | **已落地**：AlertEngine（rules source+threshold+channels + FIRING→OK 状态机去抖 + 恢复通知）；事件源挂点 = 磁盘水位轮询（60s）/ 外部卷探针 degraded / syncmgr 任务 failed / 登录锁定；分发复用 NotifyCenter 渠道。残余：NAT 穿透失败、规则热加载 |
| **P1：指标深化** | 传输层（TCP/WS/QUIC）指标入 `/metrics`；`/metrics` 加认证（`metrics_token` 或独立端口） | **已落地**（#497）：`metrics_token`（query/Bearer 常量时间比较，仅门 /metrics 零回归）+ xfer TCP 连接级指标（sproxy_xfer_tcp_conns/messages/bytes_*）；mux 流级已聚合。残余：无（WS/QUIC 指标 + 独立指标端口已落地） |
| **P2：Webhook 通用插件 + 外部集成** | 通用 Webhook（任意 JSON 模板）+ Alertmanager/Grafana 对接；通知渠道测试端点（`POST /api/notify/test`） | **已落地**：webhook 通用渠道（POST `{title,text,object,action}` JSON）+ `/api/notify/test` 渠道自检（#496）。残余：无（Alertmanager webhook v2 + Grafana annotations 渠道已落地） |

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
| **P1：出口策略管理** | exit 节点组（`--exit-group`）+ 按域名/网段分流规则 + 多出口负载均衡 + 故障自动切换（统一 socks/udp/http-proxy/mesh connect 的出口选择） | **已落地（部分）**：`--exit-group` 出口节点组（`mesh.NewExitGroupDialWithMode` 三模式负载均衡：默认 failover 零回归 + round-robin 轮询 + weighted 加权；无论模式保留组内 failover 兜底；本地直连优先）+ AutoDial 接线（`--exit-group-mode`/`--exit-group-weight`）。残余：域名/网段分流规则、mesh connect 出口组 |
| **P1：服务发现健康化** | 服务列表带健康状态/延迟/RTT（复用链路质量指标），按质量排序 | **已落地**：`/api/hub/services` 响应加 `quality`（healthy/degraded/stale：基于 mux 重传/错误累计 + 节点连接时长）并按质量排序（健康在前）。残余：延迟/RTT 实时指标、WebUI 展示 |
| **P2：VPN 模式（tun/tap）** | `sclient mesh up`：虚拟子网路由进 tun/tap，整网段直达（ping/任意端口），非端口转发 | **已落地（用户态最小集）**：`sclient mesh up`（本地 SOCKS5 代理 + 虚拟子网路由到 --exit 出口；无需内核 tun/tap 特权——curl --socks5-hostname / 系统代理指向即接入）。残余：tun/tap 内核虚拟网卡（整网段透明路由，需特权 + 平台集成）、虚拟 IP 分配 |
| **P2：VPN 模式（tun/tap）** | `sclient mesh up`：虚拟子网路由进 tun/tap，整网段直达（ping/任意端口），非端口转发 | **已落地（用户态最小集）**（#522）：`sclient mesh up`（本地 SOCKS5 代理 + 虚拟子网路由到 --exit；无需内核 tun/tap 特权）。残余：tun/tap 内核虚拟网卡、虚拟 IP 分配 |
| **P2：节点级状态仪表** | per-hop 延迟/丢包/带宽入 `/metrics` + WebUI 节点拓扑图 | **已落地（部分）**：`/metrics` 输出 per-node 质量明细（sproxy_hub_node_quality{node} 0/1/2 分档 + retransmits/errors/connected_seconds{node}）+ `/api/hub/nodes` 带 quality 分档（复用 #501 判据）。残余：延迟/RTT 实时指标、WebUI 节点拓扑图 |
| **P2：mesh 集群化深化** | 多 hub 联邦已有基础（FederationClient 节点/路由交换），补跨 hub 服务发现 + 跨 hub 数据面中继（经上游 hub 路由） | **已落地（F1 跨 hub 服务发现）**：FederationClient 加服务交换（SyncServices 拉 /api/hub/federation/services + CandidateServices 跨 peer 去重 + Start 周期同步）+ 服务端 federationServicesHandler + /api/hub/services 聚合联邦服务（mesh 过滤 + node+name 去重）。跨 hub 数据面中继已落地（`federation_forward.go` Forward/防环/故障转移 + 15 测试） |

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
## 11. 下一阶段演进规划（v0.20+）

> 基于批次 1-13 功能审查与现状盘点：7 个残余补齐 + 2 个审查待办 + 3 个新方向。
> 优先级：**残余补齐（低风险高价值）> 审查待办（正确性）> 新方向（增量能力）**。

### 11.1 残余补齐（roadmap 既有标记，未完全落地）

| 里程碑 | 内容 | 状态 |
|--------|------|------|
| **P1：NAT 穿透失败告警** | AlertEngine 挂 NAT/STUN/TURN 穿透失败事件源（联动 hub 拨号失败日志）→ 通知渠道外发 | **已落地**：`nat_failure` source + `OnNATFailure/OnNATRecovered`（per-peer 去抖 + 恢复通知）；main 装配 `withNATAlert` 包装（cloud 出口拨号 / mesh node 角色拨号失败，错误原样传播 fail-closed） |
| **P1：告警规则热加载** | `notify.alerts[]` 配置变更 SIGHUP 热加载（复用软配置重载路径） | **已落地** |
| **P1：mesh 域名/网段分流** | exit 策略补 `--route <domain|cidr>=<exit-group>` 分流规则（统一 socks/udp/http-proxy/mesh connect） | **已落地**：`meshconn.Conn.Routes` + `SelectRoute`（域名后缀/cidr 网段，声明序首命中）+ `AutoDial` 命中组走 `NewExitGroupDial`（socks/http-proxy 一处收益）；udp map 命中替换出口节点（单 mux 固定出口取组内第一）；`--route` 与 `--exit-only` 互斥 fail-closed；见 [cli.md](./cli.md#--route-分流规则socks--http-proxy--udp-map） |
| **P1：多出口负载均衡** | exit 节点组补轮询/加权负载均衡（现在按序 failover） | **已落地**：`--exit-group-mode failover|round-robin|weighted`（默认 failover 零回归）+ `--exit-group-weight node:weight`；`PickExitGroup` 纯函数（round-robin 自增取模 / weighted 权重扇区轮转）+ `NewExitGroupDialWithMode`（模式只影响起点选择，无论模式保留组内 failover 兜底；旧签名委托零回归） |
| **P2：tun/tap 内核 VPN** | `sclient mesh up` 升级内核虚拟网卡（整网段透明路由，特权 + 平台集成） | 待设计（长期） |
| **P2：跨 hub 数据面中继** | 服务发现已落地，补跨 hub 数据面中继（经上游 hub 路由） | **已落地**：`federation_forward.go` `FederationForwarder.Forward`（CONNECT 转发到联邦对端 hub）+ `X-Relay-Hop`/`X-Relay-Path` 防环（`defaultRelayMaxHops=4`，超限/回源 508）+ `PeersForNode` 故障转移按序尝试 + mesh 隔离；`federation_forward_test.go` 15 用例（CrossHubRelay_EndToEnd/LoopGuard/HopLimit/Failover/PathAccumulation） |
| **P2：WebUI 节点拓扑 + 延迟/RTT** | per-hop 延迟/丢包入 /metrics + WebUI 节点拓扑图 | **已落地**：mux 心跳 Ping/Pong 采样 RTT（`Metrics.LastRTTNanos`，无在途 ping 跳过防冒充）→ `/metrics` `sproxy_hub_node_rtt_ms` + `/api/hub/nodes` `rtt_ms`（-1=无采样显式未知）；WebUI Hub 面板 SVG 拓扑（hub 中心 + 叶子节点，边色=RTT 分档 <100ms 绿 / <500ms 黄 / ≥500ms 红，未知虚线 N/A；>200 节点降级提示不卡渲染）；见 [designs/2026-09-24-webui-topology.md](./designs/2026-09-24-webui-topology.md) |

### 11.2 审查待办（正确性补齐，批次 11 P2）

| 里程碑 | 内容 | 状态 |
|--------|------|------|
| **P1：S3 complete ETag 校验** | complete 校验客户端 ETag 与落盘 part md5 匹配 + meta key 一致性（防 part 篡改/跨会话拼接） | 已落地 |
| **P1：S3 complete 配额记账** | complete 落盘前 TryReserve(合计大小) + Commit（对齐普通上传配额语义，防分块绕过配额） | 已落地 |

### 11.3 新方向（巡检发现的增量能力）

| 里程碑 | 内容 | 状态 |
|--------|------|------|
| **P1：全仓 checksum 巡检** | `POST /api/verify` 全卷一致性审计（重算 checksum 比对台账，坏文件隔离/报告），定时巡检 + 告警联动 | **已落地**（`POST /api/verify` + `verify_interval` 周期巡检 + `checksum_mismatch` 告警；见 [config.md](./config.md#全仓-checksum-巡检）） |
| **P2：卷备份/导出** | `GET /api/volumes/export`（tar 流式导出卷）+ `POST /api/volumes/import`（恢复），跨实例迁移 | **已落地**（export 流式 + manifest 校验 + import 复用写路径；见 [docs/designs/2026-09-24-volume-export.md](./designs/2026-09-24-volume-export.md)） |
| **P2：联邦卷强一致性** | LWW 之外补版本检查/冲突文件（复用 sync 冲突策略），可选 `extra.conflict_mode` | **已落地**（`pkg/volume/federated`）：`ConflictMode`（lww/version/conflict）+ `VersionedWriter` 接口（CheckVersion + WriteFileVersioned CAS）+ `WithConflictMode` 三分支转发；`extra.conflict_mode` 装配 fail-fast（写面非 VersionedWriter → 启动报错，禁静默降级） |

### 11.4 演进原则（补充）

10. **正确性优先于功能**：审查发现的正确性缺陷（P1/P2）优先于新功能——先修 S3 complete 加固，再做新方向。
11. **残余补齐优先**：既有 roadmap 标记的残余（NAT 告警/分流/负载均衡）是已验证方向的收尾，低风险高价值，先做。
12. **巡检/备份是运维底座**：全仓 checksum 巡检 + 卷导出是生产可运维性的基础能力，优先级高于增量功能。

---

> **第 11 章状态核对（2026-09-24）**：以下每项均经源码 grep 验证（行号见后），确保真实可靠——
> 未做项 = 全仓无实现命中；部分项 = 仅有基础形态无完整能力。

| # | 规划项 | 源码证据（核对方法） | 状态 |
|---|--------|----------------------|------|
| ① | NAT 穿透失败告警 | `alerts.go` `SourceNATFailure="nat_failure"` + `OnNATFailure/OnNATRecovered`（per-peer key 去抖 + 恢复）；main `withNATAlert` 包装挂点（cloud_exit.go / mesh_node.go，错误原样传播） | **已落地** |
| ② | 告警规则热加载 | `alerts.go:ReloadRules` 锁下 slices.Clone 原子换规则 + `handleSighup` 软配置路径重载（root.go:1106 `eng.ReloadRules(newCfg.Alerts.Rules)`，日志「alerts 规则已热加载」）；alerts.enabled 翻转需重启（装配期决策，Warn 明示） | **已落地** |
| ③ | mesh 域名/网段分流 | `socks.go:30-38` 仅 --dial-allow/--dial-allow-cidr 出口白名单（非分流规则） | **已落地**：`meshconn.go` `SelectRoute`/`ParseRoutes` + `AutoDial` 命中走 `NewExitGroupDial`；udp.go `routeExitNode`；`--route` 与 `--exit-only` 互斥 |
| ④ | 多出口负载均衡 | `exit_route.go` `PickExitGroup`（failover 恒 0 / round-robin 自增取模 / weighted 权重扇区轮转）+ `NewExitGroupDialWithMode`（模式只影响起点选择，组内 failover 兜底不变）+ `--exit-group-mode`/`--exit-group-weight`（默认 failover 零回归） | **已落地** |
| ⑤ | tun/tap 内核 VPN | `mesh.go` 仅用户态 SOCKS5（无 tun/tap/utun 命中） | 缺 |
| ⑥ | 跨 hub 数据面中继 | `federation_forward.go` `Forward`/`PeersForNode`（X-Relay-Hop/X-Relay-Path 防环、defaultRelayMaxHops=4、故障转移、mesh 隔离）+ `federation_forward_test.go` 15 用例（CrossHubRelay_EndToEnd/LoopGuard/HopLimit/Failover/PathAccumulation） | **已落地** |
| ⑦ | WebUI 拓扑+延迟/RTT | `mux.go` `Metrics.LastRTTNanos` + `pingSentAtNano`（pingLoop 记发出时刻、handlePongFrame 消费）；`metrics.go:writeHubNodeMetrics` `sproxy_hub_node_rtt_ms`（无采样 -1）；`hub_handler.go` `nodeRTTMs` → `/api/hub/nodes` `rtt_ms`；`app-render.js` `topologySvg`（边色分档/虚线 N/A）+ app.js `showHub` 装配；`web/e2e/webui_topology_e2e_test.go` 真浏览器 | **已落地** |
| ⑧ | S3 complete ETag 校验 | `s3_multipart.go:55-56` 写 meta、:107-108 ETag 仅输出、complete 不读 meta/不校验 req.Parts[].ETag | 缺 |
| ⑨ | S3 complete 配额记账 | `s3_multipart.go` complete 无 TryReserve/Commit | 缺 |
| ⑩ | 全仓 checksum 巡检 | `pkg/server/verify.go`：`POST /api/verify` + `verify_interval` 周期任务 + `checksum_mismatch` 告警 | **已落地** |
| ⑪ | 卷备份/导出 | `volume_export.go`：`GET /api/volumes/export`（tar 流式 + manifest.json）+ `POST /api/volumes/import`（复用 files.WriteFile + 配额 + 台账） | **已落地** |
| ⑫ | 联邦卷强一致性 | `federated.go:71-76` 写面直接转发（无版本检查/CAS，LWW 覆盖语义） | **已落地**：`federated.go` ConflictMode + VersionedWriter + WriteFile 三分支（lww 直转/version CAS/conflict 改名保留）；`backend.go` extra.conflict_mode 装配（fail-fast 禁静默降级） |

> 核对口径：`grep -rn` 全仓源码（排除 _test）；「缺」= 无实现命中；部分已落地项（quality 分档/metrics）仅为基础形态，完整能力（RTT 实时/拓扑图）未覆盖。

### 11.5 深化规划（第 11 章之外的新方向，2026-09-24 盘点）

> 与 11.1-11.3 的「残余补齐/审查待办/运维底座」不同，本节是**协议完整性与安全纵深**方向的增量能力。
> 每项均经 grep 验证（确认缺 = 全仓无实现命中；部分 = 有基础形态缺完整能力）。

| # | 里程碑 | 内容 | 源码证据 | 状态 |
|---|--------|------|----------|------|
| 1 | **P1：RBAC 角色细分（部分）** | 机制已有（RoleUser/RoleNode/RoleAdmin + requireRole 门禁，auth.go:103）；缺 reader/operator 细分档——**价值取决于只读用户/运维场景需求** | **已落地（reader 档）**：RoleReader + requireRole(reader) 只读子组（GET /download、/api/files、stat、search、download/chunk、archive-dir、versions、volumes、shares 走 fileRouteRead）；写/管理端点维持至少 user/admin（零回归）。残余：operator 档（需独立设计） | 部分 |
| 2 | **P1：IP 白名单/信任代理** | `auth.allow_ips` / 反向代理信任链（X-Forwarded-For 解析），认证前 IP 门 | **已落地**：`AuthConfig{AllowIPs, TrustedProxies}` + Validate 校验（纯 IP 归一 /32、/128，坏 CIDR 拒绝启动）+ `resolveClientIP`（信任链右向左取首个非信任项，畸形/超长/全信任回退 RemoteAddr，未配 trusted_proxies 一律忽略 XFF）+ `ipGate` 认证前门（authMiddleware 首部 + register/nonce/login 公开端点）+ ratelimit/nonce 桶键升级（trusted_proxies 装配时按真实客户端 IP 计量） | 已落地 |
| 3 | **P3：WebDAV LOCK 持久化（延后）** | LOCK/UNLOCK 现 NewMemLS（进程内存）；单实例重启丢锁协议容忍（客户端会重新 LOCK），多实例共享锁才真需要——延后 | webdav.go:47 NewMemLS | 部分（价值低） |
| 4 | **P1：S3 ListBuckets（卷即桶，不做 Create/Delete）** | 卷即桶（splitS3Bucket 首段=卷名，目录即桶语义）——**不做 CreateBucket/DeleteBucket**（避免双层命名空间，卷已有 ACL/配额隔离）；补 GET /s3/（无 list-type）返回 ListBuckets XML（aws s3 ls / rclone 感知卷即桶）；HEAD 桶存在性已有 | **已落地**：GET /s3/（无 list-type，SigV4 验签）→ ListAllMyBucketsResult XML 枚举 ACL 可见本地卷名（卷即桶；外部卷不列；xmlEscapeText 防注入）。残余：无 | 已落地 |
| 5 | ~~P2：S3 生命周期策略~~（**砍**） | 过期删除 = 回收站 TTL/版本 GC 已覆盖；转冷 = 冷热分层已覆盖——功能重叠无增量价值 | s3_*.go 无 lifecycle | 砍 |
| 6 | **P2：审计日志轮转** | audit.log 原子 append 无大小/时间轮转——补 max_size + 归档 | audit_store.go:19 仅 append | **已落地**：audit.max_size（ByteSize）+ max_archives（默认 3）持锁轮转 + 归档移位修剪；重启只载当前文件（热历史有界） | 已落地 |
| 7 | **P2：重复文件发现** | 复用 dedup 台账（dedup.json SHA-256 → 引用列表）做全仓扫描报告（同内容文件清单） | **已落地**（`pkg/files/dup_report.go`，见 [designs/2026-09-24-duplicate-finder.md](./designs/2026-09-24-duplicate-finder.md)）：`ReportFromLedger`（台账快照，refs≥2 成组）+ `ScanVolume`（walk user 桶 + SHA-256 分组，跳 symlink/meta 桶，单文件失败记 Errors 继续，ctx 取消 Truncated）→ `DuplicateBytes=Σ size×(refs-1)` 可回收空间；仅 P1 域纯逻辑（服务端路由/sclient/metrics 见设计文档片划分 P2） | 部分 |
| 8 | **P2：备份到远端卷** | 卷导出目标支持远端卷（federated/remote 写面）——本地 → 远端备份 | 卷导出本身未做（11.3） | 缺 |
| 9 | **P2：sclient 并发批量 + 进度条** | batch 命令并发执行（现逐行串行）+ 传输进度条（TUI） | 缺（批 37 裁剪：无逐行 batch 命令消费者，deadcode-check 拦截后移除） | 缺 |
| 10 | **P3：sclient 版本自检（延后）** | CLI 工具非长驻，升级提示低频——延后 | version.go 无 check | 部分（价值低） |
| 11 | **P2：Prometheus 告警规则模板** | 官方 dashboard 已有（grafana/）——补 alert.rules.yml 模板（磁盘水位/卷 degraded/同步失败） | grafana/ 仅 dashboard JSON | **已落地**：docs/grafana/alerts/alert.rules.yml（磁盘 0.85/0.95、卷 IO 失败率、备份同步失败、云下载失败率）+ 水位/备份计数指标 | 已落地 |
| 12 | **P2：Helm Ingress/TLS 补全** | helm chart 补 Ingress 资源 + TLS 证书管理（自动 ACME） | deploy 无 Ingress | **已落地**：templates/ingress.yaml（v1 + cert-manager 注解 + required fail-closed） | 已落地 |

> 优先级原则：1-3（安全/协议完整，低风险）> 4-6（协议/治理）> 7-12（增量能力/生态）。
> 与 11.1-11.3 无依赖冲突；12 项全部按注册表/配置开关扩展（演进原则 3），零回归前置。

### 11.6 版本生命周期与高可用（2026-09-24 新增）

> 用户方向：sclient 自更新 + sproxy 优雅重启 + 多副本不中断。基于 go-releaser 发布产物
> （sproxy_<ver>_<GOOS>_<GOARCH>.tar.gz + checksums.txt）与 Helm chart 现状（replicaCount:1
> 无 strategy/PDB）规划。

| # | 里程碑 | 内容 | 现状证据 | 状态 |
|---|--------|------|----------|------|
| 1 | **P1：sclient upgrade 自更新** | `sclient upgrade [--check] [--to <ver>] [--force]`：GitHub Releases API（buildinfo.ReleaseURL 已注入 `https://github.com/cocomhub/sproxy/releases`）→ 按 `runtime.GOOS/GOARCH` 匹配归档（`sproxy_<ver>_<GOOS>_<GOARCH>.tar.gz/.zip`）→ 解包取 sclient 二进制 → `checksums.txt` SHA-256 校验 → 原子替换（临时文件 + os.Rename；Windows 两段式：先退出自身再替换）→ 提示重启 | **已落地**（`pkg/selfupdate` + `sclient upgrade`，见 [cli.md](./cli.md#upgrade)）：GitHub API 单次 + CDN 直链 + SHA-256 fail-closed + 原子替换 + Windows 两段式兜底 | 已落地 |
| 2 | **P1：sproxy 优雅重启** | `kill -USR2`（新增信号）→ 新进程接管监听（SO_REUSEPORT 或新端口 + /readyz 健康检查交接）→ 旧进程 drain（复用 handleSignalShutdown 优雅关闭：cancel → s.Shutdown 等存量请求完成）→ 退出 | root.go runSignalHandler 现仅 SIGHUP（软配置）/SIGTERM（停）；无重启语义 | **已落地**（Unix-only）：USR2 → ExtraFiles 继承 listener → /readyz 就绪 → 复用 handleSignalShutdown drain；Windows 特性关闭零变化 | 已落地 |
| 3 | **P1：多副本不中断（Helm）** | deployment 补 `strategy: RollingUpdate {maxUnavailable: 0}`（先起新副本再缩旧）+ PodDisruptionBudget（minAvailable: 1）+ readinessProbe 改 `/readyz`（就绪才接流，避免滚动期间 503） | deployment.yaml 无 strategy 段；values replicaCount:1；探针用 /healthz | **已落地**：strategy RollingUpdate maxUnavailable:0/maxSurge:1 + pdb.yaml（minAvailable:1）+ readinessProbe /readyz | 已落地 |
| 4 | **P2：多副本写面限制声明** | 共享 PVC 多副本时写冲突——文档声明「多副本只读面 + 单写主」（写面仅副本 0），读面可水平扩展 | 无多副本写语义文档 | 缺 |

> 依赖：①② 独立可做；③ 依赖 readiness 探针（/readyz 已有）——探针路径需从 /healthz 改 /readyz；
> ④ 是③ 的配套约束文档。全部按零回归前置（默认单副本/无重启信号不启用）。

### 11.7 能力缺失全景（2026-09-24 逐面盘点）

> 对 sclient 命令面 / Web UI / 服务端 config / 协议面逐项 grep 验证的缺失清单。
> 已排除非缺项：presigned URL（backendPresignHandler 已有）、登录态持久化（login 已有）、
> Web UI 卷管理（createUserVolumeFormHtml 已有）、配置校验（config_validate.go 已有）。

| # | 里程碑 | 内容 | 源码证据 | 状态 |
|---|--------|------|----------|------|
| 1 | **P2：sclient du/df 空间统计** | 服务端 /api/stats 已有 DiskUsage（stats.go:35）——补 `sclient du [path]` / `df` CLI 封装（按目录递归大小 + 卷水位） | **已落地**：`GET /api/du`（pkg/server/du.go 递归统计 dirs/files/size，ACL 经 locateForRead 收口）+ `sclient du [path]` / `df`（df 复用 /api/stats 卷/磁盘水位） | 已落地 |
| 2 | **P2：sclient trash 命令** | 服务端 /api/trash 已有（列表/恢复/清空）——补 `sclient trash [list\|restore\|empty]` CLI | 服务端 handlers 有 listTrash/restoreTrash/emptyTrash；sclient 无 | 缺 |
| 3 | **P2：sclient quota 查看** | 服务端 /api/stats quota 段已有（quotaStatusOf）——补 `sclient quota` 展示本 owner 水位 | stats.go quotaStatusOf；sclient 无 | 缺 |
| 4 | **P2：卷操作 CLI** | 服务端 mirror/rebalance 已有（mirrorVolume/rebalanceVolumeHandler）——补 `sclient volume mirror\|rebalance` | **已落地**：`sclient volume copy/move/rebalance`（FileClient.CopyVolume/MoveVolume/RebalanceVolume，POST /api/volumes/{copy,move,rebalance}） | 已落地 |
| 5 | **P2：backup/export CLI** | 11.3 卷导出规划配套——`sclient backup <vol> <dest>`（导出到本地/远端） | 卷导出未做 | 缺 |
| 6 | **P1：OIDC/LDAP 外部认证** | 现仅本地凭据/Vault/TOTP——补 OIDC（Authorization Code + PKCE）/ LDAP 绑定（企业场景 SSO） | **已落地**：pkg/authn 共享契约（Authenticator/Principal/ExternalAuthHandler）+ ext/oidcldap 独立 module（OIDC jwks RS256 验签 + LDAP 绑定）+ SessionManager 会话 + 宿主注入 ExternalAuthHandlers（未配置不启用零回归） | 已落地 |
| 7 | **P3：通知 RSS/Atom 订阅** | 通知中心无订阅源——补 `/api/notify/feed`（最近通知 RSS/Atom，无需认证可配 token） | notify.go 无 feed | 缺 |
| 8 | **P3：WebSocket 服务端推送** | SSE（/api/events）已覆盖实时刷新——WS 推送低优先级（SSE 已够），记录不排期 | 仅传输层 WS | 排除（低价值） |
| 9 | **P2：卷级数据保留策略** | 桶/卷级 retention 统一策略（对齐 audit TTL/分享 TTL/版本 retention）——`volumes[].retention` | **已落地**：`volumes[].retention{version_ttl, share_ttl, audit_ttl, gc_interval}`（pkg/server/volume_retention.go，版本/分享/审计三维按卷清理 + 统一调度器周期 GC；审计仅默认卷单点权威；全零关闭零回归） | 已落地 |
| 10 | **P3：迁移向导** | 单机→多卷→联邦的自动化迁移脚本/向导（复用卷导出导入 + 镜像） | 无 migrate 工具 | 缺 |

> 优先级：6（企业 SSO 高价值）> 1-4（CLI 封装低成本，服务端能力已有）> 5/9（数据治理）> 7/10（生态/工具）。
> 8 明确排除（SSE 已满足实时推送，WS 推送无增量价值）。

### 11.8 客户端与 WebUI 缺口（2026-09-24 交叉比对）

> 服务端 95 路由（routes.go）vs WebUI 调用面 + sclient 40+ 命令 vs 服务端 API 的交叉比对结果。
> 已排除非缺：share list/revoke CLI（有）、mesh acl CLI（meshACLLines）、分享管理 UI（share modal）、
> 云端下载/同步/归档 UI（transfer page）。

#### sclient 缺口

| # | 命令 | 内容 | 源码证据 | 状态 |
|---|------|------|----------|------|
| A1 | `du/df` | 服务端 /api/stats 有 DiskUsage（stats.go:35）——CLI 按目录递归大小 + 卷水位 | **已落地**：`sclient du [path]`（GET /api/du 递归统计）+ `sclient df`（/api/stats 卷/磁盘水位） | 已落地 |
| A2 | `trash` | 服务端 /api/trash 有（list/restore/empty）——CLI 封装 | sclient 无 trash | 缺 |
| A3 | `quota` | 服务端 /api/stats quota 段有（quotaStatusOf）——CLI 展示本 owner 水位 | sclient 无 quota | 缺 |
| A4 | `volume copy/move/rebalance` | 服务端 POST /api/volumes/{copy,move,rebalance} 有——volume 命令仅 create/list/delete | **已落地**：`sclient volume copy/move/rebalance`（--from-volume/--to-volume/--max-bytes） | 已落地 |
| A5 | `upgrade` | 11.6 已规划（自更新） | **已落地**：`sclient upgrade [--check] [--to <ver>] [--force]`（pkg/selfupdate） | 已落地 |
| A6 | `backup/export` | 11.3 配套（卷导出） | 服务端 `GET /api/volumes/export` + `POST /api/volumes/import` 已落地（volume_export.go）；sclient 封装待后续片 | 服务端已落地 |
| A7 | `sync conflicts resolve` | 服务端 POST /api/sync/conflicts/{id}/resolve 有——CLI 无冲突解决 | **已落地**：`sync conflicts list` + `resolve <id> --strategy ours|theirs|manual` | 已落地 |

#### WebUI 缺口

| # | 功能 | 内容 | 源码证据 | 状态 |
|---|------|------|----------|------|
| B1 | 卷操作按钮 | volumes tab 仅展示——补 copy/move/rebalance 操作按钮 | app.js showVolumes 无操作 | 缺 |
| B2 | 凭据管理 UI | /api/credentials CRUD 无 UI——补凭据管理面板（admin） | app.js 无 credentials 调用 | 缺 |
| B3 | 同步冲突解决 UI | /api/sync/conflicts 无 UI——补冲突列表 + resolve 按钮 | app.js 无 conflicts 调用 | 缺 |
| B4 | 图片预览 | 现仅文本 previewText——补图片缩略图/预览（复用 transform thumb） | previewText 仅文本 | 缺 |
| B5 | 审计导出按钮 | /api/audit/export 无 UI 按钮——补导出链接 | audit tab 无 export | 缺 |
| B6 | 通知测试按钮 | /api/notify/test 无 UI——补测试按钮（管理动作） | notify tab 无 test | 缺 |
| B7 | Hub 联邦视图 | /api/hub/federation/* 无 UI——补联邦节点/服务视图 | hub tab 无 federation | 缺 |
| B8 | Mesh 状态视图 | /api/mesh/status 无 UI（mesh acl CLI 有）——补 mesh 状态面板 | app.js 无 mesh | 缺 |

> 优先级：B2（凭据管理，安全操作面）> A2/A3（trash/quota CLI，低成本）> B3/B1（操作面）
> > A1/A4（CLI 封装）> B4-B8（UI 增量）。WebUI 改动按硬规则带 node 单测 + Playwright e2e。

### 11.9 AI 接入规划（2026-09-24）

> 项目现无任何 AI 面（grep openai/anthropic/llm 无命中）。基于现有能力（95 API 路由/S3+WebDAV
> 协议端点/SSE 事件流/内容索引/通知中心/审计）设计三层 AI 接入。

#### 第一层：AI 客户端接入 sproxy（消费方视角，生态标准优先）

| # | 能力 | 内容 | 复用基础 | 优先级 |
|---|------|------|----------|--------|
| 1 | **MCP server** | `sproxy-mcp`：文件读写/搜索/分享/同步/通知暴露为 MCP 工具（read_file/write_file/search/stat/share/sync_status/notify_send）；stdio（本地 AI CLI）+ SSE（远程 AI 服务）；Bearer 认证 | 95 路由 HTTP API + SproxySig/Bearer | P0 |
| 2 | **S3/WebDAV 直连文档化** | LangChain S3Loader/WebDAV loader 直接读语料——写接入文档（endpoint/凭据/示例） | 已有 /s3/ + /dav/ 端点 | P1 |
| 3 | **事件流 AI 流水线** | AI agent 经 /api/events SSE 感知文件变更（新增→触发处理） | 已有 /api/events SSE | P3 |

#### 第二层：sproxy 提供 AI 能力（供给方）

| # | 能力 | 内容 | 复用基础 | 优先级 |
|---|------|------|----------|--------|
| 4 | **向量索引 + 语义搜索** | 内容索引升级 embedding（外部 embedding API/本地模型）——`/api/search/semantic?q=` 语义相关文件；索引 `meta/vector/<owner>.json` 增量 upsert | search index（#559 内容索引） | P2 |
| 5 | **AI 文件洞察** | `/api/ai/summarize?filename=`（文本摘要）+ `/api/ai/tag`（自动打标）；经 LLM 网关（OpenAI/Anthropic 兼容，配置 key）；无 key 501 fail-closed | LLM 网关新组件 | P2 |
| 6 | **智能运维助手** | AlertEngine 通知文本经 LLM 生成根因建议（磁盘水位/卷 degraded/同步失败的原因分析） | AlertEngine（7.3 已落地） | P1 |
| 6 | **智能运维助手（已落地 2026-09-24）** | `notify.ai_advisor`（enabled 默认 false 零回归 + provider/base_url/api_key_ref/model/timeout）；无 key/网关失败 → 固定模板 fail-closed；llmgate 网关新组件（OpenAI 兼容 /chat/completions） | pkg/llmgate + pkg/server AIAdvisor | P1 |

#### 第三层：治理/安全

| # | 能力 | 内容 | 优先级 |
|---|------|------|--------|
| 7 | AI 使用配额/审计 | /api/ai/* 调用记账（审计事件 + 配额扣减） | P1（随 4/5 启用） |
| 8 | 数据隐私 | 向量/摘要落盘加密（复用 at-rest 加密卷） | P1（随 4/5 启用） |
| 9 | 可选开关 | `ai.enabled` 默认 false 零回归 + 生效可观测（禁静默降级——演进原则 2） | P0（随 1 启用） |

> **优先级结论**：P0 MCP server（生态标准，消费方最先受益）→ P1 智能运维（运维场景最高价值，
> AlertEngine 已有输入）+ S3/WebDAV 文档 → P2 语义搜索/文件洞察 → P3 事件流流水线。
> 约束：LLM 网关/向量索引均需显式配置（ai.enabled + provider key），无 key fail-closed 不降级。

### 11.10 发展方向遗漏补全（2026-09-24 盲区盘点）

> 对既有 11.1-11.9 之外的盲区系统盘点（i18n/调度/可观测/协议/生态/合规六面 grep 验证）。
> 已排除低价值：去中心化存储（定位不符）、白标（SaaS 多租户才需）、合规认证（超出代码范围）、
> FTP/SMB/NFS 服务端（WebDAV/S3 已覆盖主流）。

#### 高价值（匹配项目定位）

| # | 方向 | 内容 | 源码证据 | 优先级 |
|---|------|------|----------|--------|
| 1 | **WebUI i18n 多语言** | index.html lang=zh-CN 硬编码 + app.js 全部中文文案——补 i18n 框架（en/zh 双语言，语言切换持久化） | **已落地**：i18n.js 框架（zh/en 词条表 + t()/fmt() + lang() 解析 localStorage>navigator>zh + setLang 持久化 + applyStaticI18n 静态替换）+ 语言切换按钮 + Playwright e2e。残余：app.js 全部动态文案迁移（当前覆盖关键按钮/导航） | P1 |
| 2 | **通用任务调度器** | version/trash/share/upload 5 个 GC 循环各自 ticker——补统一调度器（注册周期任务 + 维护窗口） | **已落地**：pkg/server/scheduler.go 统一 Scheduler（注册/停止/单飞防重入/panic 恢复/维护窗口），upload/version/trash/share 四循环收敛（间隔 1:1 零回归）；维护窗口配置 scheduler.maintenance_window（HH:MM，默认关） | P1 |
| 3 | **SLO/错误预算** | 40+ 指标已有但无 p99/apdex/error_budget——补延迟分位数指标 + SLO 规则（联动 AlertEngine） | **已落地**：手写无锁桶直方图（Prometheus 累计 le 桶）+ Apdex 三档 + /metrics 暴露（request_duration_seconds_bucket/sum/count + apdex 分数与三档）；middleware 时长捕获。残余：错误预算规则联动 AlertEngine | P1 |
| 4 | **文件标签系统** | content index 有 contentTokens 无 tags——补标签打标（POST /api/tags）+ 搜索按标签 | **已落地**：POST /api/tags 打标（查询参数 + JSON body 批量）+ search?tag= 精确过滤（与 q AND 组合）；tagsStore 持久化（meta/tags/<sha256(rel)>.json）+ 索引快照带 tags + 失效重建从 store 合并；非法/超量整批 400、文件不存在 404 | P2 |
| 5 | **通知出站签名** | webhook 渠道出站无 HMAC 签名——补签名头（防伪造回调/篡改） | **已落地**：webhook 渠道出站 HMAC-SHA256 签名头（`X-Sproxy-Signature: sha256=<ts>.<hex>` + 时间戳头，可配 secret/自定义头名/时钟漂移；secret 空 = 不签名零回归；接收侧验签见 `VerifyWebhookSignature`） | P2 |
| 6 | **sclient 多语言输出** | CLI 输出中文硬编码（output.go Text/JSON）——补文案 i18n（LC_ALL 感知） | output.go 中文硬编码 | P3 |

#### 中价值

| # | 方向 | 内容 | 优先级 |
|---|------|------|--------|
| 7 | IaC provider | Terraform/Ansible 管理部署（配合 Helm） | P2 |
| 8 | 混沌测试 | HA 场景故障注入（kill -9/网络分区/延迟注入） | P2 |
| 9 | 压缩算法扩展 | zstd/brotli 高压缩比（存档/传输） | P2 |
| 10 | 计量报告 | quota 已有补 usage report 导出（per-owner 周期用量） | P2 |
| 11 | 限流维度扩展 | per-endpoint/全局并发上限（现 per-IP/per-owner） | P2 |

> 优先级：H1-H3（基础面，多语言/调度/可观测）> H4-H5（功能增量）> M1-M5 > H6。
> 与 11.1-11.9 无冲突；全部零回归前置 + 注册表/配置开关扩展（演进原则 2/3）。

### 11.11 多节点共享存储与集群化（2026-09-24 可行性分析）

> 用户方向：外部卷挂载 + 本地卷 → 多节点共享存储/状态管理/扩缩容/状态同步/集群化。
> **结论：数据面可行（共享外部卷现成），控制面不可行（状态本地化）——推荐「共享外部卷 + 选主 + 只读副本」方案 A，不推荐 etcd/raft 方案 B（与轻量架构冲突）。**

#### 现状能力盘点

| 面 | 现状 | 集群化支撑 |
|----|------|-----------|
| 数据面共享 | 外部卷挂载（S3/WebDAV/baidupcs）+ 联邦卷（federated 经 mesh 隧道读写远端） | ✅ 共享外部卷现成 |
| 数据复制 | mirror_targets（N 副本）+ rebalance（冷热分层迁移） | ✅ 数据复制语义已有 |
| 控制面交换 | FederationClient（跨 hub 节点/服务交换） | ✅ 控制面基础已有 |
| 只读挂载 | federated.FS 只读形态（未注入 Writer 恒 ErrReadOnly） | ✅ 读副本形态现成 |
| 状态落盘 | 凭据/checksum/dedup/索引/分享均落**本地卷 meta**（credentials.json / dedup.json / index/<owner>.json / share/<token>.json） | ❌ 各节点独立不一致 |
| 写互斥 | acquireFileLock 本地文件锁 | ❌ 跨节点无互斥 |
| 选主 | 无 leader/raft（grep 空） | ❌ 多写冲突 |
| 配额 | 内存态 Scope（owner_quotas + reconcile） | ❌ 每节点独立 |
| 搜索索引 | 内存态 + 快照（本地 meta/index） | ❌ 多节点结果不一致 |

#### 方案 A：共享外部卷 + 选主 + 只读副本（推荐）

| # | 里程碑 | 内容 | 优先级 |
|---|--------|------|--------|
| 1 | **P1：Leader 选举** | 基于外部卷租约（S3 lease 文件 / 共享卷锁）/ 集中 DB 租约——写面节点唯一（主节点）；副本心跳续租 | P1 |
| 2 | **P1：状态上移** | 凭据/配额/索引/分享 meta → 共享外部卷（S3 等）或集中 DB（SQLite 单文件——轻量适配）——主节点写、副本读 | P1 |
| 3 | **P1：只读副本接入** | 非主节点只读挂载（复用 federated 只读形态）——读面水平扩展 | P1 |
| 4 | **P2：索引一致性** | 主节点构建索引 → 快照共享卷 → 副本加载；或变更经事件流广播 → 各节点失效重载 | P2 |
| 5 | **P2：扩缩容管理** | 扩容 = 加节点挂同一外部卷（只读）；缩容 = 节点下线 + 选主切换；写面仅主节点（冲突消除） | P2 |
| 6 | **P2：写面协调** | 主节点写 → 变更事件（/api/events 已有）→ 副本索引失效 + 缓存失效 | P2 |

#### 方案 B：etcd/raft 完全分布式（不推荐）

| 项 | 说明 |
|----|------|
| 内容 | etcd/raft 复制状态机（选主 + 状态共识） |
| 不推荐理由 | 与轻量单文件架构冲突；引入外部依赖（etcd）；运维复杂度陡增——超出「文件服务 + 隧道」定位 |

> **扩缩容形态**：扩容 = 加只读副本节点（读面水平扩展）；缩容 = 节点下线 + 选主切换；
> **状态同步形态**：主节点写共享 meta → 副本近实时读（事件流失效重载）。
> 与 11.6 多副本不中断（Helm RollingUpdate + PDB）配套：方案 A 落地后，多副本从「只读面」升级为「共享存储 + 选主」完整形态。

### 11.12 状态抽象接口（StateStore 插件化，2026-09-24）

> 用户方向：状态抽象接口——现仅本地存储，后续插件扩展 mongo/raft 等。
> **现状盘点**：仅凭据有 `CredentialStorer` 接口（Vault 已插件化）；checksum/dedup/share/index/
> audit/quota 均为具体实现（本地 JSON/内存）——需统一抽象。

#### 核心接口

```go
// StateStore 是统一状态存储抽象（本地 JSON 为默认实现，插件扩展 mongo/raft 等）。
type StateStore interface {
    Get(ctx context.Context, key string) ([]byte, error)
    Put(ctx context.Context, key string, data []byte) error      // 原子写（tmp+rename 语义）
    Delete(ctx context.Context, key string) error
    List(ctx context.Context, prefix string) ([]string, error)
    CAS(ctx context.Context, key string, old, new []byte) error  // 配额/dedup 引用计数/凭据并发
}

// AppendStore 审计/事件特化（append-only，只需顺序追加）。
type AppendStore interface {
    Append(ctx context.Context, key string, data []byte) error
}

// WatchStore 索引失效广播/选主心跳特化（变更订阅）。
type WatchStore interface {
    Watch(ctx context.Context, prefix string) (<-chan Change, error)
}

// LeaderElector 选主（11.11 方案 A 配套：写面节点唯一）。
type LeaderElector interface {
    TryAcquire(ctx context.Context, leaseID string, ttl time.Duration) (bool, error)
    Renew(ctx context.Context, leaseID string) error
    Release(ctx context.Context, leaseID string) error
}
```

#### 实现层次（注册表扩展，演进原则 3）

| 实现 | 说明 | 现状 |
|------|------|------|
| LocalStateStore | 现有 JSON 文件（os.Root 相对 + 原子写 tmp+rename）——默认零回归 | 现成（改造封装） |
| MongoStateStore | MongoDB 文档集合（_id=key，CAS 用 findAndModify）——插件注册 | 新 |
| RaftStateStore | etcd/raft 复制状态机——集群化（11.11 方案 B 轻量版） | 新 |
| Vault 已有 | 凭据专用（CredentialStorer）——不并入，保持专用 | 已有 |

#### 状态适配（key 设计）

| 状态 | 现本地落盘 | StateStore key |
|------|-----------|----------------|
| 凭据 | anonymous/meta/credentials.json | credential/anonymous |
| checksum | meta/checksum* | checksum/<owner>/<rel> |
| dedup | meta/dedup.json | dedup/<owner> |
| 分享 | meta/share/<token>.json | share/<token> |
| 索引 | meta/index/<owner>.json | index/<owner> |
| 审计 | audit.log（append） | audit/<seq>（AppendStore） |
| 配额 | 内存态 + reconcile | quota/<owner>（可持久化） |

#### 装配与门禁

| 项 | 设计 |
|----|------|
| 注册表 | `RegisterStateStore(name, factory)` / `NewStateStore(name, cfg)`（仿 RegisterBackend）✅ F1 |
| 配置 | `state_store: { type: local\|mongo\|raft, ... }`（默认 local 零回归）｜F1 接口预留，装配接线待 F3 |
| 门禁 | `internal/archcheck/state_store_gate_test.go`（R21）：非测试源码禁直接 `os.WriteFile(meta/*)`（强制走 StateStore 接口）✅ F1 |
| 测试 | LocalStateStore 往返 + CAS 冲突 + 并发 CAS + Watch + 注册表 + key 校验 + 凭据旧格式兼容（credential 适配片）✅ F1 |

> **状态**：**F1 已落地**（2026-09-24）：`pkg/state` 接口（StateStore/AppendStore/WatchStore/LeaderElector）+ LocalStateStore（原子写 tmp+rename + CAS + Watch 轮询）+ LocalAppendStore + 注册表（RegisterStateStore/NewStateStore/StateStoreTypes）+ key 段校验（与 storage.ValidSegmentName 同语义）+ R21 门禁（豁免清单含全部迁移期 Store，F2 逐片删除）。F2 各状态适配（凭据→checksum→dedup→share→index 逐个迁移）；F3 Mongo 实现 + CAS 事务；F4 LeaderElector + 选主（11.11 配套）；F5 Raft 实现（集群化，长期）。
> **与 11.11 关系**：StateStore 是集群化的**状态层基础**——状态上移（11.11-2）即把各状态从 LocalStateStore 切到 Mongo/Raft；LeaderElector 是选主（11.11-1）的接口。

### 11.13 优先级矩阵（2026-09-24 按价值×投入重排）

> 对 11.1-11.12 全部 P 项按「功能价值 × 投入产出比」重排优先级。
> 矩阵轴：**价值**（用户可感知/正确性/架构基础）× **投入**（人日估算，按片划分）。

| 档位 | 项 | 价值 | 投入 | 依据 |
|------|-----|------|------|------|
| **S1（正确性，先修）** | 11.2-① S3 complete ETag 校验 | 正确性 | 0.5 人日 | 批次11 审查 P2，协议完整性 |
| | 11.2-② S3 complete 配额记账 | 正确性 | 0.5 人日 | 防分块绕过配额 |
| **S2（高价值低投入，紧接）** | 11.5-④ S3 ListBuckets | 生态兼容 | 0.5 人日 | aws s3 ls 直接可用 |
| | 11.8-A2/A3 trash/quota CLI | 可用性 | 1 人日 | 服务端已有，纯封装 |
| | 11.7-②③ 冲突解决 CLI/UI | 可用性 | 1 人日 | 服务端已有 API |
| | 11.10-H3 SLO 指标 | 可观测 | 1 人日 | p99/apdex 入 metrics |
| | 11.6-① sclient upgrade | 运维 | 2 人日 | 自更新闭环 |
| **S3（高价值中投入，架构基础）** | 11.12 StateStore F1-F2 | 架构 | 5 人日 | 集群化状态基础 |
| | 11.11 方案 A 选主+只读副本 | 架构 | 5 人日 | 集群化写面唯一 |
| | 11.10-H2 通用任务调度器 | 可维护 | 3 人日 | 5 个 GC 统一 |
| | 11.9-⑥ 智能运维 LLM | 运维 | 3 人日 | AlertEngine 根因建议 |
| **S4（中价值，可并行）** | 11.5-① RBAC 细分 | 安全 | 2 人日 | 只读/运维角色 |
| | 11.5-② IP 白名单 | 安全 | 2 人日 | 部署形态门 |
| | 11.7-⑥ OIDC/LDAP | 企业 | 5 人日 | SSO |
| | 11.10-H1 WebUI i18n | 基础 | 3 人日 | 多语言 |
| **S5（长尾，按需）** | 11.9-① MCP server | 生态 | 5 人日 | 依赖 AI 客户端生态成熟 |
| | 11.12-F5 Raft | 架构 | 10+ 人日 | 集群化深水区 |
| | 11.10-M 中价值项 | 增量 | 各 2-3 人日 | IaC/混沌/zstd/计量 |

> **执行顺序**：S1（正确性，立即）→ S2（高价值低投入，1-2 天）→ S3（架构基础，StateStore 先行
> ——11.12 是 11.11 的前置依赖）→ S4（安全/企业，并行）→ S5（长尾按需）。
> **依赖链**：11.12 StateStore → 11.11 选主（LeaderElector 同包）→ 11.6 多副本升级（共享存储形态）。
> **投入估算**：S1-S2 合计 ~6 人日（一周内可交付），S3 合计 ~16 人日（两周），S4 合计 ~12 人日（并行三周）。

### 11.14 设计批（S1+S2+S3 共 10 项，2026-09-24 子代理头脑风暴完成）

> 对 11.13 优先级矩阵的 S1/S2/S3 档逐项完成设计（10 份设计文档，子代理产出，
> 存 `.worktrees/docs/design-batch/docs/designs/`）。每份含背景/组件/数据流/错误处理/
> 测试+变异点/片划分/零回归。设计间无冲突（各自独立文件/分支）。

| # | 设计文档 | 覆盖 roadmap 项 | 核心决策 |
|---|----------|----------------|----------|
| 1 | 2026-09-24-s3-complete-etag.md | 11.2-① | meta key 409 / ETag==md5 400 / PartNumber 范围+重复 / body 413 / 失败清理半截目标 |
| 2 | 2026-09-24-s3-complete-quota.md | 11.2-② | 双 TryReserve（Scope+卷池）超限 507 / Commit Adjust 差分 / 失败双 Release |
| 3 | 2026-09-24-s3-listbuckets.md | 11.5-④ | 复用 sigV4Verify + ACL 视图枚举卷即桶；外部卷不列；xmlEscapeText 防注入 |
| 4 | 2026-09-24-sclient-trash.md | 11.8-A2 | SDK 三方法 + CLI 三子命令 + OutputFormatter 双实现扩展；restore 用 trash_rel 令牌 |
| 5 | 2026-09-24-sclient-quota.md | 11.8-A3 | pkg/client StatsResponse 补 Quota 字段 + CLI 展示水位（服务端零改动） |
| 6 | 2026-09-24-sync-conflicts-cli.md | 11.8-A7 | conflicts list/resolve CLI（服务端 sync_handler.go 三端点已存在） |
| 7 | 2026-09-24-slo-metrics.md | 11.10-H3 | 手写无锁桶直方图 + Apdex 三档（metricsMiddleware 时长捕获） |
| 8 | 2026-09-24-sclient-upgrade.md | 11.6-① | pkg/selfupdate 纯函数 + GitHub API 单次 + CDN 直链 + SHA-256 fail-closed + Windows 两段式 |
| 9 | 2026-09-24-statestore.md | 11.12 | pkg/state 接口 + Local 默认零回归 + Mongo/Raft 插件 + 迁移矩阵 8 Store + R21/R22 门禁 |
| 10 | 2026-09-24-leader-elector.md | 11.11-① | **已落地**（#578）：pkg/leader LeaderElector 接口 + Local flock 恒主零回归（Unix flock / Windows LockFileEx 双平台互斥）+ WriteGuard 写面门（ErrNotLeader）+ RenewLoop 续租/退避补位 + R23 flock 门禁；Mongo TTL 租约（F2）与写面装配（F3）后续片 |

**依赖链**：StateStore（11.12）→ LeaderElector（11.11）→ 多副本升级（11.6）；S3 配额片依赖 ETag 片完成态（复用 routeUpload 语义）。

**待人工决策**（设计完成汇总后统一确认）：
1. mongo-driver 依赖放行（官方纯 Go，符合依赖策略）
2. 配额不迁移 StateStore（高频内存账本，靠 LeaderElector 保一致）
3. StateStore 落盘路径 state/ 新路径 + 读旧 meta 回退（双读单写）
4. S3 complete 响应加复合 ETag（S3 分块标准形态，可选加法）
5. trash restore 用 trash_rel 令牌（非原名，歧义不可消解）
6. Windows LeaderElector 回落恒主 + Warn（可观测禁静默）

> **人工决策（2026-09-24 已确认）**：
> 1. mongo-driver 放行，但**用 ext 依赖隔离**（独立 module，cmd/sproxy 允许引入；领域包不引）
> 2. 配额**不迁移** StateStore（高频内存账本，LeaderElector 保写面唯一）
> 3. StateStore 落盘 **state/ 新路径** + 读旧 meta 回退（双读单写）
> 4. S3 complete 响应**加复合 ETag**（`hex(md5(concat(md5(p1)...)))-N`）
> 5. trash restore 用 **trash_rel 令牌**（list 输出可操作令牌，非原名）
> 6. LeaderElector Windows 用 **LockFileEx** 实现（非回落恒主；Unix flock / Windows LockFileEx 双平台）

> **补设计批（2026-09-24 完成）**：对 11.13 矩阵剩余 7 项补齐设计（S3 剩余 2 + S4 全部 4 + S5 MCP），
> 3 路子代理产出 7 份文档。至此 **11.13 矩阵全部 17 项均有设计文件依据**（存 `.worktrees/docs/design-batch/docs/designs/`）。
> **实施（2026-09-24）**：11.9-⑥ 智能运维 LLM 已落地（PR #572，`notify.ai_advisor` + pkg/llmgate + AlertEngine 根因建议，见上表 11.9 第二层）。

| 补设计文档 | 覆盖项 | 核心决策 |
|-----------|--------|----------|
| 2026-09-24-task-scheduler.md | 11.10-H2（S3） | 4 个 GC 循环收敛统一 Scheduler（注册/停止/panic 恢复/单飞防重入/维护窗口），间隔 1:1 零回归 |
| 2026-09-24-ai-advisor.md | 11.9-⑥（S3） | pkg/llmgate（OpenAI 兼容）+ AlertEngine 同步拼「AI 建议」；无 key 回退模板 fail-closed；api_key_ref 引环境变量 |
| 2026-09-24-rbac-roles.md | 11.5-①（S4） | RoleReader 只读档位 + isFileGroupedRoute 只读/写子组拆分（写组 requireRole(user) 不动） |
| 2026-09-24-ip-whitelist.md | 11.5-②（S4） | auth.allow_ips（认证前 403）+ auth.trusted_proxy（仅信任代理解析 XFF）；双配置空零回归 |
| 2026-09-24-oidc-ldap.md | 11.7-⑥（S4） | ext 独立 module + Authenticator 宿主注入（DEC-C 复用，pkg/server 零新依赖）；未配置不启用 |
| 2026-09-24-webui-i18n.md | 11.10-H1（S4） | 零框架 web/static/i18n.js（zh/en 字典 + t() + data-i18n）；默认 zh 零回归 + node 单测 + Playwright e2e |
| 2026-09-24-mcp-server.md | 11.9-①（S5） | cmd/sproxy-mcp 独立二进制（手写 JSON-RPC 2.0 + stdio 传输 + Bearer/SproxySig），FileClient 薄封装，服务端零改动 |

**设计批总计**：第一批 10 份 + 补设计 7 份 = **17 份**，覆盖 11.13 矩阵全部 17 项（S1×2/S2×5/S3×4/S4×4/S5×2）。
**待确认（补设计引出）**：① ai_advisor api_key_ref 是否 SIGHUP 热重载（默认重启生效）；② OIDC/LDAP ext 依赖版本评审（x/oauth2 + go-ldap）。

> **全量设计决策（2026-09-24 全部按推荐确认）**：
> 1. 一次性分享 token 副本节点 **503**（防计数超发，不转发）
> 2. EventBus 事件持久化 **P2 预留**（事件只决定「何时检查」，一致性靠 StateStore 快照）
> 3. `--route` 与 `--exit-only` **互斥校验** + weight 语法 `node:weight`
> 4. /api/volumes/{copy,move,rebalance} 实施时全局定位（非决策项）
> 5. 卷导出超大文件 **文档限制（P3）+ 建议分块**
> 6. storage 加密卷接口实施前核 OpenDecrypted（标记待核对）
> 7. **klauspost/compress 放行**（纯 Go 社区活跃，压缩 zstd/brotli）
> 8. 磁盘水位指标实施时核现有注册表（标记待核对）
> 9. AI 向量 **单 owner 上限可配 + 配额按日重置**
> 10. fd 继承优雅重启 **Unix-only 声明**（Windows 另记）
> 11. ai_advisor api_key_ref **重启生效**（SIGHUP 不热载）
> 12. **OIDC/LDAP ext 依赖放行**（x/oauth2 + go-ldap，ext 隔离）

**设计覆盖**：roadmap 82 功能项全部有设计文件依据（62 份设计文档存 `.worktrees/docs/design-batch/docs/designs/`，17 首批 + 45 新增四批）。
**本 agent 角色**：仅负责设计方案；实现由其他 agent 分派（按 11.13 优先级 S1→S2→S3 顺序）。
