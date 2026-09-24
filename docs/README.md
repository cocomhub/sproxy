<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 文档索引

> sproxy：轻量文件上传/下载/删除服务 + 加密隧道 + mesh 内网穿透，附带 `sclient` 客户端二进制。

本目录是**现行事实源**（功能文档）。历史设计/计划文档已归档至 [docs/archive/](./archive/README.md)。

## 功能文档

| 文档 | 内容 |
|------|------|
| [api.md](./api.md) | HTTP API 参考：文件/目录/分块上传下载/批量/搜索/分享/版本/云端下载/存档/隧道/hub/凭据/mesh 端点与错误码 |
| [architecture.md](./architecture.md) | 分层传输架构（xfer / mux / tunnel / hub）、正向 HTTP 代理数据流与多租户存储布局 |
| [cli.md](./cli.md) | sclient 全部子命令参考（upload/download/list/stat/mv/cd/tunnel/relay/sync/socks/udp/mesh/http-proxy/cloud-download/context/identity 等） |
| [config.md](./config.md) | 服务端配置字段（含 cloud_downloader 配置族 / hub 传输）、优先级、SIGHUP 热重载、备份/恢复、WebDAV 网关、客户端 context 模型 |
| [deploy.md](./deploy.md) | 部署指南：Docker Compose、Helm chart、镜像、生产建议 |
| [tunnel.md](./tunnel.md) | 加密隧道协议规范（帧协议、加密参数、路由模式、安全性） |
| [mesh-testing.md](./mesh-testing.md) | Mesh 内网穿透实测指南（云服务器 + 两台 NAT 电脑） |
| [glossary.md](./glossary.md) | Mesh 场景术语表（角色 / 网络关系 / 术语映射） |
| [roadmap.md](./roadmap.md) | **设计发展规划**：文件服务 / 多卷 / 云同步 / 跨墙可识别性 / 性能五方向的现状盘点、差距分析与 P0/P1/P2 演进路线图 |
| [testing/virtual-time-conversions.md](./testing/virtual-time-conversions.md) | 测试固定等待清理台账（R14 门禁引用） |
| [grafana/README.md](./grafana/README.md) | Grafana dashboard 导入说明（`sproxy-dashboard.json`，Prometheus 数据源） |

## 归档

- [docs/archive/](./archive/README.md) — 已实现功能的历史经验沉淀（代码审查方法论 / mesh 演进 / 架构决策 / benchmark CI 稳定性 / 协作规则 / CI 合并流程）
- `docs/archive/` 另含 2026-09-20 自 `docs/superpowers/learnings/` 移入的 4 份活跃规则文档
  （agent-operating-rules、ci-merge-process、benchmark-ci-timeout-disk-io、gofix-before-pr）

## 文档维护约定

- 改动 API/配置/CLI 行为时同步更新对应根级文档（门禁 R9/R15 防漂移）
- 新增功能设计不必留在 docs/ 顶层；实现落地后把**经验教训**沉淀到 `docs/archive/` 对应主题
- 纯文档改动走 CI docs-only 占位通道（见 `ci-docs-only.yml`），可快速合并
