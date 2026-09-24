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

## [0.20.0](https://github.com/cocomhub/sproxy/compare/v0.19.0...v0.20.0) (2026-09-24)


### Added

* **ai:** 智能运维 LLM 根因建议——llmgate 网关 + AlertEngine 接线 + ai.enabled 默认关 ([#573](https://github.com/cocomhub/sproxy/issues/573)) ([0cfd437](https://github.com/cocomhub/sproxy/commit/0cfd437bafe1d7f6b88bea931ce9452db954ca17))
* **archive:** 加密归档算法选型（cipher 参数 + fail-closed 预检） ([#552](https://github.com/cocomhub/sproxy/issues/552)) ([4290471](https://github.com/cocomhub/sproxy/commit/429047107843697d940e116ef4cd649b2d5e2353))
* **auth:** IP 白名单/信任代理——allow_ips + X-Forwarded-For 认证前门 ([#572](https://github.com/cocomhub/sproxy/issues/572)) ([dfc154b](https://github.com/cocomhub/sproxy/commit/dfc154b4c56046ac395bf2ee128fac75b713d7de))
* **auth:** RBAC 角色细分——RoleReader 只读子组 + requireRole(reader) ([#566](https://github.com/cocomhub/sproxy/issues/566)) ([0d7cd94](https://github.com/cocomhub/sproxy/commit/0d7cd941791f49444ef6607af2dc74f2f562fb3f))
* **federated:** LWW 冲突语义显式化（后写覆盖） ([#551](https://github.com/cocomhub/sproxy/issues/551)) ([79d393b](https://github.com/cocomhub/sproxy/commit/79d393bc8d8f2581c4033dbd1fb6f1b650b14b51))
* **federated:** 联邦卷本地配额统计（隧道 /remote/stats 展示远端配额） ([#554](https://github.com/cocomhub/sproxy/issues/554)) ([3bb58ad](https://github.com/cocomhub/sproxy/commit/3bb58ad930525b66bc4dd3564728dc3a8677da77))
* **files:** 内容索引（全文词元）可选开关——搜索命中正文 ([#559](https://github.com/cocomhub/sproxy/issues/559)) ([0bb7cdf](https://github.com/cocomhub/sproxy/commit/0bb7cdfb4d769f3fcc88ab24103b26e8a2b94b5f))
* **leader:** LeaderElector 选主——Local flock 恒主 + WriteGuard 写面门 ([#578](https://github.com/cocomhub/sproxy/issues/578)) ([0a0fb1d](https://github.com/cocomhub/sproxy/commit/0a0fb1d1ab2981a6fbb9652d51cecb2bbcd8da65))
* **metrics:** SLO 指标——请求延迟直方图 + Apdex 入 /metrics ([#565](https://github.com/cocomhub/sproxy/issues/565)) ([e0d73cd](https://github.com/cocomhub/sproxy/commit/e0d73cdc3ac456fcdc7837fe027f6b788fe48486))
* **metrics:** 独立指标端口（metrics_port + 令牌门） ([#550](https://github.com/cocomhub/sproxy/issues/550)) ([bc4dd7b](https://github.com/cocomhub/sproxy/commit/bc4dd7b4311d8f9fe1c4f39947b60033d3256fb0))
* **notify:** Alertmanager webhook v2 + Grafana annotations 渠道适配 ([#555](https://github.com/cocomhub/sproxy/issues/555)) ([7d7642d](https://github.com/cocomhub/sproxy/commit/7d7642d46a707312ff731bd25a1afe028fe15246))
* **quic:** 0-RTT 会话恢复（session ticket 缓存 + Allow0RTT） ([#562](https://github.com/cocomhub/sproxy/issues/562)) ([1e7827a](https://github.com/cocomhub/sproxy/commit/1e7827ac4193198db6ffbb543a1a4a34350a2d50))
* **s3:** complete 正确性加固——ETag 校验 + 配额记账 ([#575](https://github.com/cocomhub/sproxy/issues/575)) ([4952f2b](https://github.com/cocomhub/sproxy/commit/4952f2ba313baf0229acb8ab4994d879d9668b2b))
* **s3:** ListBuckets——卷即桶生态兼容（aws s3 ls / rclone 可发现） ([#568](https://github.com/cocomhub/sproxy/issues/568)) ([51fc084](https://github.com/cocomhub/sproxy/commit/51fc084151ec83cb690820adbcea78f5abf87c3b))
* **scheduler:** 通用任务调度器——统一 GC 循环 + 维护窗口 ([#574](https://github.com/cocomhub/sproxy/issues/574)) ([c6a334d](https://github.com/cocomhub/sproxy/commit/c6a334df1223725d68e754d43f6871976ca6b2e7))
* **sclient:** sync conflicts list/resolve CLI（服务端冲突索引闭环） ([#564](https://github.com/cocomhub/sproxy/issues/564)) ([ac3ef10](https://github.com/cocomhub/sproxy/commit/ac3ef10e6087f53d33bb0d5e69f2649ea9e37076))
* **sclient:** sync schedule cron 定时调度同步 ([#561](https://github.com/cocomhub/sproxy/issues/561)) ([eabcf1f](https://github.com/cocomhub/sproxy/commit/eabcf1f17466c55a8a489e42c48876fd1a9caee2))
* **sclient:** trash/quota CLI——回收站 list/restore/empty + 配额水位查询 ([#571](https://github.com/cocomhub/sproxy/issues/571)) ([5633645](https://github.com/cocomhub/sproxy/commit/563364567d502e9de6a21192b65ed6793cec8506))
* **sclient:** upgrade 自更新——GitHub 发布校验 + 原子替换 ([#577](https://github.com/cocomhub/sproxy/issues/577)) ([2183443](https://github.com/cocomhub/sproxy/commit/2183443fe8760c8be6ea3c05460f2fbc5781b939))
* **sclient:** 客户端 E2EE（--encrypt/--decrypt 零知识加解密） ([#553](https://github.com/cocomhub/sproxy/issues/553)) ([8bf5b79](https://github.com/cocomhub/sproxy/commit/8bf5b7906f1499abc870f140d935ae4e6bb7d1e5))
* **skills:** 功能对抗审查 skill——四维度/P0-P3/缺陷模式/变异验证 ([#570](https://github.com/cocomhub/sproxy/issues/570)) ([e114ca7](https://github.com/cocomhub/sproxy/commit/e114ca75b2c3d193b8db94c7f78787cdde54dda1))
* **state:** StateStore 统一状态抽象接口 + Local 默认实现 + R21 门禁 ([#576](https://github.com/cocomhub/sproxy/issues/576)) ([685f7ce](https://github.com/cocomhub/sproxy/commit/685f7ce0bf6eaecb792a66bdc2bd3c06cc38d535))
* **storage:** 加密卷算法选型（volumes[].extra.cipher + fail-closed） ([#556](https://github.com/cocomhub/sproxy/issues/556)) ([5a6a876](https://github.com/cocomhub/sproxy/commit/5a6a87601f047f5e3032a99d4df49a4a41f796a8))
* **sync:** 删除传播冲突语义（目标同步期间被改 → 保留 + skipped_conflict） ([#557](https://github.com/cocomhub/sproxy/issues/557)) ([f84c5b2](https://github.com/cocomhub/sproxy/commit/f84c5b290177d934a222298260cb9712bee490de))
* **webui:** i18n 多语言框架——en/zh 词条 + 切换持久化 + Playwright e2e ([#567](https://github.com/cocomhub/sproxy/issues/567)) ([e3c36cc](https://github.com/cocomhub/sproxy/commit/e3c36cc173c5b6a84fa10daf503b88f8e9406c0c))


### Fixed

* **ci:** minio 镜像换 chainguard——quay.io pull unauthorized 基建修复 ([#579](https://github.com/cocomhub/sproxy/issues/579)) ([d37a867](https://github.com/cocomhub/sproxy/commit/d37a8670158a73dd3e615a960ded3cf510a13059))
* **files:** 加密卷分块下载解密——修复 Open 读密文 + Seek 失败 ([#558](https://github.com/cocomhub/sproxy/issues/558)) ([86c9c75](https://github.com/cocomhub/sproxy/commit/86c9c750b7f34c71b7679a330bbd6a797ffd564e))
* **hub,kad:** flush 停用持久化——杜绝异步落盘与 TempDir 清理竞态 ([#569](https://github.com/cocomhub/sproxy/issues/569)) ([26f9e89](https://github.com/cocomhub/sproxy/commit/26f9e8951ee2920eedb7e6a6d10a6aa78567604f))
* **hub:** FederationClient 并发构造 race + metrics 计数 flake（Clone 不碰共享 + 条件等待） ([#548](https://github.com/cocomhub/sproxy/issues/548)) ([8ac710e](https://github.com/cocomhub/sproxy/commit/8ac710e737e6bf94abcaa4bb7e95669439c5db0f))
* **sftp:** 移除测试死变量 fsRoot——修复并行测试 DATA RACE ([#547](https://github.com/cocomhub/sproxy/issues/547)) ([49f5458](https://github.com/cocomhub/sproxy/commit/49f54587676051773ca1f54a2866baac5d84166e))
* **test:** 消除 remote_read/volumes 测试竞态——listener 关闭重试 + fake 后端反注册 ([#580](https://github.com/cocomhub/sproxy/issues/580)) ([e31dbaa](https://github.com/cocomhub/sproxy/commit/e31dbaa415a54f5827cc413c887fc708eddfd156))


### Changed

* **grafana:** 官方 dashboard JSON + 导入说明 ([#560](https://github.com/cocomhub/sproxy/issues/560)) ([884b8fb](https://github.com/cocomhub/sproxy/commit/884b8fb3ab8b2557e2f540fc40a4dc2fa26cc3f9))
* **roadmap:** 第 11 章完整规划 + 82 项功能设计全覆盖 ([#563](https://github.com/cocomhub/sproxy/issues/563)) ([77d8394](https://github.com/cocomhub/sproxy/commit/77d8394ce88651955680b170df7f67cdde2e4ad6))

## [0.19.0](https://github.com/cocomhub/sproxy/compare/v0.18.0...v0.19.0) (2026-09-23)


### Added

* **alert:** 配额预警（quota_watermark per-owner 水位 + 恢复通知） ([#502](https://github.com/cocomhub/sproxy/issues/502)) ([3da1368](https://github.com/cocomhub/sproxy/commit/3da1368b61a8976fe2a6fae67f80c07ccf3726da))
* **alert:** 阈值告警引擎（磁盘水位/卷 degraded/同步失败/登录锁定） ([#499](https://github.com/cocomhub/sproxy/issues/499)) ([0f11b0a](https://github.com/cocomhub/sproxy/commit/0f11b0a6ea1bd635ada32c360a6f20c4313b7a33))
* **archive:** 加密归档（RegisterCipher + 流式 AES-256-GCM + encrypt 参数） ([#510](https://github.com/cocomhub/sproxy/issues/510)) ([62e6564](https://github.com/cocomhub/sproxy/commit/62e65640f959c073645e6fa52f98870aa298d809))
* **bench:** 端到端全链路吞吐基准（xfertest 跨传输 + tcp/ws/quic） ([#505](https://github.com/cocomhub/sproxy/issues/505)) ([45d8c90](https://github.com/cocomhub/sproxy/commit/45d8c90d696c32336321fb3388d294b8b6b04d26))
* **cli:** sclient mesh up（虚拟子网用户态接入 = SOCKS5 代理） ([#522](https://github.com/cocomhub/sproxy/issues/522)) ([144c738](https://github.com/cocomhub/sproxy/commit/144c738f8b9b5ac5b8819d1c0e70fd414441faad))
* **federated:** 联邦卷回写（写面注入 + WithWriter 转发 + writable 开关） ([#516](https://github.com/cocomhub/sproxy/issues/516)) ([0fb23c2](https://github.com/cocomhub/sproxy/commit/0fb23c28f5a48a64ffe8caa4f9456c012eb8ddc8))
* **files:** gzip 压缩变换（?transform=gzip 流式 + Content-Encoding 头） ([#503](https://github.com/cocomhub/sproxy/issues/503)) ([4f5fdff](https://github.com/cocomhub/sproxy/commit/4f5fdff46906d7b149235eb88233eed0205d7447))
* **files:** 分享图片水印（?transform=thumb&watermark 半透明点阵叠加） ([#517](https://github.com/cocomhub/sproxy/issues/517)) ([64c5900](https://github.com/cocomhub/sproxy/commit/64c5900a965c4262d3b850c3e860d4ba577fa9d5))
* **files:** 回收站/软删除（trash 桶 + /api/trash 列表/恢复/清空 + TTL 清理） ([#506](https://github.com/cocomhub/sproxy/issues/506)) ([5079398](https://github.com/cocomhub/sproxy/commit/50793982eedc126e242c555f67e317f7562c1a12))
* **files:** 搜索索引快照持久化（重启载入免全量 WalkDir） ([#483](https://github.com/cocomhub/sproxy/issues/483)) ([6e51c41](https://github.com/cocomhub/sproxy/commit/6e51c41e1fb22bc14941bc627280606c51e89b58))
* **grpc:** gRPC TLS 传输（SPROXY_GRPC_CERT/KEY/CA，自签回落同 quic） ([#528](https://github.com/cocomhub/sproxy/issues/528)) ([95fb1f2](https://github.com/cocomhub/sproxy/commit/95fb1f236b6e6a189674171a7efc9666f69d7cd4))
* **grpc:** gRPC 传输真实实现 + relay/hub 装配（--transport grpc） ([#523](https://github.com/cocomhub/sproxy/issues/523)) ([26be7f6](https://github.com/cocomhub/sproxy/commit/26be7f62f67aeeea2e1ad26b1fd183c239a98161))
* **grpc:** 会话数上限（MaxConcurrentStreams 128 防 DoS） ([#532](https://github.com/cocomhub/sproxy/issues/532)) ([2e06336](https://github.com/cocomhub/sproxy/commit/2e063366d1f8aceda929081e2228cce3f0383da6))
* **gzip:** 按内容类型自动 gzip（Accept-Encoding + Content-Type 白名单 + SSE/WS 跳过） ([#542](https://github.com/cocomhub/sproxy/issues/542)) ([ba5ab74](https://github.com/cocomhub/sproxy/commit/ba5ab74289b0ca625fb42fa2ccd761d65d91d750))
* **hub:** 服务发现健康化（/api/hub/services 质量分档 + 排序） ([#501](https://github.com/cocomhub/sproxy/issues/501)) ([c01871e](https://github.com/cocomhub/sproxy/commit/c01871e98fb9310a559beee1565db3b58f2c1669))
* **hub:** 跨 hub 服务发现（FederationClient 服务交换 + 聚合 /api/hub/services） ([#511](https://github.com/cocomhub/sproxy/issues/511)) ([3187b32](https://github.com/cocomhub/sproxy/commit/3187b3243d9c84f8b845bd0fa6cd85288652bff5))
* **mesh:** hub 中继 E2E 透传（RelayStreamRequest 带 E2E/Path） ([#539](https://github.com/cocomhub/sproxy/issues/539)) ([44aa53a](https://github.com/cocomhub/sproxy/commit/44aa53a876712834b0ae92112bbdd3e03a8d5ceb))
* **mesh:** 出口节点组 --exit-group（组内按序 failover + 本地优先） ([#508](https://github.com/cocomhub/sproxy/issues/508)) ([72a645e](https://github.com/cocomhub/sproxy/commit/72a645ea4706bfc0b059fcc1ce281bda454b85fd))
* **metrics:** /metrics 可选令牌认证（metrics_token）+ xfer TCP 连接级指标 ([#497](https://github.com/cocomhub/sproxy/issues/497)) ([2edbb00](https://github.com/cocomhub/sproxy/commit/2edbb008e12dd85e45c69f807acd68121e9b7dc4))
* **metrics:** WS/QUIC 传输级指标（conns/messages/bytes + provider 注入） ([#544](https://github.com/cocomhub/sproxy/issues/544)) ([ba1e53e](https://github.com/cocomhub/sproxy/commit/ba1e53e1dc0ec7b699cf48811a53317b5fc84334))
* **metrics:** 节点级状态仪表（/metrics per-node 质量明细 + /api/hub/nodes quality） ([#507](https://github.com/cocomhub/sproxy/issues/507)) ([0cced7c](https://github.com/cocomhub/sproxy/commit/0cced7c1a13f57c8a4732c7a967d5452fd705a49))
* **notify:** 通知中心框架 + 微信渠道（wecom/serverchan）+ 运维端点 ([#496](https://github.com/cocomhub/sproxy/issues/496)) ([9d9d647](https://github.com/cocomhub/sproxy/commit/9d9d647092470f56b7258360d295bd2c5588ab6c))
* **notify:** 邮箱（SMTP）+ Webhook 通用渠道 ([#498](https://github.com/cocomhub/sproxy/issues/498)) ([7125baf](https://github.com/cocomhub/sproxy/commit/7125baf73643c5b3fbf2a935fe7916dcbcd0cb90))
* **s3:** S3 兼容服务端（/s3/ 路由 + SigV4 验签 + owner 卷映射） ([#515](https://github.com/cocomhub/sproxy/issues/515)) ([d55e956](https://github.com/cocomhub/sproxy/commit/d55e956dd1fff69b37128126df6efbed1a242111))
* **s3:** S3 分块上传（init/upload-part/complete，rclone 兼容） ([#525](https://github.com/cocomhub/sproxy/issues/525)) ([afd7aad](https://github.com/cocomhub/sproxy/commit/afd7aad65f1679e4204bbe5762ba503d3a5f8ecc))
* **s3:** S3 分块上传完整闭环 + abort（[#525](https://github.com/cocomhub/sproxy/issues/525) 补回 + abort 清理） ([#531](https://github.com/cocomhub/sproxy/issues/531)) ([b345dfe](https://github.com/cocomhub/sproxy/commit/b345dfeec23fa2afa6188624f448927c058a4205))
* **s3:** S3 服务端扩展——ListObjectsV2 + HeadObject（rclone 兼容） ([#519](https://github.com/cocomhub/sproxy/issues/519)) ([631cfb2](https://github.com/cocomhub/sproxy/commit/631cfb2c2a3dd3d1106d91c5a7cc4726fdc8eb5b))
* **s3:** 多桶语义（/s3/&lt;卷名&gt;/&lt;key&gt; 映射卷 user 桶） ([#545](https://github.com/cocomhub/sproxy/issues/545)) ([9f0d8c8](https://github.com/cocomhub/sproxy/commit/9f0d8c80580ef8c5abb519828e16aed3db1dc880))
* **sclient:** 直传失败自动重试（指数退避 + 重新签发） ([#486](https://github.com/cocomhub/sproxy/issues/486)) ([544c127](https://github.com/cocomhub/sproxy/commit/544c1277232146603680d92fa831a36498a589ee))
* **share:** 分享只读标志（readonly 参数 + 响应 + 访问头语义可见） ([#504](https://github.com/cocomhub/sproxy/issues/504)) ([14bca31](https://github.com/cocomhub/sproxy/commit/14bca3196e5bad132b43a70a13d38c04dfa6afa1))
* **share:** 分享绑定水印种子（watermark 参数 + 持久化 + 响应头） ([#546](https://github.com/cocomhub/sproxy/issues/546)) ([ba9dc06](https://github.com/cocomhub/sproxy/commit/ba9dc065f5ec63520983d439b4a1e2a3a36fe2c5))
* **stats:** /api/stats 暴露配额水位（quota 段） ([#537](https://github.com/cocomhub/sproxy/issues/537)) ([544cf3f](https://github.com/cocomhub/sproxy/commit/544cf3f592dae20c18202f10bdff4e63736af0cf))
* **sync:** at-rest 加密卷（EncryptedFS 透明加密包装） ([#518](https://github.com/cocomhub/sproxy/issues/518)) ([47cae90](https://github.com/cocomhub/sproxy/commit/47cae907573edd5812d153bf1423e0e8092ae5cc))
* **sync:** 冲突保留败方副本（keep-both 防静默丢失，覆盖型策略通用 + merge3 二进制回退） ([#487](https://github.com/cocomhub/sproxy/issues/487)) ([042f1a6](https://github.com/cocomhub/sproxy/commit/042f1a6d90d0bd18f7d2621a9c03cc7103a215b3))
* **sync:** 跨 FS 块级增量 v2（remoteFS BlockAccessor + 写面块会话） ([#494](https://github.com/cocomhub/sproxy/issues/494)) ([b3da646](https://github.com/cocomhub/sproxy/commit/b3da6461a1c0e23a30788a721b23994263d2cd21))
* **trash:** 回收站周期 GC + WebUI 视图（残余清零） ([#534](https://github.com/cocomhub/sproxy/issues/534)) ([699fa98](https://github.com/cocomhub/sproxy/commit/699fa981154c30597f4eb2eda8aaaa5fa7da37de))
* **upload:** 大文件自动转分块回退（X-Auto-Chunked 头） ([#479](https://github.com/cocomhub/sproxy/issues/479)) ([56fbad3](https://github.com/cocomhub/sproxy/commit/56fbad3c8bcb5df58a8bbcc33bba38030f81d3d8))
* **volume:** at-rest 加密卷装配开关（extra.encrypt + key 文件） ([#540](https://github.com/cocomhub/sproxy/issues/540)) ([e9213eb](https://github.com/cocomhub/sproxy/commit/e9213eb04d65bca05bfcf0230f04c2095aa15e2b))
* **volume:** 多副本镜像目标（mirror_targets N 副本） ([#484](https://github.com/cocomhub/sproxy/issues/484)) ([ef85b10](https://github.com/cocomhub/sproxy/commit/ef85b10f4b755ea69deb8ff7e15e099cdf2eb459))
* **volume:** 联邦卷只读适配层（F1 片） ([#490](https://github.com/cocomhub/sproxy/issues/490)) ([f5d2dc0](https://github.com/cocomhub/sproxy/commit/f5d2dc053d31e0a4f508953ff6395a9d1aee9aa3))
* **volume:** 联邦卷端到端（F2+F3 只读挂载远端 mesh 卷） ([#492](https://github.com/cocomhub/sproxy/issues/492)) ([a6b3703](https://github.com/cocomhub/sproxy/commit/a6b3703b2cf4517d74d4beef170b0ab32fe5a6c6))
* **webdav:** enabled 开关 + 多卷 owner 卷选择（残余清零） ([#541](https://github.com/cocomhub/sproxy/issues/541)) ([920a648](https://github.com/cocomhub/sproxy/commit/920a648b151a81b888a4ad761fcbfc5bb9a11034))
* **webdav:** 本地卷 WebDAV 服务端挂载面（/dav/ + authMiddleware） ([#500](https://github.com/cocomhub/sproxy/issues/500)) ([d860aff](https://github.com/cocomhub/sproxy/commit/d860affbd52b99ff74465cdb2f34214ed353a512))
* **webui:** stats 面板通知区（最近通知渲染 + 纯函数单测 + Playwright e2e） ([#533](https://github.com/cocomhub/sproxy/issues/533)) ([40568a2](https://github.com/cocomhub/sproxy/commit/40568a2012732626271a06b1b78811da6f87bf35))


### Fixed

* **federated:** writable 开关真正接线——只读约束 fail-closed + 回写注入生效 ([#520](https://github.com/cocomhub/sproxy/issues/520)) ([9489d95](https://github.com/cocomhub/sproxy/commit/9489d95531f154bd334ec4a6c488433ae5eab7b8))
* **files:** 搜索索引 map 并发读写窗口——copy-on-write 替换指针 ([#512](https://github.com/cocomhub/sproxy/issues/512)) ([be878cc](https://github.com/cocomhub/sproxy/commit/be878cc28df84369a504610d88a0ed55b38d5b90))
* **files:** 水印单次编码 + 缩略图共用缩放实现 ([#527](https://github.com/cocomhub/sproxy/issues/527)) ([076cdca](https://github.com/cocomhub/sproxy/commit/076cdca2c1ce433eefd9043247a016fc7925d58e))
* **files:** 水印参与 transform 缓存键——修复水印绕过/缓存污染 ([#521](https://github.com/cocomhub/sproxy/issues/521)) ([e8948d9](https://github.com/cocomhub/sproxy/commit/e8948d9d17dc5894a77ea7a4d339f92a0a59a823))
* **grpc:** Dial 恒 TLS——修复与 Listen 的 TLS 不对称 ([#535](https://github.com/cocomhub/sproxy/issues/535)) ([2b2f42a](https://github.com/cocomhub/sproxy/commit/2b2f42a10cdad71f6d398549bd8779bea0d24051))
* **ops:** 运维正确性——通知去抖含 Result + 状态机集中迁移校验 + 归档路径双保险 + 文档补齐 ([#514](https://github.com/cocomhub/sproxy/issues/514)) ([db90853](https://github.com/cocomhub/sproxy/commit/db90853f322bc5ac64354fd58239d394853fdc8c))
* **release:** 显式注入 GORELEASER_CURRENT_TAG/PREVIOUS_TAG，根治嵌套 module tag 污染发布版本 ([#482](https://github.com/cocomhub/sproxy/issues/482)) ([8a856a9](https://github.com/cocomhub/sproxy/commit/8a856a9641bd35779c28d9e7ee567af6e25ce708))
* **s3:** ListObjectsV2 XML 转义——防文件名特殊字符注入 ([#536](https://github.com/cocomhub/sproxy/issues/536)) ([41479ef](https://github.com/cocomhub/sproxy/commit/41479efbcdccdf47a5c9cd9e1c17544199a63f70))
* **s3:** 分块上传加固——partNumber 校验 + 请求体限流 ([#538](https://github.com/cocomhub/sproxy/issues/538)) ([a0b49d9](https://github.com/cocomhub/sproxy/commit/a0b49d90e73f145110599237b6370a6e12e00be9))
* **s3:** 移除生产调试日志 + 清理遗留 ([#526](https://github.com/cocomhub/sproxy/issues/526)) ([d044482](https://github.com/cocomhub/sproxy/commit/d04448261d4d6305520274bcff03492321150d15))
* **security:** 文件安全加固——用户卷 Extra 加密 + 分享淘汰可观测 + checksum 常量时间 + 目录下载拒绝 ([#513](https://github.com/cocomhub/sproxy/issues/513)) ([8aa0bbc](https://github.com/cocomhub/sproxy/commit/8aa0bbcdd573356678575bd0d6ab57e6567a253b))


### Changed

* **roadmap:** 收尾审计——联邦卷回写/S3/VPN 补标记 ([#524](https://github.com/cocomhub/sproxy/issues/524)) ([51ada5a](https://github.com/cocomhub/sproxy/commit/51ada5a632c1a1dcbcfd43e0ae578263781ebefe))
* **roadmap:** 收尾标记回填（联邦卷回写/S3） ([#530](https://github.com/cocomhub/sproxy/issues/530)) ([f43b8ec](https://github.com/cocomhub/sproxy/commit/f43b8ec483cd801f70deb472e7b408ad3cbe176e))
* **roadmap:** 新增通知与可观测性、Mesh 私有组网深化方向 ([#493](https://github.com/cocomhub/sproxy/issues/493)) ([6a9af01](https://github.com/cocomhub/sproxy/commit/6a9af0147eaeeccbb9d4208a86f6b977538b406a))
* **roadmap:** 标记上传管线/外部后端/带宽限速/质量指标/QUIC/基准门禁/搜索索引已落地 ([#488](https://github.com/cocomhub/sproxy/issues/488)) ([706f05d](https://github.com/cocomhub/sproxy/commit/706f05d22721b33feeb89c501ce1ea08f887ac11))
* **roadmap:** 标记内存观测+自动调优已落地 ([#491](https://github.com/cocomhub/sproxy/issues/491)) ([4af3260](https://github.com/cocomhub/sproxy/commit/4af3260fcab8b888ae47c111049454ff77a14fac))
* **roadmap:** 标记去重/删除传播/扇出/被动伪装/审计落盘已落地 ([#485](https://github.com/cocomhub/sproxy/issues/485)) ([898d6f2](https://github.com/cocomhub/sproxy/commit/898d6f27d4220530bae434480b3e77266a484571))
* **roadmap:** 标记大文件演进/白皮书/QUIC 已落地 ([#480](https://github.com/cocomhub/sproxy/issues/480)) ([99906ff](https://github.com/cocomhub/sproxy/commit/99906ff34aee4e394c32db9bec9b20758bbeefae))
* **roadmap:** 路线图扩容至十方向 + 15 项待设计演进详设 ([#495](https://github.com/cocomhub/sproxy/issues/495)) ([e428acb](https://github.com/cocomhub/sproxy/commit/e428acbe4d33b816d28707484209ae6d43efed68))
* **roadmap:** 集中回填联邦卷回写/S3 标记 ([#529](https://github.com/cocomhub/sproxy/issues/529)) ([b187234](https://github.com/cocomhub/sproxy/commit/b1872347feecf411fb1f2ce2b6165faf99bde7bd))
* **stealth:** 联邦卷设计规格（roadmap 3.3 P2） ([#489](https://github.com/cocomhub/sproxy/issues/489)) ([e5c2536](https://github.com/cocomhub/sproxy/commit/e5c2536c5f53c45b06412534fee123568711d4a1))

## [0.18.0](https://github.com/cocomhub/sproxy/compare/v0.17.0...v0.18.0) (2026-09-22)


### Added

* **bench:** 基准回归门禁（benchstat 基线入库 + bench-gate 对比，退化超阈值即红） ([#420](https://github.com/cocomhub/sproxy/issues/420)) ([ad32efa](https://github.com/cocomhub/sproxy/commit/ad32efa23534b86779aeef9fbb9abeedc3f05787))
* **cli:** 客户端传输统计（upload/download/cloud-download 输出速率/耗时/分块成功率） ([#436](https://github.com/cocomhub/sproxy/issues/436)) ([250c8d6](https://github.com/cocomhub/sproxy/commit/250c8d6d36faabbbbdac60ef462d2c2578a7e1c7))
* **files:** 内容寻址去重（dedup.enabled 可选，同 checksum 硬链接 + 引用计数台账） ([#424](https://github.com/cocomhub/sproxy/issues/424)) ([69eb262](https://github.com/cocomhub/sproxy/commit/69eb2622c68d5486dde8db0db7799e6fe5bbaced))
* **files:** 列表走增量文件索引（目录条目+元数据，大目录亚秒） ([#423](https://github.com/cocomhub/sproxy/issues/423)) ([e763614](https://github.com/cocomhub/sproxy/commit/e7636140e390cbcc3de9e20747c6c26957223775))
* **files:** 图片缩略图派生缓存（meta/transform 原子落盘） ([#475](https://github.com/cocomhub/sproxy/issues/475)) ([6c2fd46](https://github.com/cocomhub/sproxy/commit/6c2fd46a7d706839642f4d44a193cb99de7f578c))
* **files:** 图片缩略图缓存 GC（孤儿 tmp + TTL 回收） ([#478](https://github.com/cocomhub/sproxy/issues/478)) ([ed10350](https://github.com/cocomhub/sproxy/commit/ed10350e28e1c57bd56629b49f72fc3dedeb5260))
* **files:** 搜索/列表走增量文件索引 + max_upload_bytes 恢复可配 ([#422](https://github.com/cocomhub/sproxy/issues/422)) ([be5359a](https://github.com/cocomhub/sproxy/commit/be5359aba243ecc0d1225f449bb2becc56da3f43))
* **mesh:** relay/hub 装配 QUIC 传输（--transport quic + hub.transports.quic，UDP 形态抗 DPI 干扰） ([#418](https://github.com/cocomhub/sproxy/issues/418)) ([43010a9](https://github.com/cocomhub/sproxy/commit/43010a9a4e5ea7215671fef8060a891cdc2d989f))
* **mesh:** 传输质量感知选路（SmartDial 候选按重传率/延迟加权，mesh connect --quality-routing 显式开关） ([#446](https://github.com/cocomhub/sproxy/issues/446)) ([c527633](https://github.com/cocomhub/sproxy/commit/c527633cce9ed2589610d184b3454eada39ed92f))
* **mesh:** 质量源回填（MuxQualitySource + mesh 建连后注册，SmartDial 加权真实生效） ([#467](https://github.com/cocomhub/sproxy/issues/467)) ([65b0b2d](https://github.com/cocomhub/sproxy/commit/65b0b2d7573ad49e40a2ccb65654134217a6fe3f))
* **mesh:** 质量触发动态切换（劣化连接重拨 + 防抖 + 手动锁定可观测） ([#469](https://github.com/cocomhub/sproxy/issues/469)) ([bf75466](https://github.com/cocomhub/sproxy/commit/bf75466eca589c817bac251457d2b16097c3d59e))
* **mux:** 传输质量指标入 /metrics（重传成功/队列满/耗尽计数器，聚合展示） ([#430](https://github.com/cocomhub/sproxy/issues/430)) ([784c83b](https://github.com/cocomhub/sproxy/commit/784c83b49f608c646f606c8e5e64eae6eb8fc85b))
* **mux:** 大传输缓冲水位自动调整（SendBuffered 统计 + watermark 阈值自适应 + 防抖可观测） ([#470](https://github.com/cocomhub/sproxy/issues/470)) ([826746e](https://github.com/cocomhub/sproxy/commit/826746ef74b4e56dd1db2bce72ef8b0a64cb64dd))
* **sclient:** s3 预签名直传闭环（upload-direct 命令） ([#477](https://github.com/cocomhub/sproxy/issues/477)) ([f5509ec](https://github.com/cocomhub/sproxy/commit/f5509ec02b7a58614753cff8ef7862485fd9d00d))
* **server:** rebalance 迁移进度入 /metrics + WebUI 进度条（按卷对展示百分比） ([#448](https://github.com/cocomhub/sproxy/issues/448)) ([97cf102](https://github.com/cocomhub/sproxy/commit/97cf102f798d273fd232fff597c2ea1c8211544c))
* **server:** s3 预签名直传服务端签发端点 ([#474](https://github.com/cocomhub/sproxy/issues/474)) ([6e324f4](https://github.com/cocomhub/sproxy/commit/6e324f49d3b4328b9b1230e7c1a1b568c5c830f6))
* **server:** 上传管线扩展（RegisterTransform 插件 + 图片缩略图按需生成缓存） ([#472](https://github.com/cocomhub/sproxy/issues/472)) ([39b659e](https://github.com/cocomhub/sproxy/commit/39b659e44656a473380022c88cc638144110c305))
* **server:** 事件流补版本/分享事件（/api/events 支持 version/share 动作，EventBus 加公开 Publish） ([#437](https://github.com/cocomhub/sproxy/issues/437)) ([f33f130](https://github.com/cocomhub/sproxy/commit/f33f1303bfca9434db4d949e2268dc21324fe4ab))
* **server:** 冷热 warm 档位细化（hot→warm→cold 两级降级 + warm 独立阈值） ([#466](https://github.com/cocomhub/sproxy/issues/466)) ([88c7bb8](https://github.com/cocomhub/sproxy/commit/88c7bb86f097bb1af2ee7bac33929ee65e58cf9a))
* **server:** 冷热分层（卷 tier 属性 + 自动降级任务复用 rebalance + 读时按需回迁） ([#452](https://github.com/cocomhub/sproxy/issues/452)) ([75ef458](https://github.com/cocomhub/sproxy/commit/75ef458339a14306c243df780385f45013ede827))
* **server:** 卷健康指标（每卷读写延迟/失败率入 /metrics，带 volume+op 标签） ([#432](https://github.com/cocomhub/sproxy/issues/432)) ([c039106](https://github.com/cocomhub/sproxy/commit/c0391064a8c480b381bdf25eb1122569837d6ad6))
* **server:** 受认证保护 /debug/pprof + 分配指标（debug_pprof_enabled 显式开关默认关） ([#463](https://github.com/cocomhub/sproxy/issues/463)) ([28855e9](https://github.com/cocomhub/sproxy/commit/28855e9fbee2478a3b2031019e835ce27249294b))
* **server:** 审计默认落盘 + /api/audit 过滤查询（重启历史可查） ([#431](https://github.com/cocomhub/sproxy/issues/431)) ([ae2abe2](https://github.com/cocomhub/sproxy/commit/ae2abe269662bfa7c9ab8b6305770e23df799788))
* **server:** 带宽限速接入多实例协调后端（rate_limit.bandwidth.coord_backend=file，跨实例共享字节配额，等待不拒绝） ([#427](https://github.com/cocomhub/sproxy/issues/427)) ([86a501b](https://github.com/cocomhub/sproxy/commit/86a501bfb18d6fbfdd4292999a701d1175d87ec5))
* **server:** 文件变更事件流 SSE /api/events（per-owner 游标回放） ([#433](https://github.com/cocomhub/sproxy/issues/433)) ([18a1ce7](https://github.com/cocomhub/sproxy/commit/18a1ce72eccb31bc5755f5217a695a37cff1a760))
* **server:** 文件级带宽限速（rate_limit.bandwidth per-owner token 桶，upload/download 双路径） ([#425](https://github.com/cocomhub/sproxy/issues/425)) ([437c74f](https://github.com/cocomhub/sproxy/commit/437c74ff532f50f1229c715ba4dccc9fd6202630))
* **sync:** sclient sync watch 补 push/both 方向（双向连续同步，delete 传播可配，防双向死循环） ([#445](https://github.com/cocomhub/sproxy/issues/445)) ([b4a3edc](https://github.com/cocomhub/sproxy/commit/b4a3edcf2ac622d09d43058fed0c4ff11fd52610))
* **sync:** sclient sync watch 连续同步（事件流驱动增量同步，去抖+轮询回退+优雅退出） ([#441](https://github.com/cocomhub/sproxy/issues/441)) ([de6a518](https://github.com/cocomhub/sproxy/commit/de6a5181183c2680cd995b6b031a0bea34d4ff89))
* **sync:** 冲突索引 API（ConflictRecorder 持久化 + GET /api/sync/conflicts + resolve） ([#464](https://github.com/cocomhub/sproxy/issues/464)) ([aa8e190](https://github.com/cocomhub/sproxy/commit/aa8e190e5c79ff6b2eb1229c73c37f0c1041ff3e))
* **sync:** 删除传播（delete_policy=propagate）+ 双向同步（sync both） ([#421](https://github.com/cocomhub/sproxy/issues/421)) ([86221b9](https://github.com/cocomhub/sproxy/commit/86221b9233072e50b53839d0d1591412dc4e5104))
* **sync:** 同步任务失败单文件重试（POST /api/sync/tasks/{id}/retry，sclient sync retry） ([#442](https://github.com/cocomhub/sproxy/issues/442)) ([b45bfbb](https://github.com/cocomhub/sproxy/commit/b45bfbbbb97ae1356d05d4e763570424e886ffaa))
* **sync:** 同步校验与统计报告（--verify 核对 checksum，汇总+失败清单输出） ([#435](https://github.com/cocomhub/sproxy/issues/435)) ([09e9110](https://github.com/cocomhub/sproxy/commit/09e9110339f41f8fb38bd444efbc32b39a4b368a))
* **sync:** 块级增量同步 v1（固定块 SHA-256 比对只传差异块，复用分块 checksum 基建） ([#451](https://github.com/cocomhub/sproxy/issues/451)) ([0b0cfff](https://github.com/cocomhub/sproxy/commit/0b0cfffc033a3337315e2723959e373c26e5bab3))
* **sync:** 多节点扇出（一次 push 到多 sync_remotes，失败节点独立重试 + 汇总视图） ([#459](https://github.com/cocomhub/sproxy/issues/459)) ([f283613](https://github.com/cocomhub/sproxy/commit/f2836136904827ef1006a26f9e0efeb118a156d3))
* **sync:** 文本冲突 3 方合并（diff3 纯 Go + conflict_policy=merge3，冲突写标记文件 + ConflictRecorder 回调） ([#461](https://github.com/cocomhub/sproxy/issues/461)) ([b5f0cab](https://github.com/cocomhub/sproxy/commit/b5f0cab12b78097e15b57e402f0f95580e06c485))
* **tunnel:** WS 路径/升级头自定义（两端一致 + 默认零回归）+ JA3 指纹差异清单文档 ([#462](https://github.com/cocomhub/sproxy/issues/462)) ([555f41a](https://github.com/cocomhub/sproxy/commit/555f41a7a67511adc2c9bbdf3d23c19f7b583a9f))
* **tunnel:** 被动伪装层（TLS cipher/ALPN 可配 + 空闲填充开关 + 启用状态可观测） ([#453](https://github.com/cocomhub/sproxy/issues/453)) ([928c8ae](https://github.com/cocomhub/sproxy/commit/928c8aea1cbee1a4d6466d0a8312451b59a322c2))
* **volume:** s3 大文件分片上传（multipart + 阈值可配 + 失败重试 + 中止清理） ([#460](https://github.com/cocomhub/sproxy/issues/460)) ([5838bee](https://github.com/cocomhub/sproxy/commit/5838beee9272b70200a47b7a604a6de0af9b17be))
* **volume:** s3 签名 v4 直传（presigned URL 签发） ([#473](https://github.com/cocomhub/sproxy/issues/473)) ([21003b1](https://github.com/cocomhub/sproxy/commit/21003b1fc1f19852c019c129521ba6f44856cad4))
* **volume:** SFTP 外部后端（List/Read/Write/Delete + 健康探针 degraded 可观测） ([#454](https://github.com/cocomhub/sproxy/issues/454)) ([1f071b4](https://github.com/cocomhub/sproxy/commit/1f071b42ed8f0ce4ad003ff045c2587de2dd5ec6))
* **volumes:** 跨卷复制与定时镜像（POST /api/volumes/copy + volumes[].mirror_to） ([#419](https://github.com/cocomhub/sproxy/issues/419)) ([f5ae1da](https://github.com/cocomhub/sproxy/commit/f5ae1dae0bc437120aaf06e999ccfe494ff47725))
* **web:** WebUI SSE 实时刷新（EventSource 订阅 /api/events 增量刷新，断线重连+游标回放） ([#434](https://github.com/cocomhub/sproxy/issues/434)) ([6cece42](https://github.com/cocomhub/sproxy/commit/6cece425eff0e93e1cb6bebd05b3a5ba998a4e5e))
* **web:** WebUI 卷健康仪表（/metrics volume_io 解析渲染，healthy/warning/degraded 徽标） ([#440](https://github.com/cocomhub/sproxy/issues/440)) ([3e0abb0](https://github.com/cocomhub/sproxy/commit/3e0abb06bd997fafc217f15bf61bab3dbbf8c3a8))


### Fixed

* **cloud:** quota_sink 测试断言改条件等待（容忍 DeleteTask 异步配额释放时序，根治 CI +Vault 偶发 flake） ([#439](https://github.com/cocomhub/sproxy/issues/439)) ([d151c73](https://github.com/cocomhub/sproxy/commit/d151c734cd6c8a8d9eb78ecc9dc71d8b1d81d3f5))
* **e2e:** TestAuth_SaveKeysSigns 补 v2 签名 skey-id 预置 + 列表渲染正向断言（根治 CI flake） ([#447](https://github.com/cocomhub/sproxy/issues/447)) ([86d8bf0](https://github.com/cocomhub/sproxy/commit/86d8bf07874e56aaa6e722f15d4e36a61a410cda))
* **files:** readChunkBody 先拷贝后归还池条目（根治并发 chunk 上传 DATA RACE） ([#457](https://github.com/cocomhub/sproxy/issues/457)) ([8e0db3a](https://github.com/cocomhub/sproxy/commit/8e0db3ab768c890c246db1991029ef6646463e6c))
* **files:** 去重硬链接 FAT/exFAT 不可用时回退普通复制（内容复制+台账登记+配额双计，上传不报错） ([#429](https://github.com/cocomhub/sproxy/issues/429)) ([192e471](https://github.com/cocomhub/sproxy/commit/192e471a8360d68abca04d3797d03c12c9ea5cde))
* **mux:** StreamID 回绕 fail-closed 防护（ErrStreamIDExhausted） ([#417](https://github.com/cocomhub/sproxy/issues/417)) ([6016948](https://github.com/cocomhub/sproxy/commit/6016948325eeda37b6ab6cc4747bda61a3c6f6b4))
* **syncmgr:** 终态日志锁内快照 + chunk 池按条目归还 + deadcode 升 v0.50.0 + linux 基线补录（修复残余四件套） ([#450](https://github.com/cocomhub/sproxy/issues/450)) ([c8b2a0f](https://github.com/cocomhub/sproxy/commit/c8b2a0f5204603c7ed707a0c84e29407a11952ce))
* **sync:** 块级增量复制 WriterAt 双 Close 消除（显式 Close 置 nil，Windows 句柄偶发占用根治） ([#456](https://github.com/cocomhub/sproxy/issues/456)) ([07d2577](https://github.com/cocomhub/sproxy/commit/07d25774cdb33737b80b926e67f676ea40b68a43))
* **webdav:** 测试注入独立连接池 + pre-commit 排除 worktree（修 CI transport connection broken flake） ([#455](https://github.com/cocomhub/sproxy/issues/455)) ([6f1afea](https://github.com/cocomhub/sproxy/commit/6f1afeac68a2d96a31d0a8ede54682720022c717))


### Changed

* **bench:** Benchmark job 全局互斥 + I/O 塌陷判据阈值 2s→5s（缓解 runner 拥塞误判） ([#428](https://github.com/cocomhub/sproxy/issues/428)) ([f1db417](https://github.com/cocomhub/sproxy/commit/f1db417cf90f52fa96ffcebcc199bd85b469254e))
* **ci:** Benchmark 拆为手动触发 workflow，PR 不再跑基准阻塞检查 ([#458](https://github.com/cocomhub/sproxy/issues/458)) ([355450c](https://github.com/cocomhub/sproxy/commit/355450cf6405aecd6f163a1051131cc750cbc851))
* **core:** 减少隧道/上传/分块路径的重复分配（缓冲池化与复用） ([#444](https://github.com/cocomhub/sproxy/issues/444)) ([245c322](https://github.com/cocomhub/sproxy/commit/245c3228c24061e7653c8b78ea50896f4768c880))
* **docs:** 修复 mesh E2E 接线表述 + 补门禁清单与发布事故归档 ([#416](https://github.com/cocomhub/sproxy/issues/416)) ([54c4a90](https://github.com/cocomhub/sproxy/commit/54c4a9039e8c1167da4a6a3646f2bd5d49be1625))
* **files:** dedup 台账 per-owner 懒建缓存（避免高频上传每次磁盘 Load dedup.json） ([#426](https://github.com/cocomhub/sproxy/issues/426)) ([dc2ef19](https://github.com/cocomhub/sproxy/commit/dc2ef19ab0f3a240729dd7679e805ab4ef8a0cc2))
* **files:** 分块上传读缓冲改为所有权语义（消除整块拷贝 + 修复池泄漏） ([#471](https://github.com/cocomhub/sproxy/issues/471)) ([80cb1d5](https://github.com/cocomhub/sproxy/commit/80cb1d56bd6b5c72bd57cecbe86a539c75bad85c))
* **mux:** 发送缓冲池提升到 mux 级共享（短命流复用 + 免流表锁） ([#465](https://github.com/cocomhub/sproxy/issues/465)) ([560711b](https://github.com/cocomhub/sproxy/commit/560711bb573765710037195265dc5edc839c1627))
* **roadmap:** 新增设计发展规划（文件服务/多卷/云同步/跨墙可识别性/性能五方向路线图） ([#415](https://github.com/cocomhub/sproxy/issues/415)) ([3017286](https://github.com/cocomhub/sproxy/commit/3017286d9115fe0e79b4f985ccd31112a76f8ac1))
* **roadmap:** 标注事件通知/同步校验/客户端传输统计已落地（[#434](https://github.com/cocomhub/sproxy/issues/434)-[#437](https://github.com/cocomhub/sproxy/issues/437)） ([#438](https://github.com/cocomhub/sproxy/issues/438)) ([fd112db](https://github.com/cocomhub/sproxy/commit/fd112db03cb57c9622ccb2493a1d96ec38618c66))
* **roadmap:** 标注冷热 warm 档/冲突索引/内存观测已落地（[#463](https://github.com/cocomhub/sproxy/issues/463)-[#467](https://github.com/cocomhub/sproxy/issues/467)） ([#468](https://github.com/cocomhub/sproxy/issues/468)) ([b223343](https://github.com/cocomhub/sproxy/commit/b2233438d48a078acf94ca69db12547046cefbce))
* **roadmap:** 标注卷健康迁移仪表/传输质量选路已落地（[#445](https://github.com/cocomhub/sproxy/issues/445)-[#448](https://github.com/cocomhub/sproxy/issues/448)） ([#449](https://github.com/cocomhub/sproxy/issues/449)) ([8406c52](https://github.com/cocomhub/sproxy/commit/8406c525e73441ad1350f6de36b6ca04c643133d))
* **roadmap:** 标注连续同步/同步重试/卷健康仪表已落地（[#439](https://github.com/cocomhub/sproxy/issues/439)-[#442](https://github.com/cocomhub/sproxy/issues/442)） ([#443](https://github.com/cocomhub/sproxy/issues/443)) ([47e1322](https://github.com/cocomhub/sproxy/commit/47e132231b13c03b07fba576da5a56e08b4013c7))
* **stealth:** CDN WebSocket 官方前置指南 ([#476](https://github.com/cocomhub/sproxy/issues/476)) ([9c50ebb](https://github.com/cocomhub/sproxy/commit/9c50ebb48a2899295ad838d57e00372d12238202))

## [0.17.0](https://github.com/cocomhub/sproxy/compare/v0.16.0...v0.17.0) (2026-09-20)


### Added

* **mesh:** direct/webrtc 直连端到端加密接线（L⇄T 直连路径 E2E）([#410](https://github.com/cocomhub/sproxy/issues/410)) ([cc35bd7](https://github.com/cocomhub/sproxy/commit/cc35bd7489ee840bf6acbc0f7f9957ae8c67c488))
* **mesh:** via-relay 多跳端到端加密接线（X 中间节点纯字节泵透传）([#408](https://github.com/cocomhub/sproxy/issues/408)) ([aca5ef2](https://github.com/cocomhub/sproxy/commit/aca5ef2b267f3854fdba9a6613e2fd95631cd36f))
* **mesh:** 端到端加密生产接线（一期 L↔T 字节流形态）([#406](https://github.com/cocomhub/sproxy/issues/406)) ([8f4899e](https://github.com/cocomhub/sproxy/commit/8f4899e5f8239821ad40397004e987991f62e6e5))
* **net:** netutil.DefaultTransport 共享工厂 + 装配层默认共享、SDK 库隔离 + option 注入 ([#411](https://github.com/cocomhub/sproxy/issues/411)) ([0a035b3](https://github.com/cocomhub/sproxy/commit/0a035b3ae3f9ef7c7094d6224347308797d4943e))


### Fixed

* **mesh:** via-direct 兼容路径假成功修复（X 未确认出口就绪即失败）([#412](https://github.com/cocomhub/sproxy/issues/412)) ([39b769c](https://github.com/cocomhub/sproxy/commit/39b769cb964047b95a516702028677704e21ffb6))


### Changed

* **archcheck:** CI 挂载 archcheck 门禁 + 测试侧 Transport 统一 netutil.IsolatedTransport + mesh data race 修复 ([#403](https://github.com/cocomhub/sproxy/issues/403)) ([50f7122](https://github.com/cocomhub/sproxy/commit/50f71222c23316b5cdaf72d334a152850cb1decb))
* **net:** 全仓裸 Transport 收敛 IsolatedTransport 基座 + R19 门禁升级拦全部裸构造 ([#407](https://github.com/cocomhub/sproxy/issues/407)) ([225ef9f](https://github.com/cocomhub/sproxy/commit/225ef9ffcdfb00347e02e1d22ffa902afa837330))
* **net:** 生产 http.Client 全显式 Transport 连接池 + R20 门禁 + 测试 client helper 收敛 testutil ([#409](https://github.com/cocomhub/sproxy/issues/409)) ([26a57ad](https://github.com/cocomhub/sproxy/commit/26a57adc36106b6a5db4b541146de4df0d6c024f))
* **release:** 消除同一 tag 双跑 + 修复子 module tag 补建 identity ([#404](https://github.com/cocomhub/sproxy/issues/404)) ([e30538a](https://github.com/cocomhub/sproxy/commit/e30538a564add5763fe91c0f8447ab8a1c09f732))

## [0.16.0](https://github.com/cocomhub/sproxy/compare/v0.15.0...v0.16.0) (2026-09-20)


### ⚠ BREAKING CHANGES

* **cli:** context set 字段 flag 改名（--env → --env-name / --user → --user-name / --volume → --volume-name），脚本需迁移。
* **mesh:** PathProvider 接口从 Dial/Enabled 改为 Expand() []Candidate；缓存 key 从提供者名改为候选 ID。外部实现者需适配。

### Added

* **cli:** cliflag 通用包收敛 flag 读取样板 ([#400](https://github.com/cocomhub/sproxy/issues/400)) ([2a8a1e3](https://github.com/cocomhub/sproxy/commit/2a8a1e30ced06693c92b6f289ce7dafcf21b6493))
* **cli:** context set 字段 flag 改名避免与 root 全局 flag 同名异意 ([#398](https://github.com/cocomhub/sproxy/issues/398)) ([5f2d889](https://github.com/cocomhub/sproxy/commit/5f2d889e4177ad2362f475c9b664da95774efc89))
* **http-proxy:** 经目标节点出口的正向 HTTP 代理 ([#394](https://github.com/cocomhub/sproxy/issues/394)) ([def6fdf](https://github.com/cocomhub/sproxy/commit/def6fdfb98b7563228a0f04053a8db024f003864))
* **mesh:** http-proxy 出口路由并行竞速 + 云端下载经 mesh 出口 ([#395](https://github.com/cocomhub/sproxy/issues/395)) ([6c44290](https://github.com/cocomhub/sproxy/commit/6c4429068f215222205d47a36da491c66028cae8))
* **mesh:** SmartDial 多路径竞速择优（自动选最佳路由） ([#382](https://github.com/cocomhub/sproxy/issues/382)) ([9b2315a](https://github.com/cocomhub/sproxy/commit/9b2315a2119cf7ba2c3e2670e998e1c58aa49a6d))
* **mesh:** via-direct-X 数据面直连多跳（双候选竞速，经 X 打洞免 hub 字节） ([#388](https://github.com/cocomhub/sproxy/issues/388)) ([236f22f](https://github.com/cocomhub/sproxy/commit/236f22fb5ea64695a0e6f4383d0624de1fe22172))
* **mesh:** via-node 多跳提供者（候选展开模型，端到端 RTT 自动择优） ([#385](https://github.com/cocomhub/sproxy/issues/385)) ([4c0a857](https://github.com/cocomhub/sproxy/commit/4c0a8573d13f1e0499f4fd1680a4d0a45cd900bd))
* **mesh:** 候选索引（缓存命中零 Expand 快照复用）+ via-node 真实数据面 e2e ([#386](https://github.com/cocomhub/sproxy/issues/386)) ([2d6bdd1](https://github.com/cocomhub/sproxy/commit/2d6bdd1a7339ce05c72d1bf976ec75d6540e3a04))
* **mesh:** 端到端加密协议层 + SmartDial 竞速精度修正（T1-T7）([#396](https://github.com/cocomhub/sproxy/issues/396)) ([42c842b](https://github.com/cocomhub/sproxy/commit/42c842b20f2a041798c5561a31e6238a0eaea2cd))
* **net:** netutil.IsolatedTransport 统一隔离 Transport 构造 + 门禁 ([#402](https://github.com/cocomhub/sproxy/issues/402)) ([9dbbc7f](https://github.com/cocomhub/sproxy/commit/9dbbc7f391ffd69b5f5805ccd7de146588d18c92))


### Fixed

* **accesskey:** vault 集成测试 HTTP client 按请求自建，隔离共享连接池 ([#399](https://github.com/cocomhub/sproxy/issues/399)) ([2c8564a](https://github.com/cocomhub/sproxy/commit/2c8564a9f8108d205be8bfd29215ac3d89003994))
* **cloud:** 任务配额所有权句柄收敛（TaskAccount）— 根治并发 resume/cancel 账本泄漏 ([#384](https://github.com/cocomhub/sproxy/issues/384)) ([db46e97](https://github.com/cocomhub/sproxy/commit/db46e977bf53f87c6c8b2de232ea893a38dfa4e6))


### Changed

* **agents:** 新增提交/发布纪律项目 skill（破坏性变更格式与补救） ([#387](https://github.com/cocomhub/sproxy/issues/387)) ([3a055ad](https://github.com/cocomhub/sproxy/commit/3a055ad54aaca6194ffc18d6b52e8fcb129fd789))
* **agents:** 新增文档生命周期纪律 skill（设计文档收敛约束）+ .pi/ 纳入 docs-only 忽略清单 ([#391](https://github.com/cocomhub/sproxy/issues/391)) ([acd58b6](https://github.com/cocomhub/sproxy/commit/acd58b63c18a927bd7fa713db6b8dac2102648c3))
* **cli:** 补 mesh connect --smart/--mdns/--virtual-subnet 路由择优文档 ([#390](https://github.com/cocomhub/sproxy/issues/390)) ([f8bd8f8](https://github.com/cocomhub/sproxy/commit/f8bd8f884d21c55adfb68d13f228daba5a846d44))
* **docs:** 整合历史学习文档到权威归档（learnings/.trae/.learnings/.workbuddy 清理） ([#397](https://github.com/cocomhub/sproxy/issues/397)) ([4364e4a](https://github.com/cocomhub/sproxy/commit/4364e4abef5b386559378a28b51f51f724d6e9fa))
* **docs:** 清理历史设计文档并归档经验（docs 功能化整理） ([#389](https://github.com/cocomhub/sproxy/issues/389)) ([eaf241f](https://github.com/cocomhub/sproxy/commit/eaf241f53ad673bc48562f954c18f2ba2bb85c5d))
* **docs:** 补齐 mesh 出口族权威文档缺口 ([#393](https://github.com/cocomhub/sproxy/issues/393)) ([57cdc63](https://github.com/cocomhub/sproxy/commit/57cdc632ea7cdd88ce3d1e57ef07ab1e2441fb93))
* **mesh:** 沉淀 SmartDial/via-node/via-direct 演进经验 + README/architecture 自动选路文档 ([#392](https://github.com/cocomhub/sproxy/issues/392)) ([bb5c46d](https://github.com/cocomhub/sproxy/commit/bb5c46d29b6eac35b6ca7433edec393c275971f9))
* **net:** 全仓消除 http.DefaultClient 共享连接池（测试批量隔离 + 生产兜底自建） ([#401](https://github.com/cocomhub/sproxy/issues/401)) ([d91f530](https://github.com/cocomhub/sproxy/commit/d91f530c1466d0e4ef5c79d8524616d5996baec7))

## [0.15.0](https://github.com/cocomhub/sproxy/compare/v0.14.0...v0.15.0) (2026-09-19)


### ⚠ BREAKING CHANGES

* **context:** sclient 配置从单份平铺 sclient.yaml 重构为 kubectl 式 environments/users/contexts 三件套（~/.config/sproxy/config.yaml）：
    - `--config <path>` 现指向新 config.yaml（旧 sclient.yaml 自动迁移导入为 context，之后 config.yaml 为唯一事实源）；
    - `SCLIENT_ENV` 语义从「切整份文件」变为「映射/选择 environment」（同名 context 优先）；
    - mesh/relay/p2p/socks 默认连接面（hub/node_id/TURN/STUN/virtual_subnet）与凭据改从当前 context 的 env/user 回落（显式 flag 仍优先）；
    - `config show/set` 输出从平铺变为「当前 context 解析后的扁平视图」；
    - `trust login` 不再隐式注册新账号（注册由独立 `trust register` 承担）。

### Added

* **bench:** 总时长守卫（--max-duration 5m，I/O 塌陷聚合慢提前终止） ([#372](https://github.com/cocomhub/sproxy/issues/372)) ([3032ee0](https://github.com/cocomhub/sproxy/commit/3032ee02a77601b2809673e19418086be32328f3))
* **cli:** trust register/login 分家——register 独立注册，login 纯登录免记 AK ([#377](https://github.com/cocomhub/sproxy/issues/377)) ([52fb845](https://github.com/cocomhub/sproxy/commit/52fb845643c91c2e9294ff66f48c718f915edd9a))
* **context:** sclient 多环境多用户 context 模型重构（environments/users/contexts 三件套）([#381](https://github.com/cocomhub/sproxy/issues/381)) ([9c68526](https://github.com/cocomhub/sproxy/commit/9c68526ef2b47912679d9ccbc397f4e354378681))
* **credentials:** TOTP 注册两段式提交（pending 确认制）+ owner 幂等 + 首 admin 单槽 ([#369](https://github.com/cocomhub/sproxy/issues/369)) ([37e32d1](https://github.com/cocomhub/sproxy/commit/37e32d1a6090513ef99a6880ae21ee89edeff92b))
* **deploy:** Docker Compose + Helm chart 部署工件（一键部署 + K8s 支持） ([#379](https://github.com/cocomhub/sproxy/issues/379)) ([a389b65](https://github.com/cocomhub/sproxy/commit/a389b65f5daa388aab65fc29cc35a8e896746143))
* **hub:** 多跳链式中继发现层补全——联邦节点表合并候选，3 hub 递归转发 e2e ([#378](https://github.com/cocomhub/sproxy/issues/378)) ([43edcee](https://github.com/cocomhub/sproxy/commit/43edcee69c0d76360f0954fcdcdceab9a6d71849))
* **login:** 用户名（owner）登录——服务端 owner→AK 反查 + CLI/Web UI 免记 AK ([#374](https://github.com/cocomhub/sproxy/issues/374)) ([0514a4d](https://github.com/cocomhub/sproxy/commit/0514a4d14e320d2d6be559209c87ca06e84f4c47))
* **sclient:** 信令面 --ca-file 支持 + client 创建路径收敛（mesh/p2p/socks/udp/relay 自签 hub 可用） ([#365](https://github.com/cocomhub/sproxy/issues/365)) ([52c2770](https://github.com/cocomhub/sproxy/commit/52c2770c6389dc56169d84698b9b74faefcc5770))
* **volume:** 外部卷容量纳管（独立容量 + 用量查询 + 系统限额） ([#371](https://github.com/cocomhub/sproxy/issues/371)) ([b315b04](https://github.com/cocomhub/sproxy/commit/b315b04f52e5f67554c34519668efea0daa94de0))


### Fixed

* **release:** 子 module tag 自动化（release.yml 挂 tag-release） ([#367](https://github.com/cocomhub/sproxy/issues/367)) ([df8b252](https://github.com/cocomhub/sproxy/commit/df8b25275ebdea932fcdf3e59d184d8f033faf3a))
* **web:** 非 2xx 错误信息解析服务端 error 字段 + 日志审计/Benchmark 方案 ([#368](https://github.com/cocomhub/sproxy/issues/368)) ([fae2fb3](https://github.com/cocomhub/sproxy/commit/fae2fb32b639bc37e9613d81a6f13b0549ba3a7f))


### Changed

* **fix:** go fix stdlib 现代化一次性清理 + PR 前 make build 经验 ([#375](https://github.com/cocomhub/sproxy/issues/375)) ([7aa47d8](https://github.com/cocomhub/sproxy/commit/7aa47d8190456474899e0f3b357128a697aecf0a))
* **gates:** go fix/addlicense/PR title 硬门禁 + 多 module pre-commit ([#376](https://github.com/cocomhub/sproxy/issues/376)) ([728e58d](https://github.com/cocomhub/sproxy/commit/728e58dc0958a2ebc7a4072a32acfb159a6eb3b8))
* **hub:** 单 hub 100+ 节点规模化验证（注册/查找/中继拨号/删除压力） ([#380](https://github.com/cocomhub/sproxy/issues/380)) ([bfdeb42](https://github.com/cocomhub/sproxy/commit/bfdeb423f9d6f85296f4f11605c3a3226b26633b))
* **webdav:** 真实 WebDAV 服务端兼容性测试（可选，WEBDAV_ENDPOINT 驱动） ([#373](https://github.com/cocomhub/sproxy/issues/373)) ([db56d8f](https://github.com/cocomhub/sproxy/commit/db56d8f1c2815e5ce9a797733dc65b57bd565a33))
* **webrtc:** 包级 slog 注入 logger + 凭据轮换测试修复 ([#370](https://github.com/cocomhub/sproxy/issues/370)) ([f1b7a8c](https://github.com/cocomhub/sproxy/commit/f1b7a8c43ff280cb74c6e3cf5f91588344832bb8))

## [0.14.0](https://github.com/cocomhub/sproxy/compare/v0.13.0...v0.14.0) (2026-09-18)


### ⚠ BREAKING CHANGES

* **files:** 删除 SetSessionRoute / SetSessionStorageMgrReserved / SetSessionTempPath 三个导出 setter——生产路径已全用 setSession*IfCurrent 门控变体，外部消费者须改用 GetSession 取会话后调用门控变体（同包测试路径），或经 UploadInit 生产路径。

### Added

* **baidupcs:** P2 库兜底真实现（分片上传+断点续传），脱离二进制完整可用 ([#355](https://github.com/cocomhub/sproxy/issues/355)) ([0e04b05](https://github.com/cocomhub/sproxy/commit/0e04b057b8800f4b2de77c47f14daf0cd5306f28))
* **baidupcs:** P3 sync.FS 适配层 + VolumeBackend，网盘成为可同步存储 ([#354](https://github.com/cocomhub/sproxy/issues/354)) ([0171aba](https://github.com/cocomhub/sproxy/commit/0171abaa376d5894aaea09a45a418e409d0c0175))
* **baidupcs:** P4 双向同步集成 + V3 volume 接入（多盘系统卷，可插拔 backend）([#356](https://github.com/cocomhub/sproxy/issues/356)) ([862572d](https://github.com/cocomhub/sproxy/commit/862572dfb13676f14759daa8125ad12aa466e6d2))
* **baidupcs:** 百度网盘存储后端 plugin（独立 module + 二进制优先/库兜底）([#339](https://github.com/cocomhub/sproxy/issues/339)) ([71f1c87](https://github.com/cocomhub/sproxy/commit/71f1c87f908802270f5238c9ecdd6b81bba8d229))
* **baidupcs:** 百度网盘存储后端（外部 fork + replace 接入，零污染） ([#349](https://github.com/cocomhub/sproxy/issues/349)) ([a170fc3](https://github.com/cocomhub/sproxy/commit/a170fc3877aeb5f4f331670545b4d058a7782a94))
* **config:** 支持人类可读字节大小配置（owner_quotas/bucket_limits/vol_capacity） ([#327](https://github.com/cocomhub/sproxy/issues/327)) ([3260195](https://github.com/cocomhub/sproxy/commit/3260195a15c060c72d410e2dca3e8a44f6fe33c5))
* **credentials:** 凭据定期自动轮换调度器（到期前通知+keep_old 裁剪） ([#337](https://github.com/cocomhub/sproxy/issues/337)) ([bdf204a](https://github.com/cocomhub/sproxy/commit/bdf204a5a1e97a0bf0d95a7ed2bbf70e69251f7b))
* **gateway:** WebDAV 网关 + sproxy dav 本地代理，任意工具访问远端 ([#338](https://github.com/cocomhub/sproxy/issues/338)) ([7ae7a37](https://github.com/cocomhub/sproxy/commit/7ae7a375fabd154ce85b12d57d988b545412ff68))
* **ops:** 备份/恢复脚本 + SIGHUP 热更新范围收敛测试 ([#335](https://github.com/cocomhub/sproxy/issues/335)) ([5fc8f21](https://github.com/cocomhub/sproxy/commit/5fc8f21dcc6eab00ebc65e7081f36c0990823fe6))
* **ratelimit:** 多实例协调限流（文件锁共享计数） ([#334](https://github.com/cocomhub/sproxy/issues/334)) ([f9c3993](https://github.com/cocomhub/sproxy/commit/f9c3993dfd25fc6dcdd3dc0faa29b2fffca988d5))
* **server:** liveness/readiness 探针分离 + 审计导出端点 ([#329](https://github.com/cocomhub/sproxy/issues/329)) ([b336ea5](https://github.com/cocomhub/sproxy/commit/b336ea5188e9286e3e30b8f580e8881144b83c39))
* **share:** 分享链接持久化，重启后恢复未过期分享 ([#328](https://github.com/cocomhub/sproxy/issues/328)) ([cc00e71](https://github.com/cocomhub/sproxy/commit/cc00e71038dd2ab843d2fada351c6e5e956882df))
* **versioning:** 保留期+周期版本 GC ([#330](https://github.com/cocomhub/sproxy/issues/330)) ([fdf8e0f](https://github.com/cocomhub/sproxy/commit/fdf8e0f8f010f47fc4d514f7910f4ff8c6b5fb10))
* **volumes:** /api/volumes/rebalance 卷再平衡端点 ([#336](https://github.com/cocomhub/sproxy/issues/336)) ([cc96b5c](https://github.com/cocomhub/sproxy/commit/cc96b5c5a1f7207d97d0aa028ef5046a1517a2dd))
* **volume:** S3 对象存储后端（go.mod 隔离 + MinIO 容器测试） ([#361](https://github.com/cocomhub/sproxy/issues/361)) ([5dc3b34](https://github.com/cocomhub/sproxy/commit/5dc3b3402eb6ec281f0de16ae2dcff4f967e726d))
* **volume:** V3 通用卷模型框架（Type/Extra + 可插拔后端） ([#357](https://github.com/cocomhub/sproxy/issues/357)) ([004dcbf](https://github.com/cocomhub/sproxy/commit/004dcbf769372348b7cd99843f6867e122f69d19))
* **volume:** WebDAV 存储后端（V3 plugin 第一个真实扩展） ([#360](https://github.com/cocomhub/sproxy/issues/360)) ([d9e7935](https://github.com/cocomhub/sproxy/commit/d9e7935c3aa37c971d163606a4656cb39c985d2e))
* **volume:** 用户卷 quota per-owner 融合 ([#362](https://github.com/cocomhub/sproxy/issues/362)) ([a7620e0](https://github.com/cocomhub/sproxy/commit/a7620e078638536036e18a9c9dab0b1ac4cea8d5))
* **volume:** 用户卷 Web UI + sclient CLI 接入（e2e + Playwright） ([#359](https://github.com/cocomhub/sproxy/issues/359)) ([c9cb2d2](https://github.com/cocomhub/sproxy/commit/c9cb2d2b60f324750bd8db9f5b34ee8cc13b1eca))
* **volume:** 用户卷（per-owner meta store + 管理 API） ([#358](https://github.com/cocomhub/sproxy/issues/358)) ([2f6cff6](https://github.com/cocomhub/sproxy/commit/2f6cff6a072139bc66f22c56a613e4f934481780))


### Fixed

* **baidupcs:** 回退百度网盘 plugin 合并（避免开源实现受 sproxy 强校验污染）([#342](https://github.com/cocomhub/sproxy/issues/342)) ([4c1cd86](https://github.com/cocomhub/sproxy/commit/4c1cd8662037438e1ebb732b87b8151c549196ea))
* **files:** 删除不门控导出 setter（SetSessionX 生产零调用，统一门控变体） ([#363](https://github.com/cocomhub/sproxy/issues/363)) ([892ed21](https://github.com/cocomhub/sproxy/commit/892ed21f78b52afdc6d73ad844cb0468e4e92d08))
* **sclient:** --ca-file/--insecure 在 HTTP 直连面生效（含 trust login TOTP 注册） ([#364](https://github.com/cocomhub/sproxy/issues/364)) ([d68b3fb](https://github.com/cocomhub/sproxy/commit/d68b3fbed9d7fd9bd279b280ae0431d4c29b9965))
* **server:** 写面 TOCTOU 窗口闭合（delete/cloud/version） ([#331](https://github.com/cocomhub/sproxy/issues/331)) ([495a840](https://github.com/cocomhub/sproxy/commit/495a840ca2312f517c36221e2576f56526f54b88))
* **test:** rate limiter 更新配置测试改用 1s 窗口（修 CI Windows flake） ([#346](https://github.com/cocomhub/sproxy/issues/346)) ([0cc7a04](https://github.com/cocomhub/sproxy/commit/0cc7a0451bd8370acd3bce33ccaa4f5327d5d844))


### Changed

* **agents:** 沉淀提交信息与 PR 描述原则（合并信息聚焦功能维度） ([#341](https://github.com/cocomhub/sproxy/issues/341)) ([0960528](https://github.com/cocomhub/sproxy/commit/09605280a8a9b3c235596f3a6214cd74f296aaaf))
* **agents:** 纯文档 PR 规则更新为「走 docs-only 占位通道可秒合并」 ([#348](https://github.com/cocomhub/sproxy/issues/348)) ([ecac08e](https://github.com/cocomhub/sproxy/commit/ecac08e219fc7d49e1f0fd928de86918fb3eeae5))
* **deps:** bump github.com/mxschmitt/playwright-go ([#152](https://github.com/cocomhub/sproxy/issues/152)) ([75d9011](https://github.com/cocomhub/sproxy/commit/75d9011cda8e098a3618a86fff99f0fe81f5ffb4))
* **docs:** 混合代码 PR 不再触发 docs-only（job 级 detect + if 精确判定变更集） ([#352](https://github.com/cocomhub/sproxy/issues/352)) ([c1db862](https://github.com/cocomhub/sproxy/commit/c1db86269762e7e8b2171059d13a5055a12098bc))
* **docs:** 纯文档 PR 用同名占位检查满足必检（免跑完整 CI） ([#345](https://github.com/cocomhub/sproxy/issues/345)) ([9e8695f](https://github.com/cocomhub/sproxy/commit/9e8695f959e10e0c47e73e0e7410cf3946e0472b))
* **git:** ignore .worktrees + docs(plans): 四份实现计划 ([#333](https://github.com/cocomhub/sproxy/issues/333)) ([9599119](https://github.com/cocomhub/sproxy/commit/9599119f03bae405520c266c8c1aa170e0bc71a0))
* **mux,tcp,tunnel:** 协议帧解析 fuzz 扩展 ([#332](https://github.com/cocomhub/sproxy/issues/332)) ([e3f018c](https://github.com/cocomhub/sproxy/commit/e3f018c9a199c48c484da30912f11ab0f7a79798))
* **readme:** 回归验证 docs-only 在混合跳过改动后仍生效 ([#353](https://github.com/cocomhub/sproxy/issues/353)) ([2d18408](https://github.com/cocomhub/sproxy/commit/2d184087e26d56d0c2db04cc35930094aed227c2))
* **readme:** 验证 docs-only 占位检查通道 ([#347](https://github.com/cocomhub/sproxy/issues/347)) ([9b724fb](https://github.com/cocomhub/sproxy/commit/9b724fb973d41f5f5ba2bb965d792f72de233d43))
* **release:** release PR 只触发 docs-only，不再跑完整 CI ([#351](https://github.com/cocomhub/sproxy/issues/351)) ([6b07323](https://github.com/cocomhub/sproxy/commit/6b073237a638914dee235b5fc7d1de9c2ac38e74))
* **release:** release-please 分支只触发 docs-only 占位检查（免跑完整 CI） ([#350](https://github.com/cocomhub/sproxy/issues/350)) ([0e1d05d](https://github.com/cocomhub/sproxy/commit/0e1d05dfe92a2fd7eca955701915dca1e3a9fa93))
* **toolchain:** go work sync 修剪 go.work.sum 过期 sum（pion pin 收束 + playwright [#152](https://github.com/cocomhub/sproxy/issues/152)） ([#326](https://github.com/cocomhub/sproxy/issues/326)) ([6471dac](https://github.com/cocomhub/sproxy/commit/6471dac213af0f56064407e48b579d900b27ae84))

## [0.13.0](https://github.com/cocomhub/sproxy/compare/v0.12.0...v0.13.0) (2026-09-16)


### ⚠ BREAKING CHANGES

* **toolchain:** 消费方工具链需 Go 1.27 以上；Go 语言版本下限从 1.26 提升为 1.27。

### Fixed

* **bench:** 看门狗终止前先发 SIGQUIT 抓 goroutine 栈，卡死现场可定位 ([#319](https://github.com/cocomhub/sproxy/issues/319)) ([50c54fb](https://github.com/cocomhub/sproxy/commit/50c54fbb52fec709955685f8f88b7a52d5e5ee7b))
* **release:** 移除 packages['.'].component，修复 release-please 对 [#282](https://github.com/cocomhub/sproxy/issues/282) 的 untagged 误判 ([#320](https://github.com/cocomhub/sproxy/issues/320)) ([8f745c3](https://github.com/cocomhub/sproxy/commit/8f745c3635e852f85e73054ad93b1099f55dbd4f))


### Changed

* **toolchain:** go get -u 更新全部模块依赖（go1.27） ([#324](https://github.com/cocomhub/sproxy/issues/324)) ([ba64d72](https://github.com/cocomhub/sproxy/commit/ba64d729352e504deb903bf877c7b7545d990266))
* **toolchain:** Go 指令与 CI 工具链升级到 Go 1.27（破坏性变更） ([#322](https://github.com/cocomhub/sproxy/issues/322)) ([8297ae6](https://github.com/cocomhub/sproxy/commit/8297ae6b659ee0e9d7d300a8bc4ade35e94cf1c0))
* **toolchain:** 应用 go1.27 go fix 现代语法转换 ([#323](https://github.com/cocomhub/sproxy/issues/323)) ([05a90db](https://github.com/cocomhub/sproxy/commit/05a90dbce3b0dc30f2376752e17b254106b80637))

## [0.12.0](https://github.com/cocomhub/sproxy/compare/v0.11.1...v0.12.0) (2026-09-16)


### ⚠ BREAKING CHANGES

* **client:** client.WithTracer(nil) 由「保持默认 slog tracer」改为「完全关闭追踪」（不再注入 traceparent）；默认 tracer 的 span 行降为 Debug 级，默认 Info 配置下不再输出。

### Added

* **mux:** 流级可观测性——活跃流数与最久空闲时长指标（审计 F6 修法②） ([#316](https://github.com/cocomhub/sproxy/issues/316)) ([e918765](https://github.com/cocomhub/sproxy/commit/e918765b681afb41c3ad1862ae98625f427984e9))


### Fixed

* **archcheck:** R18 并发门禁改为扫描全仓并重建基线 ([#294](https://github.com/cocomhub/sproxy/issues/294)) ([9d4792b](https://github.com/cocomhub/sproxy/commit/9d4792b0933e224c16ccdc413d7c7dbb61e086c0))
* **archcheck:** R18 门禁堵住两类静默漏检（识别正则与子树覆盖）并统一排除口径 ([#299](https://github.com/cocomhub/sproxy/issues/299)) ([aa717cc](https://github.com/cocomhub/sproxy/commit/aa717cc8c75038234b36bdd72b35623badfe7611))
* **auth:** 认证日志改走可注入 logger，去掉匿名请求的 SproxySig 误报 WARN ([#289](https://github.com/cocomhub/sproxy/issues/289)) ([b6827a4](https://github.com/cocomhub/sproxy/commit/b6827a49d331e46de0a1dbba5a4a5eab38d3c473))
* **bench:** benchmark 入口加包级 -timeout，卡死时输出 goroutine 栈而非被 job 静默取消 ([#300](https://github.com/cocomhub/sproxy/issues/300)) ([e750716](https://github.com/cocomhub/sproxy/commit/e750716944ae33680a8e68f0b431659d029ade7e))
* **bench:** client benchmark 加停滞守卫，把 6 分钟静默超时变成 2 秒响亮失败 ([#287](https://github.com/cocomhub/sproxy/issues/287)) ([770bd5d](https://github.com/cocomhub/sproxy/commit/770bd5d58c894d03d37501f1447d346e55556568))
* **bench:** pkg/server benchmark 走生产装配并真正校验结果（修 401/400 空跑与 make bench 吞失败） ([#285](https://github.com/cocomhub/sproxy/issues/285)) ([83da0f1](https://github.com/cocomhub/sproxy/commit/83da0f155c7d33b122a537d170e43246d8e9f852))
* **bench:** 卡死的 benchmark 由进程外看门狗快速失败并保留日志（-timeout 对 benchmark 无效） ([#312](https://github.com/cocomhub/sproxy/issues/312)) ([6329f32](https://github.com/cocomhub/sproxy/commit/6329f32a2432b0539462dbc749ce53535bcaf5cf))
* **bench:** 并发上传基准不再从 worker goroutine 内 Fatal，并发度随机器而非硬编码 10 ([#295](https://github.com/cocomhub/sproxy/issues/295)) ([7848ddd](https://github.com/cocomhub/sproxy/commit/7848ddd32d4e68422ceadd10ec034b16c49baab2))
* **client:** 分块上传遇「会话缺少在途临时文件」自动重新初始化自愈 ([#317](https://github.com/cocomhub/sproxy/issues/317)) ([8de7480](https://github.com/cocomhub/sproxy/commit/8de748097b083dccf9b95437493dba9e576b6e60))
* **client:** 客户端追踪默认静默（span 行降为 debug）且可按 logger 改道/彻底关闭 ([#293](https://github.com/cocomhub/sproxy/issues/293)) ([ab12cd0](https://github.com/cocomhub/sproxy/commit/ab12cd074ca610996f6d999e198001f6a90ba7c5))
* **cloud:** 丢弃分片时只回拨实际消失的字节，删除失败不再多退配额 ([#305](https://github.com/cocomhub/sproxy/issues/305)) ([7eaacc7](https://github.com/cocomhub/sproxy/commit/7eaacc78b3c49c311b915d0c2f5f63e00ea19ca3))
* **cloud:** 云任务取消后配额归零与删除失败重试，消除状态与磁盘/账本不一致 ([#290](https://github.com/cocomhub/sproxy/issues/290)) ([f27ee6e](https://github.com/cocomhub/sproxy/commit/f27ee6ee40f0c336902645e455878b44ec601047))
* **cloud:** 崩溃后不再遗留永不启动的任务，续传失败不再改写任务终态 ([#298](https://github.com/cocomhub/sproxy/issues/298)) ([56b17d9](https://github.com/cocomhub/sproxy/commit/56b17d943c7989bcaf7fd8ae3598fe294d52aa1c))
* **cloud:** 租户不可用时续传失败不再遗留运行标记与占位 ([#296](https://github.com/cocomhub/sproxy/issues/296)) ([fd32fe8](https://github.com/cocomhub/sproxy/commit/fd32fe8cc7a7d863565c3f88ddf0182168a27125))
* **downloader:** 416 finalize 失败径不再删除 partial，消除配额账本残留 ([#315](https://github.com/cocomhub/sproxy/issues/315)) ([9dce02c](https://github.com/cocomhub/sproxy/commit/9dce02cb3986846c94bac8334edc6cad1dfd8674))
* **files:** 会话释放路径与持久化快照经同一把内嵌锁互斥，消除 DATA RACE ([#314](https://github.com/cocomhub/sproxy/issues/314)) ([87579c2](https://github.com/cocomhub/sproxy/commit/87579c22557fac9fef35bf188d0a086248f5d5fa))
* **files:** 分块上传加 total_chunks 上界与「合并中」屏障（防内存放大与校验-落盘 TOCTOU） ([#303](https://github.com/cocomhub/sproxy/issues/303)) ([d5dd988](https://github.com/cocomhub/sproxy/commit/d5dd9886985fd39bd7189e5663de4653f4951d30))
* **files:** 分块上传的会话状态发布改为锁内写，已完成会话不再释放 P5 预留 ([#304](https://github.com/cocomhub/sproxy/issues/304)) ([8d12c6f](https://github.com/cocomhub/sproxy/commit/8d12c6f11bf0ca088459c45d2a662b67d3511424))
* **files:** 分块会话持久化经会话内嵌串行锁，消除旧快照覆盖新快照的乱序落盘 ([#313](https://github.com/cocomhub/sproxy/issues/313)) ([0f62f7a](https://github.com/cocomhub/sproxy/commit/0f62f7a6c0760c8af4e9f647d113bb3ac620d9bd))
* **files:** 分块会话的状态发布改为按注册世代校验身份 ([#311](https://github.com/cocomhub/sproxy/issues/311)) ([bce7fdd](https://github.com/cocomhub/sproxy/commit/bce7fdd0288b44ef17746a1ce779c328412f3d50))
* **files:** 回收分块会话的孤儿产物，init 发布失败时回滚预留与临时文件 ([#309](https://github.com/cocomhub/sproxy/issues/309)) ([2542d0f](https://github.com/cocomhub/sproxy/commit/2542d0ffbaee18e8c5822bf7c29da2be168bfa75))
* **mux:** readLoop 内的 UDP 转发与心跳回包不再同步阻塞 ([#308](https://github.com/cocomhub/sproxy/issues/308)) ([5b27aa6](https://github.com/cocomhub/sproxy/commit/5b27aa649a9838a0d91b19f6b776bc79d939d84e))
* **mux:** readLoop 内的帧投递不再阻塞，dataCh 满时落入以窗口为上界的溢出缓冲 ([#310](https://github.com/cocomhub/sproxy/issues/310)) ([fef67d3](https://github.com/cocomhub/sproxy/commit/fef67d344861fba5e709aafe4184368853db9a88))
* **mux:** 修远端可触发进程崩溃的短 WindowUpdate 帧，并让 Abort 真正注销流 ([#301](https://github.com/cocomhub/sproxy/issues/301)) ([b6e49e6](https://github.com/cocomhub/sproxy/commit/b6e49e667bcd7a5ad37e74681e66e807b2044b2d))
* **mux:** 窗口信用不再静默丢失，重传队列满改为关连接 ([#306](https://github.com/cocomhub/sproxy/issues/306)) ([770108b](https://github.com/cocomhub/sproxy/commit/770108b0733e3a10c44ab4236c4daf2ae592a816))
* **quota:** 超额释放只按本层实际扣减量传播，不再污染祖先账本 ([#302](https://github.com/cocomhub/sproxy/issues/302)) ([078d219](https://github.com/cocomhub/sproxy/commit/078d219f6f9427e4b340b6ffb5c0a11ccd5aa403))
* **release,cloud:** release PR 标题模板含版本号与 scope + 任务文件删除先于终态发布（修 Windows flake） ([#281](https://github.com/cocomhub/sproxy/issues/281)) ([8763728](https://github.com/cocomhub/sproxy/commit/87637280aa6280586434576b745b35be175138ca))
* **release:** 聚合 PR 标题模板也须含版本号（group-pull-request-title-pattern） ([#284](https://github.com/cocomhub/sproxy/issues/284)) ([3383230](https://github.com/cocomhub/sproxy/commit/338323021aa8cc081621ddcbfd018870b831bb53))
* **telemetry:** span 嵌套层级按父 span 递推，修日志缩进无限增长 ([#286](https://github.com/cocomhub/sproxy/issues/286)) ([9eeaf4f](https://github.com/cocomhub/sproxy/commit/9eeaf4f5b01d1b87946519e19572f6fec0b7c96f))
* **test:** 修掉 pkg/client 测试传输层注册表的跨用例串扰（全局 Clear 与时钟派生重名） ([#297](https://github.com/cocomhub/sproxy/issues/297)) ([09c03ca](https://github.com/cocomhub/sproxy/commit/09c03caf75d4f9501597692e3e9ed23ba747ed6e))
* **test:** 分享用例断言上传/创建链路并钉住空 token 不返回 200（消假绿） ([#291](https://github.com/cocomhub/sproxy/issues/291)) ([36201e3](https://github.com/cocomhub/sproxy/commit/36201e3be47789673abeff800c3d44e2bf0a3f73))


### Changed

* **bench:** client benchmark 夹具不再把上传体落盘 ([#283](https://github.com/cocomhub/sproxy/issues/283)) ([877826a](https://github.com/cocomhub/sproxy/commit/877826a277f28350aa27b6fccd6202ba8140de13))
* **bench:** 删除重复定义的 bench-old 目标 ([#288](https://github.com/cocomhub/sproxy/issues/288)) ([8e0badc](https://github.com/cocomhub/sproxy/commit/8e0badcea1179c44abdfd3694182786912208f35))
* **cloud:** 删除死字段 reservation 并为审计 F2/F5 补判据守卫与注释 ([#307](https://github.com/cocomhub/sproxy/issues/307)) ([1a53174](https://github.com/cocomhub/sproxy/commit/1a531743db8ade2056dbd3081e270650d446a236))
* **httptransport:** deadline 用例改走 synctest 虚拟时钟，去掉 15s 墙钟窗口 ([#292](https://github.com/cocomhub/sproxy/issues/292)) ([5e3337c](https://github.com/cocomhub/sproxy/commit/5e3337c164f6bcdf71102328b5bfb6f8f9dee3fd))

## [0.11.1](https://github.com/cocomhub/sproxy/compare/v0.11.0...v0.11.1) (2026-09-15)


### Removed

- `pkg/tunnel.NewHandler` —— 统一到 `NewLocalHandler`（第二个参数传 `nil` 即纯外部转发，前者是其特例）。
- `(*tunnel.Handler).UpdateKey` —— 空实现；隧道密钥由认证层按 AK→SK 派生并放入请求 ctx，**不可热替换**。
- `pkg/server.TunnelUpdater` 与 `(*server.Handlers).TunnelHandler()` —— 随 `tunnel_key` 废除后已无调用方。
- `pkg/tunnel/xfer/ext/grpc.XferServer` —— 零引用空接口。

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
* **docs:** 全量刷新 md 反映最新现状（移除过期路由/配置/传输实现）+ 扩展文档漂移门禁 R9 + fix(build): bench 补 prepare 依赖 ([bb422a9](https://github.com/cocomhub/sproxy/commit/bb422a9e171e3add7de375a93adf7195ae64723d))

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
