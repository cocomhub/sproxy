<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格。
版本号遵循 [SemVer 2.0.0](https://semver.org/lang/zh-CN/)。

> 变更类型：`Added` 新增 / `Changed` 变更 / `Deprecated` 弃用 / `Removed` 移除 /
> `Fixed` 修复 / `Security` 安全。0.1.0–0.11.0 的版本 tag 按提交时间线回溯建立，
> 每个版本对应的提交范围见文末链接。

## [0.11.1](https://github.com/cocomhub/sproxy/compare/v0.11.0...v0.11.1) (2026-09-15)


### Fixed

* **e2e:** xfer_tls 双端口有界重试（F3 余量收口） ([fc94bda](https://github.com/cocomhub/sproxy/commit/fc94bdaf955ed860e4ffd0bb394a0ec5e4c167d0))
* **files:** rename 竞争窗口收口与 AtomicRename 破坏性删除目标 ([#259](https://github.com/cocomhub/sproxy/issues/259)) ([0fe5d41](https://github.com/cocomhub/sproxy/commit/0fe5d41498f186ee1d940b08f54ca4143cd7374d))
* **lint:** R12 门禁里 err 影子声明（govet shadow） ([#255](https://github.com/cocomhub/sproxy/issues/255)) ([afc0c1c](https://github.com/cocomhub/sproxy/commit/afc0c1cca932cdd0fbf653e3cb0ac545ecdee81c))
* **mux:** 核实消除 retransmitLoop 泄漏技术债（回归钉 + ticker）+ F3 端口重试样板 ([21d2a3f](https://github.com/cocomhub/sproxy/commit/21d2a3fd01ed3a67afcf2cb0076172aead2f3b09))
* **sclient:** --hub 派生 wss→https；删除 test-only 死代码并下沉条目解析 ([#261](https://github.com/cocomhub/sproxy/issues/261)) ([25a9c5e](https://github.com/cocomhub/sproxy/commit/25a9c5eeb589d756fef818ba3a92f6f071a043f1))
* **sclient:** preview 改用 SDK 下载，支持隧道模式与统一认证/传输 ([#257](https://github.com/cocomhub/sproxy/issues/257)) ([e1d1219](https://github.com/cocomhub/sproxy/commit/e1d12194377ebf94de6cce9017bd979ae87f923f))
* **sclient:** relay status/stats/remove-node 改走 SDK，并修正 stats 字段名 ([#258](https://github.com/cocomhub/sproxy/issues/258)) ([8e0bccb](https://github.com/cocomhub/sproxy/commit/8e0bccba044cbbb7d21d501a261f7ddda8cdd5b2))
* **server:** 跨节点 listener 在瞬时 Accept 错误后不再静默死亡 ([#251](https://github.com/cocomhub/sproxy/issues/251)) ([7687e6b](https://github.com/cocomhub/sproxy/commit/7687e6beed780c8de59a7e7bdc2cb584afb384d8))
* **test:** master 合并后首跑补救（隔离连接池/Share Expired 判定/TLS 最低版本语义）+ 新增并发注册门禁 R18 + CHANGELOG 全类型可见 ([#273](https://github.com/cocomhub/sproxy/issues/273)) ([89f83b7](https://github.com/cocomhub/sproxy/commit/89f83b70810cc930d9700e9892d7d341ec13efdd))


### Changed

* **agents:** 修复 AGENTS.md 遗留问题（编号错位/必检项过时/changelog 表述）+ 扩展 R9/R12 文档门禁 ([210fb60](https://github.com/cocomhub/sproxy/commit/210fb60efacdac7ffa791f9825b971f6d27faeee))
* **agents:** 推送规则放宽为 https/SSH 双通道（本机 SSH 已验证可用） ([5f06f1e](https://github.com/cocomhub/sproxy/commit/5f06f1e423ce2510c2fd12e7b93fda54e22f7b62))
* **agents:** 移除全部过时技术债清单（已逐条核实）+ fix(client): TunnelDo 失败且 mux 已终止时立即清缓存 ([395144f](https://github.com/cocomhub/sproxy/commit/395144ffee73095fdc43e319f3987f421a30cdc4))
* **baseline:** 开源库基线标准化——GoReleaser CI 修复 + 死代码清理 + 测试工具归位 + 文档收口 ([#249](https://github.com/cocomhub/sproxy/issues/249)) ([0a1d2f5](https://github.com/cocomhub/sproxy/commit/0a1d2f597659ab4bf8ebec8236b0379efbd15a09))
* **ci:** 修复 make notest 空转假门禁（两处缺陷）+ 补 3 处缺失测试 ([#271](https://github.com/cocomhub/sproxy/issues/271)) ([a8dcb0d](https://github.com/cocomhub/sproxy/commit/a8dcb0d4c8c92f859bf4aa2c1672d1f85ea8dea8))
* **cleanup:** 非测试 TODO 审计收敛（13 处，零行为变更） ([#260](https://github.com/cocomhub/sproxy/issues/260)) ([d8494c8](https://github.com/cocomhub/sproxy/commit/d8494c8ee782b42d7c3f4adea1da9fc8b7020069))
* **client:** 拆分 1896 行的 client.go 为 6 个同包文件（D3 第 2 处，零 API 变更） ([#267](https://github.com/cocomhub/sproxy/issues/267)) ([e5894b9](https://github.com/cocomhub/sproxy/commit/e5894b9340c9f94aaf6a79d00eb8e0d5b49b5abc))
* **cloud:** 拆分 2328 行的 manager.go 为 6 个同包文件（D3 第 1 处，零 API 变更） ([#266](https://github.com/cocomhub/sproxy/issues/266)) ([9ac5474](https://github.com/cocomhub/sproxy/commit/9ac547402daa0f5a9d9f049fc0e3b3378b432e74))
* **flake:** 测试固定等待再条件化（161→136）+ 收尾文档 ([#270](https://github.com/cocomhub/sproxy/issues/270)) ([0cabfae](https://github.com/cocomhub/sproxy/commit/0cabfae91c6f66cb6e79c4c77a5f8f4dc0ffb47e))
* **gates:** 覆盖率门禁去 bc（Windows 静默 PASS）+ 死代码失败门禁 + R11 去 git 依赖 ([#262](https://github.com/cocomhub/sproxy/issues/262)) ([445ddbd](https://github.com/cocomhub/sproxy/commit/445ddbda12f8290af02977d35969b16a0ed354df))
* **p2p:** 手工 SDP 信令下沉 pkg/tunnel/p2p（cmd 薄层 D1-c） ([#265](https://github.com/cocomhub/sproxy/issues/265)) ([291831b](https://github.com/cocomhub/sproxy/commit/291831babac230e0afe6a8242b564be8a3a138fc))
* **release:** CHANGELOG 改由 release-please 单一事实源（废止逐 commit 手改）+ 门禁 R12 ([#254](https://github.com/cocomhub/sproxy/issues/254)) ([0722ad4](https://github.com/cocomhub/sproxy/commit/0722ad45ce1513a03dc75e8426f8c9b11a17d08e))
* **release:** commit-msg 强制 scope 钩子 + release-please 机制文档 + 门禁钉 ([#280](https://github.com/cocomhub/sproxy/issues/280)) ([adf23eb](https://github.com/cocomhub/sproxy/commit/adf23eb2d2d28177d1181af23a23251958fa554a))
* **release:** 发布机制标准化——release-please 接入 + CHANGELOG 单源 + 嵌套模块 tag 脚本 ([#250](https://github.com/cocomhub/sproxy/issues/250)) ([8bd14d2](https://github.com/cocomhub/sproxy/commit/8bd14d2977f22bebde9c71d8da4688906a93e0d5))
* **release:** 固化发布流程踩坑 + R12 守 RELEASING.md ([#256](https://github.com/cocomhub/sproxy/issues/256)) ([bca7705](https://github.com/cocomhub/sproxy/commit/bca7705b92975660ba904f1e9edecf91465fbe4e))
* **repo:** 补齐 CONTRIBUTING/SECURITY 并清除 auth_token 术语残留与零引用导出 ([#264](https://github.com/cocomhub/sproxy/issues/264)) ([5f3b793](https://github.com/cocomhub/sproxy/commit/5f3b793bc3c5a5bb3237b9eaa12e02c9590a25f7))
* **server:** 拆分 1492 行的 config.go 为 4 个同包文件（D3 第 4 处/收官，零 API 变更） ([#269](https://github.com/cocomhub/sproxy/issues/269)) ([f729ab6](https://github.com/cocomhub/sproxy/commit/f729ab60dd04b3365269cac3bb95595e2188336c))
* **server:** 拆分 1547 行的 handlers.go 为 5 个同包文件（D3 第 3 处，零 API 变更） ([#268](https://github.com/cocomhub/sproxy/issues/268)) ([442336f](https://github.com/cocomhub/sproxy/commit/442336f01c3c46137886094961a6b35a20d856a7))
* **tests,docs:** 固定等待棘轮门禁 + WaitFor 助手 + sclient 全局选项文档补全 ([#263](https://github.com/cocomhub/sproxy/issues/263)) ([471b558](https://github.com/cocomhub/sproxy/commit/471b558e16bd4abc85938ad1b63a6bfb977ca345))
* **test:** 分组测试超时统一放宽到 60s（-race 低性能环境） ([#253](https://github.com/cocomhub/sproxy/issues/253)) ([8d05ebc](https://github.com/cocomhub/sproxy/commit/8d05ebcce47c8f41995474dae37d62ac3b9297c1))
* **test:** 消除固定等待并以条件等待/并行化治理 flake（136→38 处，e2e -race 墙钟 174s→58s，单元套件降至 42s） ([13c2852](https://github.com/cocomhub/sproxy/commit/13c2852a2961199183ae2fbba6c8f4ebd54b66c7))
* 全量刷新 md 反映最新现状（移除过期路由/配置/传输实现）+ 扩展文档漂移门禁 R9 + fix(build): bench 补 prepare 依赖 ([bb422a9](https://github.com/cocomhub/sproxy/commit/bb422a9e171e3add7de375a93adf7195ae64723d))

## [0.11.0] - 2026-09-14

跨节点访问面与文件服务域收口：跨节点只读/写访问面、remote 传输、`pkg/files` 域操作
API、WebRTC 直连与 mesh 可观测性。

### Added

- 跨节点卷访问：`remote` 传输原语 + B 侧只读 listener，A 侧客户端与 `sync.FS` 实现
  （`remote://` 目标）。
- 跨节点写面：B 侧写 listener（`remote_write` 配置）与写 handler（授权 + 直调域 API），
  A 侧 4 个写方法与 `volwrite` 服务名。
- 卷 ACL 增加 `scope` 轴（`read|write|rw`，缺省 `read`）与 `AuthorizeMeshWrite`；
  新增 `GET /api/mesh/acl` 跨节点授权只读视图（仅 owner 自身）。
- mesh 任务载体可见性：同步任务对外字段新增 `kind`/`transport`/`carriers`，
  新增 `GET /api/mesh/status` 与 `sclient mesh status --server`。
- WebRTC 直连：实例级 ICE 配置（`ICEOptions`）、远端 hub 拨号器（`remote.Dialer`）+
  可选回落选路、A 侧 mesh 客户端配置段（hub 可为远端）与启动期校验。
- 进程内 mesh node 角色（消除 B 侧独立 sidecar 进程）。
- 跨节点带标签 Prometheus 指标（载体 / 回落 / 写面拒绝）。
- 文件服务（`pkg/files`）域操作 API：`List/Search/StatPath/OpenPath`、`WriteFile`、
  `MakeDir/RemoveDir`、`RenameFile/DeleteFile`，批量族在域方法之上循环；新增 Option
  构造入口（唯一必需项编译期保证 + 最小默认能力 + 按需注入接口）。
- `HTTPError.Reason` 原因码与域级测试覆盖、校验和计算收敛为单一事实源。

### Changed

- `pkg/sync` 的 `HTTPTransport` 迁出（网络实现归位），载体模型 `Kind` 一次定清；
  `syncmgr`、`downloader`、`cloud` 提升为顶级包，`pkg/files` 接缝完成全功能迁移。
- 路径安全校验抽为顶级包 `pathguard`；校验和台账、容量核算、卷集合装配分别抽为
  `pkg/checksum`、`pkg/storage` 子包、`pkg/volume` 子包。
- 装配层迁移到 Option 构造，删除 `Deps`/`NewService` 兼容层；`internal/slogutil`
  统一 nil logger 归一化；`storage.Root.AtomicRename` 收敛原 `atomicRenameRoot`。
- **写路径并发语义**：单次上传 / 跨卷 move / 版本 restore / 版本 delete / 分块 init 与
  complete 共用 `<owner>\x00<rel>` 文件级锁，同 rel 并发**非阻塞 409（fail-closed）**，
  客户端应重试；批量删除 / 批量重命名不受该锁影响（逐条继续处理、幂等成功）。

### Fixed

- QUIC 传输：TLS 校验可用性 + `Accept` 死锁 / context / 流控修复。
- mux：帧负载上限收敛（接收窗口 65536 > 帧长上限 65535 导致的静默截断），编码不再
  静默截断。
- xfer：`Send` 改为全或无——短写写足，写错误即关闭连接（issue #215）。
- 版本 ID 生成 int64 溢出修复，`created_at` 毫秒语义还原。
- 多卷：跨卷版本可见性 + 写路径文件锁延伸。
- 同步任务列表投影漏 `kind`/`transport`/`carriers`，导致 Web UI 载体徽标在真实数据下
  永不显示。
- mDNS 测试收敛到单播回环地址，不再触发 Windows 防火墙弹窗（生产路径不变）。

### Security

- 跨节点只读/写面采用白名单路由 + 受限 context 委派 + 操作审计；修复版本存储迁移
  过程中引入的一条授权控制旁路。

## [0.10.0] - 2026-09-09

存储规模化与身份体系：多卷存储全链路、分层配额、凭据安全存储、认证插件化与 TOTP。

### Added

- 多卷存储：卷配置模型（`volumes`/`placement` 配置键）与卷 ACL 域包、卷集合多根 +
  每卷容量池 + reconcile 双目标、upload 卷路由 + 读路径跨卷定位 + reconcile 逐卷校准、
  特征桶跟随 + 默认卷 ACL 门禁、`GET /api/volumes` 与跨卷 move。
- 多卷客户端表面：FileClient 卷上下文、`sclient volumes` / `--volume`、Web UI 卷 badge
  与容量仪表。
- 分层配额封顶 `bucket_limits`，覆盖所有写路径并完成端到端验证。
- 凭据存储接口化：`CredentialStorer`/`SecureStorer` 与 storer 注册表；
  `EncryptingStorer` + AESGCM/KMS 双实现 + master key 装配；Vault Transit 加密后端
  （`credential_store.backend` 分支 + AAD context）。
- 凭据管理：多 SecretKey 轮换（签名 v2）、`trust` 命令、AccessKey 标准化。
- 认证面插件化 + 账号角色模型 + 公开注册端点（简单 AK/SK 默认、首 admin 回环、
  宿主 Authenticator 注入）。
- TOTP 插件：`force_totp` 注册分支 + 动态码登录 + session SK + `trust login`；
  Web 注册/登录页与客户端 JS QR。
- 操作审计日志 Web UI 查看面板。
- OpenTelemetry 装配：tracing 重命名上移 + autoexport + OTLP exporter。
- rate limiter 热更新（`UpdateConfig` 接线 `PUT /api/config`）。

### Fixed

- rate limiter 测试 `do` helper 去重。

## [0.9.0] - 2026-09-02

mesh 组网、文件同步引擎与多租户，并包含一项**破坏性**认证重构。

### Added

- mesh 数据面加固（hub/p2p/relay）；hub 状态持久化（节点/路由/信令重启不失忆）；
  TURN 凭证注入 WebRTC `ICEServers` + TURN REST 短期凭证（coturn 标准，REST 优先、
  静态回落）。
- 去中心化发现与协议通达：mDNS 局域网发现、DHT 接线（`ext/kad` → `hub.DHTRegistry`）、
  SOCKS5 代理出口、UDP 隧道（mux `FrameDatagram`）、hub 裸 TCP 中继、证书身份 +
  对端公钥指纹 pinning（防 MITM）、hub 联邦（节点表同步 + 跨 hub 链式中继）。
- 虚拟 IP / 子网分配（hub 分配 + 出口 NAT + mesh 路由 + CLI + E2E）。
- 文件同步：`pkg/sync` 引擎核心（FS 抽象 / LocalFS / 差异 / 冲突 / 并发编排）、
  HTTPTransport 传输层、服务端 `SyncManager` 任务生命周期、`sclient sync push/pull`、
  Web UI `sync_task` 频道；瞬时网络错误指数退避自动重试（`retrying` 状态）。
- `xfer` tcp+tls 传输 + TLS listener 服务端接线 + sclient 客户端装配 + e2e；
  mesh 服务解析 round-robin（同名多副本均匀分布、失败跳过）。
- `sclient mesh node` 常驻组网；sclient 前端库 + Web UI 迁移；传输管理器
  （数据层 / 下载管线 / 上传真实暂停）。
- 操作审计日志；version/meta/buildinfo 命令收拢；tracing W3C `traceparent` 可观测性
  与配置多环境。
- 多租户存储布局重构：任务级 owner 隔离、租户桶布局、配额池（移除旧 `uploads_dir`）。

### Changed

- **破坏性**：认证改为 access-key 驱动（SproxySig 请求签名 + 隧道 AK/HMAC + hub 准入），
  旧的单一 `auth_token` 模型及相关配置不再兼容。

### Fixed

- 上传断点续传可靠性 + Web UI 渲染重构；隧道断流修复。
- TURN REST 日志凭据脱敏（`Redacted` + 移除 userinfo）。
- syncmgr 测试统一确定性 `started` 信号，消除 flake。

### Security

- 长时身份密钥 + 对端公钥指纹校验，缓解中间人攻击；操作审计日志落地。

## [0.8.0] - 2026-08-21

云端离线下载任务组、续传可靠性与用量账本修正。

### Added

- 云端离线下载任务组：`CloudTask` 增加 `group_id`，组持久化到 `.__downloads__/groups/`，
  重启后自动恢复（含孤儿任务重建最小组）。
- 响应体读取空闲超时 `cloud_download_idle_timeout`（默认 1m），远端停流不再永久挂起。
- 全量下载也写入 `.partial` 文件：任意中断（网络 / 超时 / 进程重启）后均可 Range 续传。
- 排队中的下载任务可取消（`cancelFuncs` 在等信号量前注册），`Close()` 不再因排队任务卡死。
- 任务恢复支持 `cancelled` 状态；`force=false` 真正走 Range 续传。
- Web UI 云端下载新增“创建组”按钮，任务 / 组列表统一 3s 轮询。
- 配置键 `cloud_download_timeout` / `cloud_max_retries` / `cloud_retry_delay` 从 `Config`
  接线到 `CloudDownloadManager`（此前未生效），默认值：30m / 10 次 / 10s。
- 任务 / 组列表分页：`offset`/`limit`（CreatedAt 降序 + ID tie-break），API 返回
  `{tasks, total}`；SDK 新增 `WithTotal` 变体，CLI `list` 增加 `--offset`/`--limit`。
- 云归档加固：三处归档（单任务 / 批量 / 组）`O_EXCL` 防覆盖（同名 409），新增
  `cloud_archive_max_bytes` 总量限制，打包前 `TryReserve` 配额预占、打包后按实际大小对账。
- cancelled 语义统一为失败：batch/group 链与 CLI `wait` 均把取消计入失败并等所有任务
  终态后整体报错；`submitTasks`/`submitGroup` 幂等（提交阶段崩溃恢复不重复提交）。
- CLI 易用性：`list` 展示 ETag/GroupID 字段，`download`/`download-archive`/组 `download`
  增加 `--output-dir`，`delete`/`delete-group` 增加 `--yes` 确认，task/group cancel 404
  幂等统一，`--timeout 0` 表示不限时。
- Web UI：任务清理失败不再静默（toast 提示），链式下载防重入 + 组失败保留供 resume，
  `validateEntries` 改 Map 防原型键误判，host 含空格 / 非法字符的 URL 双端一致拒绝。
- 外部库客户端增强：云下载、分块上传与批量 CLI 操作。

### Fixed

- 下载卡住：默认单次尝试超时 + 空闲超时兜底，信号量不再被挂死任务占满。
- 重试语义：超时 / 网络 / 5xx 自动重试（续传），4xx / SSRF 等确定性失败不重试；
  用户取消不重试且状态不被 `failTask` 覆盖。
- 存储账本：以 `ReservedSize` 为唯一权威，完成 / 取消 / 删除 / 清理按实际预留释放并归零，
  消除每个任务约 1 GiB 的占位泄漏；重启后按磁盘扫描结果重算，避免多退 / 少退。
- 失败任务保留 `.partial` 供续传（此前 `failTask` 用 `RemoveAll` 连同部分文件一起删除）。
- 组归档改为按子任务目录收集已完成文件（此前读不存在的 `.__cloud__/<groupID>/` 恒报错），
  `archive_file` 落库到真实组对象。
- 组状态机修正（completed/partial/failed/cancelled/pending/downloading），`CancelGroup`
  不再强制把含已完成任务的组改为 cancelled。
- 任务删除竞态：删除后完成的下载不再触碰存储 / checksum / 状态。
- pkg/server 大规模代码审查修复。

## [0.7.0] - 2026-08-02

Web UI / CLI / SDK 能力扩展、链式工作流与传输安全强化。

### Added

- Web UI：文件分享、版本管理（查看 / 恢复 / 删除）、目录打包下载、运行时存储限制调整、
  小文件简单上传、文件预览。
- 分享管理（后端 API + CLI + Web UI）；服务器监控与配置管理；Hub 中继管理面板 +
  `relay` 子命令；`cloud-download list/cancel` + `stat`/`version` JSON 输出。
- sclient 抽象层重构：IOStreams / Service 接口 / Factory / State，全部子命令迁移到工厂
  函数模式，移除全局变量依赖。
- 云端下载：批量下载（服务端 + sclient + Web UI）、存储集成（统计字段 / 配置端点 /
  周期扫描 / 批量持久化）。
- 云下载归档工作流（SDK 封装 + 快捷归档 + 一键链式操作）；SDK 完整性
  （Hub/StorageConfig 方法、`ErrNotFound` 哨兵）；链式工作流 API（KVStore /
  ChainRunner / CloudDownloadChain + mtime 全链路保留）。
- TLS 默认启用 + 自签证书自动生成，客户端默认 HTTPS，无认证启动时告警。
- 证书管理重构 + ACME 自动证书 + mTLS 客户端证书 + DNSPod 插件。
- Kademlia DHT 节点发现（`ext/kad` 插件）；WebRTC xfer 适配器与注册。
- mux 异步重传队列 + 加密流读缓冲。

### Changed

- `handlers.go` 按领域拆分为多个 handler 文件。
- 性能：SHA-256 hash pool + AES cipher block cache 优化。

### Fixed

- Web UI：CSP 阻止所有 inline event handler 导致按钮无法点击；统计弹窗无效 CSS。
- 分块上传客户端未使用服务端返回的 adjusted `chunk_size`。
- 续传检测不支持 Tunnel 模式。
- sclient 缺失 `auth_token` 认证支持，`config.go` 中文乱码。

### Security

- 密码学强化：ECDH PFS、AAD 绑定、重放保护、`tunnel_key` 拒绝明文。

## [0.6.0] - 2026-07-01

工程化基座（测试 / 覆盖率 / 基准 / CI / 多 module）与云端下载 v1。

### Added

- 多 module workspace：`cmd/sproxy`、`cmd/sclient` 拆分为独立 `go.mod`，由 `go.work` 管理。
- 覆盖率门禁（CI，阈值 70%）、基准基线系统（本地 10 条记录 + CI artifact + trend）、
  统一覆盖率 / 耗时趋势报告、增强版 linter（revive/gocritic/gosec/whitespace/goimports/
  paralleltest/thelper/reassign）、pre-commit hook（vet/gofmt/loopback）。
- 测试工具集：`pkg/testutil` 及 `mockdht`/`mockxfer`/`mockserver` 子包；
  `ChecksumStoreIface`/`UploadStoreIface` 接口抽象。
- Web UI Playwright 端到端测试（独立 `go.mod`）。
- 云端下载 v1：`downloader` 包 + `CloudDownloadManager` + handlers、`sclient
  cloud-download` 命令、Web UI 管理页、执行器接线 + URL 去重 + 清理校验、重启恢复
  进行中任务、Prometheus 指标、SSRF 深度防护（扩展 blocklist）。
- 存储管理：`StorageManager` + `max_storage_bytes` 配置。
- `plugin` 包从 `pkg/tunnel/plugin` 提升为顶层 `pkg/plugin`。

### Changed

- SonarQube 全量治理：认知复杂度（S3776）、参数过多（S107）、路径注入（S2083）、
  SSRF（S5144）等问题修复；Web UI JS 拆分为多文件并完成 `var` → `let/const` 转换。
- Makefile 三段式标准化 + CI 流水线补齐；viper 抽象为 Provider 接口（cmd 层不再直接
  依赖 viper）；mux `Stream` 接口抽象消除通道竞态；sclient 全部 `os.Exit(1)` 改为
  `RunE` 错误返回。

### Fixed

- mux `acceptCh` 满时回发 `FrameReject` 而非静默丢弃，防止 `Read` 永久阻塞。
- `ListenAndServe` 失败时信号处理 goroutine 泄漏。
- `joinSafePath` 校验失败时记录 warn 日志；`bodyToString` base64 编码二进制响应体。
- WS `Send` 数据竞争与 `Close` 死锁。
- `captureRootCmdArgs` 导致 sclient 3 个测试失败。

### Security

- 所有 `filepath.Join` 替换为 `safePath`，防止路径注入；SSRF 路径校验 + QUIC TLS
  证书验证；路径安全校验改为 fail-close。

## [0.5.0] - 2026-06-13

隧道 v2：可插拔分层传输（xfer / mux / tunnel / hub）与传输插件化。

### Added

- `xfer` 传输抽象层（`Conn{Send/Receive/Close}`）+ 内置 TCP 实现 + `xfertest` 跨传输
  通用测试套件。
- `mux` 多路复用层（帧协议 / 虚拟流 / 心跳 / 流控 / 重传）+ 基于 mux 的
  `Tunnel.Do/Serve`（AES-256-GCM）。
- hub 星型中继路由表 + `sclient relay` 命令 + FileClient `WithXfer`（可插拔传输）。
- WebSocket 传输子模块（`xfer/ext/ws`，独立 go module）并设为默认传输层。
- TCP 直连 / QUIC / WebRTC（骨架）/ gRPC（骨架）传输子模块；DHT 节点发现；
  无 Hub 直连 P2P；Tracing。
- 中继实战化：自动重连 + 鉴权 + Hub 管理 API；流控 + 重传 + 大文件优化；
  Hub Prometheus 指标 + 诊断命令；mTLS 支持。
- `plugin.Registry[T]` 通用注册框架；`/api/relay` 暴露 mux 指标。

### Changed

- HTTP/TCP 传输移入 `xfer/internal/`，WS/QUIC 抽为 `ext/` 独立模块；
  `hub.DHT` / `tracing.Tracer` 抽象化（memory DHT / slog tracer 为内置实现）。

### Fixed

- mux 数据竞争（handleFrame 锁内 send、atomic lastPong、缓存 Context）。
- `bufferedResponseWriter` 数据竞争；wsConn `Send` 数据竞争与 `Close` sendLoop 死锁。

## [0.4.0] - 2026-06-06

安全加固与 P5 功能（版本管理 / 分享 / 监控 / 多用户 / 归档）。

### Added

- 文件版本管理（save/list/restore/delete）。
- 文件分享链接（`POST /api/share` + `GET /s/{token}`）。
- 统计监控（`GET /api/stats` + Web UI 监控面板）与 Prometheus `GET /metrics`。
- 多用户权限系统（API Key + 权限分配）。
- 文件归档下载（tar.gz 流式打包）。
- Web UI：前端分页、批量操作（checkbox + 批量删除 / 重命名）、隧道模式流式下载。
- CORS 配置项；`UploadStore` goroutine 健康检查；运维与发布自动化。

### Changed

- 日志字段名统一为英文 snake_case；`shortHash` 抽取到 `internal/shortid`。
- 服务端 List 分页硬上限（默认 1000）。

### Fixed

- 自动生成的隧道密钥输出到 stderr，不再污染 stdout。

### Security

- AuthToken 常量时间比较；Web UI 静态文件路由添加 CSP 响应头。

## [0.3.0] - 2026-06-04

Phase 1–4：代码质量与覆盖率、搜索 / 分页 / CI / Docker、批量操作 / 压缩 / 限流 /
模糊测试 / 排序、发布自动化 / 基准测试 / 搜索 UI / 隧道优化 / e2e / TLS 自签。

### Added

- Phase 1（代码质量与覆盖）：
  - 测试覆盖率达标：pkg/server 71.6%、pkg/client 60.2%、pkg/tunnel 83.3%
  - 新增 `internal/size` 测试、`cmd/sproxy` 测试、`cmd/sclient` 测试
  - Web UI 新增重命名按钮（调用 POST /rename）
- Phase 2（搜索/分页/CI/Docker）：
  - 文件搜索 API：`GET /api/files/search?q=keyword`（递归 WalkDir + 不区分大小写）
  - 文件列表分页：`GET /api/files?offset=N&limit=M`，响应含 total/offset/limit
  - GitHub Actions CI：lint + test（ubuntu/windows）+ 交叉编译
  - Dockerfile：多阶段构建（golang:1.26-alpine → alpine:3.21），非 root 用户
- Phase 3（批量操作/压缩/限流/模糊测试/排序）：
  - 批量删除 API：`POST /api/batch/delete`，continue-on-error 模式
  - 批量重命名 API：`POST /api/batch/rename`，continue-on-error 模式
  - 传输压缩：GzipMiddleware 透明 gzip 压缩 JSON 响应
  - 速率限制全覆盖：apiHandler 链统一应用 RateLimiter
  - ValidateFilePath 模糊测试：5s 无崩溃，84 个 interesting 输入
  - 文件列表排序：`?sort=name|size|time&order=asc|desc`
- Phase 4（发布自动化/基准测试/搜索UI/隧道优化/e2e/TLS）：
  - goreleaser 发布自动化：5 平台交叉编译 + archive 打包 + changelog
  - pkg/server 基准测试：upload 84MB/s、download 222MB/s、并发/分块
  - pkg/client 基准测试：upload 74MB/s、download 97MB/s、分块/List
  - Web UI 文件搜索：搜索栏 + 清除按钮 + 隧道/非隧道双模式
  - 隧道流性能优化：可配置 chunk 大小 + sync.Pool 减少分配
  - 端到端冒烟测试：test/e2e_test.go，启动子进程跑完整操作流程
  - TLS 自签证书自动生成：ECDSA P-256、10年有效期、含 SAN
- 新增 `internal/size` 包统一管理大小常量（client/server 共享引用），传输上限改为硬限制以消除误配导致的 413。

### Changed

- 配置新增 `tls.auto_tls` 字段：证书缺失时自动生成自签证书
- 服务端中间件链重构：localMux → GzipMiddleware → apiHandler → RateLimiter（可选）
- 文件列表 API 响应扩展：新增 `total`/`offset`/`limit` 字段（向后兼容）
- tunnel 流式加解密支持可配置 chunk 大小

### Fixed

- `context.TODO()` 替换为 `context.Background()`
- `sync.Map uploadCache` 从包级变量迁移为 FileClient 结构体字段
- `json.Encode` 和 `os.MkdirAll` 错误被忽略的问题
- 服务端 API 路由未受速率限制保护的问题
- 分块上传 `uploadComplete` 缺 `MkdirAll` 导致子目录上传失败；Go 1.26 `mime/multipart`
  截断子目录路径（新增 `X-File-Path` 头）；分块下载回退路径 `X-Chunk-Checksum` 头设置顺序错误；
  分块大小协商预留 multipart 开销避免 413。
- sclient `delete` 默认通过远端 stat 获取 checksum（新增 `--check-local`）；`list` 空目录分支遗漏。

## [0.2.0] - 2026-06-02

文件重命名 / 元信息端点、标准 Range 下载、文档体系与 15 项缺陷修复。

### Added

- 新增 `POST /rename` 端点：服务端文件重命名 / 移动，要求 `X-File-Checksum` 头与 delete 对称。
- 新增 `HEAD /api/files/stat` 端点：通过响应头返回单文件 size / checksum / mtime。
- sclient 新增 `mv` 子命令（先 Stat 取 checksum 再 Rename）。
- sclient 新增 `stat` 子命令。
- `GET /download` 支持标准 HTTP `Range` header（206 + `Content-Range`），通过
  `http.ServeContent` 实现，向下兼容旧客户端的全量下载。
- 配置项 `server_timeouts.shutdown`：graceful shutdown 超时（默认 30s）。
- 新增 `docs/` 目录：
  - `docs/api.md`：完整 HTTP API 参考
  - `docs/tunnel.md`：加密隧道协议规范
  - `docs/config.md`：配置字段表 + 优先级 + SIGHUP 范围
  - `docs/cli.md`：sclient 全部子命令参考
- `MaxMetadataBytes` 与 `ErrMetadataTooLarge` 导出，便于第三方实现兼容。

### Changed

- `server.RegisterRoutes` 改为返回 `*Handlers`，新增 `Close()` 用于优雅关停。
  `cmd/sproxy/root.go` 在 `defer` 中调用 `h.Close()`，确保 `UploadStore` 后台
  goroutine 不在进程内重启场景下泄漏。
- shutdown 流程改用 `context.WithTimeout(cfg.ServerTimeouts.Shutdown)`，
  且 `os.Exit(1)` 被替换为 `slog.Error + return`，让 defer 链路完整执行。
- `Config.Validate` 通过 `tunnel.ParseKey` 同时校验 `tunnel_key` 的长度与 hex 格式，
  错误消息更明确。
- `/download` 改用 `http.ServeContent`，不再嗅探覆盖 `Content-Type`。
- `chunk_checksum` 现为 `POST /upload/chunk` 必填字段（要求 64 位 hex）。
- `ChunkedUploadSession` 持久化时先快照 slice 再 marshal，消除与 `MarkChunkReceived` 之间的 data race。
- sclient `resolveRemotePath` 改为返回 `(string, error)`，包含 `..` 的相对路径在客户端就被拒绝。
- `config.example.yaml` 补全 `max_upload_bytes`、`server_timeouts.shutdown` 等字段的注释。

### Fixed

- **CRITICAL**：`tunnel.decodeMetadataFrame` 加入 1 MiB 长度上限，避免恶意客户端通过
  伪造 `metaLen = MaxUint32` 触发 4 GiB 内存分配（远程 OOM 拒绝服务）。
- **HIGH**：`UploadStore` 的 `persistLoop` / `cleanupLoop` goroutine 现在在进程退出
  / Handlers.Close() 时被显式停止，且 `Stop()` 通过 `sync.Once` 实现幂等。
- **HIGH**：`pkg/client.ChunkedDownload` 抽出 `tryDownloadChunk` 辅助函数，
  消除重试循环中 `defer resp.Body.Close()` 累积与双 close 风险。
- 上传 handler 不再对同一 `*os.File` 双 close（删除多余 `defer tempFile.Close()`）。
- `tunnel.dispatchLocal` 使用 `defer + recover()` 兜底，handler panic 时仍能关闭
  `metaReady` channel，避免响应组装 goroutine 永久阻塞。
- `uploadComplete` 合并分块循环改为调用 `mergeOneChunk` 辅助函数，每个 chunk 文件由
  `defer chunkFile.Close()` 落到函数边界，杜绝句柄漏关。
- `client.doRequest` 在 `(resp != nil, err != nil)` 同时返回的非典型场景下兜底关闭
  `resp.Body`，避免连接泄漏。
- `ChecksumStore.saveLocked` 失败时 `defer os.Remove(tmpPath)` 清理 `.tmp` 残留；
  启动时一次性清扫历史残留。
- `tunnel.streamRecorder.Header()` 现在加锁返回，消除潜在的 map 并发读写。

### Security

- 隧道 metadata 帧长度上限防止远程 OOM 拒绝服务。
- `tunnel_key` 严格 hex 校验避免误用非法字符导致运行时密钥解码失败。

## [0.1.0] - 2026-06-01

初始公开版。提供：

- 文件上传 / 下载 / 删除 / list / mkdir / rmdir / 分块上传 / 分块下载 API
- AES-256-GCM 加密隧道（`POST /tunnel`）
- 嵌入式 Web UI（`/ui/`）
- sclient 配套客户端（cobra + viper + XDG）

[Unreleased]: https://github.com/cocomhub/sproxy/compare/v0.11.0...HEAD
[0.11.0]: https://github.com/cocomhub/sproxy/compare/v0.10.0...v0.11.0
[0.10.0]: https://github.com/cocomhub/sproxy/compare/v0.9.0...v0.10.0
[0.9.0]: https://github.com/cocomhub/sproxy/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/cocomhub/sproxy/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/cocomhub/sproxy/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/cocomhub/sproxy/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/cocomhub/sproxy/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/cocomhub/sproxy/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/cocomhub/sproxy/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/cocomhub/sproxy/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/cocomhub/sproxy/releases/tag/v0.1.0
