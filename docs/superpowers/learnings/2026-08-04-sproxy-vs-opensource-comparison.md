<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 与开源方案对比分析

> 编写日期：2026-08-04
> 分析范围：sproxy 项目全功能与主流开源文件服务/隧道方案的异同、优缺点、适用场景与未来发展方向

## 一、sproxy 项目简介

sproxy 是 `github.com/cocomhub/sproxy` 下的轻量文件服务 + 加密隧道项目，双二进制架构（sproxy 服务端 + sclient 客户端），约 24k LOC Go + 37k LOC 测试代码。

### 核心定位

**文件服务 + 加密隧道 + 中继网络的三合一平台**，三者深度整合而非拼凑。

### 现有功能清单

| 功能域 | 具体能力 | 实现位置 |
|--------|----------|----------|
| **文件操作** | 上传/下载/删除/重命名/移动 | `pkg/server/` |
| **目录操作** | 创建/删除目录（递归） | `pkg/server/dirs.go` |
| **分块上传** | 初始化/上传分块/状态查询/完成/断点续传 | `pkg/server/chunked_upload.go` |
| **分块下载** | 自定义偏移/大小下载，单块 SHA-256 校验 | `pkg/server/chunked_download.go` |
| **批量操作** | 批量删除/重命名，continue-on-error | `pkg/server/` |
| **文件列表** | 分页/排序/子目录过滤 | `pkg/server/list_handler.go` |
| **文件搜索** | 递归 WalkDir 子串匹配（不区分大小写） | `pkg/server/list_handler.go` |
| **文件分享** | TTL/最大下载次数/一次性链接/撤销 | `pkg/server/share.go` |
| **文件版本管理** | 上传前自动备份/版本列表/恢复/删除 | `pkg/server/version.go` |
| **云端下载** | 服务端 URL 下载/任务管理/取消/重试 | `pkg/server/cloud_download.go` |
| **云端归档** | 下载完成后自动打包/批量归档 | `pkg/server/cloud_archive_handler.go` |
| **存档（Archive）** | 服务端压缩/解压缩 | `pkg/server/archive.go` |
| **链式工作流** | KVStore/ChainRunner/CloudDownloadChain | `pkg/client/chain*.go` |
| **加密隧道（传统）** | AES-256-GCM + POST /tunnel 请求转发 | `pkg/tunnel/tunnel.go` |
| **加密隧道（多路复用）** | Tunnel.Do/Serve 基于 mux 虚拟流 | `pkg/tunnel/tunnel_mux.go` |
| **虚拟流多路复用** | mux 层：帧协议/心跳/重传/流管理 | `pkg/tunnel/mux/` |
| **传输层抽象** | xfer.Conn (Send/Receive/Close) 可插拔 | `pkg/tunnel/xfer/` |
| **TCP 传输** | 内置 HTTP POST 传输 | `pkg/tunnel/xfer/internal/tcp/` |
| **WebSocket 传输** | 独立子模块 | `pkg/tunnel/xfer/ext/ws/` |
| **QUIC 传输** | 独立子模块 | `pkg/tunnel/xfer/ext/quic/` |
| **gRPC 传输** | 独立子模块 | `pkg/tunnel/xfer/ext/grpc/` |
| **WebRTC 传输** | 独立子模块 | `pkg/tunnel/xfer/ext/webrtc/` |
| **Hub 中继网络** | 星型拓扑/节点注册/路由表/中继转发 | `pkg/tunnel/hub/` |
| **P2P 直连** | DHT 发现 + WebRTC + mux 多路复用 | `pkg/tunnel/p2p/` |
| **Kademlia DHT** | 分布式路由表扩展 | `pkg/tunnel/hub/ext/kad/` |
| **ECDH PFS 握手** | 隧道密钥完美前向保密 | `pkg/tunnel/ecdh.go` |
| **重放保护** | IAT/JTI 机制防止重放攻击 | `pkg/tunnel/replay.go` |
| **认证** | Bearer Token / 多用户 API 密钥（读写权限） | `pkg/server/auth.go` |
| **速率限制** | 滑动窗口全局限流 | `pkg/server/ratelimit.go` |
| **CORS** | 白名单来源/方法/头/预检 | `pkg/server/cors.go` |
| **Gzip 压缩** | 透明 JSON 响应压缩 | `pkg/server/gzip.go` |
| **Prometheus 指标** | 请求数/字节数/活跃连接/mux 指标/云端下载指标 | `pkg/server/metrics.go` |
| **存储管理** | 按分类（用户/分块/版本/云端）统计/配额控制 | `pkg/server/storage_manager.go` |
| **配置 API** | 运行时读取/更新存储配置 | `pkg/server/config_api.go` |
| **TLS 证书管理** | 文件证书/ACME 自动证书/DNSPod/自签证书/mTLS | `pkg/certmgr/` |
| **Web UI** | 嵌入式 SPA 文件管理界面 | `web/static/` |
| **sclient CLI** | 30+ 子命令：上传/下载/删除/隧道/中继/分享/诊断等 | `cmd/sclient/` |
| **FileClient SDK** | Go 原生 SDK：FileClient 结构体 | `pkg/client/` |
| **路径安全** | ValidateFilePath + joinSafePath + 符号链接检测 | `pkg/server/validate.go` |
| **Checksum 校验** | 所有文件操作强制 SHA-256 校验 | `pkg/server/checksum.go` |
| **健康检查** | GET /healthz（含 UploadStore 健康状态） | `pkg/server/handlers.go` |
| **Docker 支持** | 多阶段构建，非 root 用户 | `Dockerfile` |
| **GoReleaser CI/CD** | 5 平台交叉编译 + archive 打包 | `.goreleaser.yaml` |
| **E2E 测试** | 子进程二进制测试 | `test/e2e_test.go` |
| **模糊测试** | ValidateFilePath/calcChunkSize | `pkg/server/*_fuzz_test.go` |

## 二、开源方案概览

### 2.1 文件管理类

| 项目 | 语言 | Stars | 定位 |
|------|------|-------|------|
| **File Browser** | Go + Vue | ~26k | 轻量 Web 文件管理器（已归档） |
| **Cloudreve** | Go + React | ~22k | 自建云盘，多存储后端 |
| **Filestash** | Go + React | ~14k | 通用文件管理平台，存储无关 |
| **PsiTransfer** | Node.js + Vue | ~1.8k | 简单文件分享，无账号 |

### 2.2 隧道/反向代理类

| 项目 | 语言 | Stars | 定位 |
|------|------|-------|------|
| **frp** | Go | ~90k | 快速反向代理，NAT 穿透 |
| **Chisel** | Go | ~13k | TCP/UDP 隧道 over HTTP，SSH 加密 |
| **ngrok** | Go | ~1.6k (discussions) | 商业隧道服务，付费 |

### 2.3 功能矩阵对比

| 功能 | sproxy | FileBrowser | Cloudreve | Filestash | PsiTransfer | frp | Chisel |
|------|--------|-------------|-----------|-----------|-------------|-----|--------|
| 文件上传/下载 | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ |
| 分块上传/断点续传 | ✅ | ❌ | ✅ | ❌ | ✅ (tus) | ❌ | ❌ |
| 目录管理 | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 文件搜索 | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 文件分享链接 | ✅ | ❌ | ✅ | ✅ | ✅ | ❌ | ❌ |
| 文件版本管理 | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 批量操作 | ✅ | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 云端下载 | ✅ | ❌ | ✅ (Aria2) | ❌ | ❌ | ❌ | ❌ |
| 服务端存档 | ✅ | ❌ | ✅ | ❌ | ✅ (zip) | ❌ | ❌ |
| Web UI | ✅ | ✅ | ✅ | ✅ | ✅ | ❌ (dashboard) | ❌ |
| CLI 客户端 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ (frpc) | ✅ |
| Go SDK | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 加密隧道 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| 多路复用 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| 多传输协议 | ✅ (TCP/WS/QUIC/gRPC/WebRTC) | ❌ | ❌ | ❌ | ❌ | ✅ (TCP/UDP/HTTP/HTTPS) | ✅ (TCP/UDP) |
| 中继网络 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ✅ |
| P2P 直连 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| DHT 发现 | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| ECDH PFS | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 重放保护 | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 多用户 | ✅ (API keys) | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 读写权限 | ✅ | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 速率限制 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| CORS | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| Prometheus 指标 | ✅ | ❌ | ❌ | ❌ | ❌ | ✅ | ❌ |
| 存储配额 | ✅ | ❌ | ✅ | ❌ | ❌ | ❌ | ❌ |
| 多存储后端 | ❌ | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ |
| WebDAV | ❌ | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 文件预览 | 基础 | ✅ | ✅ | ✅ | ❌ | ❌ | ❌ |
| 用户管理 | ❌ | ❌ | ✅ | ✅ | ❌ | ❌ | ❌ |
| ACME 自动证书 | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| mTLS | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ | ❌ |
| 认证方式 | Bearer + API Keys | 表单 | 表单/OAuth | 表单/OAuth | 无 | Token/OIDC | 密钥 |
| 部署 | 单二进制 | 单二进制 | 单二进制 | 单二进制 | Node.js | 双二进制 | 单二进制 |
| 外部依赖 | yaml.v3 + golang.org/x/* | 少量 | Gin + ent + 多 | 较多 | 较多 | 少量 | 极少 |
| 测试覆盖率 | 高（37k 测试代码） | — | — | — | — | — | — |

## 三、sproxy 与各类方案的详细对比

### 3.1 vs 文件管理类（File Browser / Cloudreve / Filestash）

#### 优势

1. **加密隧道深度集成**：sproxy 的隧道层不是"可选附加"，而是与文件操作 API 深度融合。sclient 默认通过隧道访问所有 API，提供传输层加密 + 认证。
2. **传输层可插拔**：支持 TCP/WS/QUIC/gRPC/WebRTC 五种传输协议。这在文件管理类方案中独一无二。
3. **中继 + P2P 网络**：Hub 中继 + Kademlia DHT + WebRTC P2P，使文件服务可以跨越 NAT 部署，无需公网 IP。
4. **SDK 优先设计**：pkg/client 提供完整的 Go SDK，第三方 Go 程序可直接 import 使用，这在文件管理类方案中少见。
5. **零外部依赖**：核心依赖仅 yaml.v3 + golang.org/x/*，极简依赖树。
6. **Checksum 全链路验证**：每个文件操作都强制 SHA-256 校验，防止数据静默损坏。
7. **文件版本管理**：上传前自动备份旧版本，支持版本列表、恢复和删除。

#### 劣势

1. **无多存储后端**：仅支持本地文件系统，Cloudreve/Filestash 支持 S3/OSS/OneDrive 等十余种后端。
2. **无用户管理**：无注册/登录/用户组/多租户，仅 API keys 权限控制。
3. **无文件预览**：Web UI 仅基础列表，无图片/视频/文档在线预览。
4. **Web UI 功能简陋**：相比 File Browser 的成熟 UI 和 Cloudreve 的完整界面，sproxy 的 Web UI 功能有限。
5. **无 WebDAV 协议**：无法通过 WebDAV 挂载为网络驱动器。
6. **无离线下载集成**：Cloudreve 集成 Aria2/qBittorrent，sproxy 的云端下载仅支持 HTTP(S) URL。
7. **社区规模小**：单项目，无社区贡献者，无生态插件。

### 3.2 vs 隧道/代理类（frp / Chisel / ngrok）

#### 优势

1. **文件服务 + 隧道一体化**：frp/Chisel 仅做隧道，不提供文件管理。sproxy 在一个端口中同时提供文件服务和隧道转发。
2. **ECDH PFS + 重放保护**：frp 和 Chisel 使用静态预共享密钥或 SSH 加密，sproxy 额外提供 ECDH 完美前向保密和 IAT/JTI 重放保护。
3. **传输层可插拔**：frp 支持 TCP/UDP/HTTP/HTTPS，Chisel 支持 TCP/UDP。sproxy 支持以上全部 + WebSocket/QUIC/gRPC/WebRTC。
4. **虚拟流多路复用**：mux 层提供完整的流管理（打开/关闭/心跳/重传），比 frp 的 channel 模型更精细。
5. **DHT 分布式发现**：Kademlia DHT 使节点可以无需中心化注册即可发现彼此，frp 和 Chisel 依赖中心服务器。
6. **Go SDK**：pkg/client 提供客户端 SDK，frp 和 Chisel 仅提供 CLI 二进制。
7. **Prometheus 指标**：标准 Prometheus 格式，frp 有 dashboard 但无标准指标。

#### 劣势

1. **隧道性能未优化**：sproxy 的隧道经过多层的序列化（xfer → mux → tunnel → HTTP），相比 frp 的纯 TCP 转发有额外开销。
2. **无 UDP 隧道**：sproxy 隧道聚焦 HTTP 请求-响应，不支持 UDP 协议转发。frp 支持 UDP，Chisel 也支持 UDP。
3. **无 SOCKS5 代理**：Chisel 内置 SOCKS5 代理，sproxy 无此功能。
4. **无 HTTP 反向代理**：frp 支持 HTTP/HTTPS 反向代理（域名/路径路由），sproxy 的隧道是通用加密通道，不提供反向代理能力。
5. **无负载均衡**：frp 支持后端服务负载均衡，sproxy 无此功能。
6. **无健康检查**：frp 支持后端健康检查自动摘除，sproxy 无此功能。
7. **无 Dashboard UI**：frp 的 Server Dashboard 提供实时状态可视化，sproxy 仅有 /metrics 文本端点。
8. **社区差距大**：frp 90k stars，Chisel 13k stars，sproxy 是内部项目。

### 3.3 vs 文件分享类（PsiTransfer）

#### 优势

1. 功能完整性远超 PsiTransfer（文件管理、版本控制、云端下载、隧道等）
2. Go SDK 可供程序调用
3. 加密隧道 + 认证机制

#### 劣势

1. PsiTransfer 的上传体验更简单（无需认证，一条 URL 即可分享）
2. PsiTransfer 的 tus.io 协议支持更标准化的断点续传
3. PsiTransfer 无需安装客户端，纯浏览器操作

## 四、sproxy 的独特优势汇总

### 4.1 唯一性能力（其他开源方案均不具备）

1. **文件服务 + 加密隧道 + 中继网络三合一**
2. **五种传输协议可插拔**（TCP/WS/QUIC/gRPC/WebRTC）
3. **ECDH PFS + 重放保护**的隧道加密
4. **Kademlia DHT 分布式节点发现**
5. **P2P 直连**（DHT 发现 + WebRTC 传输 + mux 多路复用）
6. **Checksum 全链路强制验证**（上传/下载/删除/重命名/批量操作）

### 4.2 差异化优势

| 维度 | sproxy 优势 |
|------|------------|
| **部署复杂度** | 单二进制，零外部依赖，5 秒启动 |
| **安全深度** | 传输加密（隧道）+ 访问控制（Bearer/API Keys）+ 数据完整性（SHA-256）+ 路径安全（多级校验）+ 防重放（IAT/JTI）+ PFS（ECDH） |
| **可编程性** | Go SDK + CLI + REST API 三种接入方式 |
| **网络穿透** | 无需公网 IP：Hub 中继 + DHT 发现 + WebRTC P2P |
| **测试质量** | 37k LOC 测试代码，覆盖率门禁，模糊测试，E2E 测试，-race 全开 |
| **依赖管理** | 核心仅 3 个外部依赖，审计成本极低 |

## 五、sproxy 的短板与风险

### 5.1 功能缺失

| 缺失能力 | 影响 | 优先级 |
|----------|------|--------|
| **多存储后端** | 无法使用 S3/OSS/OneDrive 等云存储 | 高 |
| **用户管理与多租户** | 无法支持多用户场景 | 高 |
| **文件预览（图片/视频/文档）** | Web UI 体验远不如竞品 | 中 |
| **WebDAV 协议** | 无法挂载为网络驱动器 | 中 |
| **离线下载（BT/磁力链）** | 云端下载仅支持 HTTP(S) | 中 |
| **Web UI 功能完整性** | 无批量操作界面、无版本管理界面 | 中 |
| **国际化（i18n）** | 仅中文/英文混合 | 低 |
| **移动端适配** | Web UI 未针对移动端优化 | 低 |

### 5.2 架构风险

| 风险 | 描述 |
|------|------|
| **隧道性能开销** | 多层封装（xfer → mux → tunnel → HTTP）带来额外延迟，大文件传输性能不如纯 TCP 转发 |
| **mux 层复杂度** | 帧协议 + 心跳 + 重传 + 流管理，潜在的死锁/竞态风险 |
| **Hub 单点故障** | 中继网络依赖中心 Hub，Hub 故障导致所有中继节点失联 |
| **内存存储的分享链接** | ShareStore 纯内存，重启丢失 |
| **配置热重载有限** | 仅部分"软配置"支持 SIGHUP 重载 |
| **全局包级变量** | 部分包（如 cmd/sproxy）仍有包级全局变量，测试隔离成本高 |

### 5.3 生态风险

| 风险 | 描述 |
|------|------|
| **无社区** | 单开发者项目，无外部贡献者 |
| **无插件系统** | 所有功能内置，不可扩展 |
| **无标准化协议** | 隧道协议为自定义帧格式，无第三方兼容实现 |
| **文档不足** | API 文档较好，但架构文档、部署指南、最佳实践不完整 |

## 六、适用场景分析

### 6.1 sproxy 最适合的场景

| 场景 | 说明 |
|------|------|
| **内网文件服务 + 公网访问** | 通过 Hub 中继或 WebRTC P2P 穿透 NAT，无需公网 IP 即可提供文件服务 |
| **安全文件传输通道** | 需要加密隧道 + 完整性校验的场景（如跨网络传输敏感数据） |
| **自动化文件处理管道** | 通过 Go SDK 集成到自动化流程中（CI/CD 产物分发、日志收集等） |
| **多节点文件分发网络** | 利用 Hub + DHT 构建小型文件分发网络 |
| **嵌入式文件服务** | 需要将文件服务嵌入到现有 Go 应用中（import pkg/client） |
| **开发/测试环境** | 需要快速搭建文件服务 + 隧道进行测试 |
| **个人/小团队文件同步** | 简单的上传/下载/分享需求 |

### 6.2 不适合的场景

| 场景 | 不适合原因 | 推荐替代 |
|------|------------|----------|
| **企业级文件管理平台** | 无用户管理、无多存储后端、无审计日志 | Cloudreve / Filestash |
| **公网文件分享服务** | 无注册机制、无文件预览、Web UI 简陋 | PsiTransfer / File Browser |
| **高性能反向代理** | 隧道性能不如纯 TCP 转发，无负载均衡 | frp / nginx |
| **SOCKS5 代理** | sproxy 不支持 SOCKS5 | Chisel / shadowsocks |
| **大规模中继网络** | Hub 单点，无集群化部署 | frp（支持多 frpc 连接同一 frps） |
| **多协议隧道**（非 HTTP） | 隧道聚焦 HTTP 请求-响应 | frp（支持 TCP/UDP/HTTP/HTTPS） |

### 6.3 场景-方案匹配矩阵

```
场景                     sproxy  FileBrowser  Cloudreve  Filestash  frp  Chisel
─────────────────────────────────────────────────────────────────────────────
个人文件管理               ★★★     ★★★★        ★★★★★     ★★★★      -    -
团队文件协作               ★★      ★★★         ★★★★★     ★★★★      -    -
内网穿透 + 文件服务        ★★★★★   -           -          -          ★★★  ★★
安全隧道转发              ★★★★    -           -          -          ★★★★ ★★★★
自动化文件处理管道         ★★★★★   -           -          -          -    -
P2P 文件传输              ★★★★    -           -          -          ★★   -
企业云盘                  -       -           ★★★★★     ★★★★      -    -
公网文件分享              ★★      ★★★         ★★★★      ★★★       -    -
NAT 穿透代理              ★★★     -           -          -          ★★★★★ ★★★★
开发/测试基础设施          ★★★★    ★★★         ★★★       ★★★       ★★★★ ★★★
嵌入式文件服务             ★★★★★   -           -          -          -    -
```

## 七、sproxy 未来发展方向

### 7.1 短期（1-3 个月）

| 方向 | 具体措施 | 优先级 |
|------|----------|--------|
| **修复架构风险** | mux 层 goroutine 泄漏修复、ShareStore 持久化、热重载范围扩大 | P0 |
| **Web UI 增强** | 图片预览、文件搜索 UI、版本管理 UI、批量操作界面 | P1 |
| **配置管理完善** | 扩大 SIGHUP 热重载范围、配置校验增强 | P1 |
| **文档完善** | 部署指南、最佳实践、架构决策记录（ADR） | P1 |
| **性能优化** | 隧道层减少内存拷贝、mux 帧批处理 | P2 |

### 7.2 中期（3-6 个月）

| 方向 | 具体措施 | 优先级 |
|------|----------|--------|
| **多存储后端** | 抽象 StorageBackend 接口，支持 S3/MinIO 兼容存储 | P0 |
| **用户管理** | 基础用户注册/登录、Session 管理、RBAC 权限模型 | P1 |
| **WebDAV 协议支持** | 通过 WebDAV 挂载为网络驱动器，提升文件管理体验 | P1 |
| **文件预览** | 图片缩略图、视频转码流式播放、文档在线预览 | P1 |
| **离线下载** | 集成 Aria2 或实现 BT/磁力链下载器 | P2 |
| **国际化** | 中英文 i18n 支持 | P2 |

### 7.3 长期（6-12 个月）

| 方向 | 具体措施 | 前景 |
|------|----------|------|
| **插件系统** | 基于 Go plugin 或 WASM 的插件架构，支持第三方扩展 | 从"单一项目"向"平台"演进 |
| **Hub 集群化** | Hub 多实例 + 一致性哈希 + 故障转移，消除单点 | 支撑大规模中继网络 |
| **标准化协议** | 提交隧道协议规范 I-D 或开源标准，促进第三方实现 | 建立生态基础 |
| **移动端 App** | 基于 FileClient SDK 的移动端应用 | 扩大使用场景 |
| **FUSE 挂载** | 通过 FUSE 将 sproxy 文件系统挂载为本地目录 | 提升文件操作体验 |
| **AI 集成** | 智能文件分类、OCR 图片文字提取、自动标签 | 面向未来 |

### 7.4 战略定位建议

sproxy 不应试图与 Cloudreve 或 File Browser 在"文件管理"领域正面对抗，也不应试图与 frp 在"反向代理"领域竞争。sproxy 的独特价值在于 **"文件服务 + 安全隧道 + 网络穿透"的三合一整合**。

**建议定位：**
> 面向开发者和自动化场景的安全文件传输基础设施

**核心差异化策略：**

1. **深耕 Go SDK 体验**：使 pkg/client 成为 Go 生态中文件传输的事实标准 SDK
2. **强化安全深度**：持续投入加密、校验、防篡改能力，成为"最安全的开源文件传输方案"
3. **简化部署体验**：极致化的单二进制部署 + 零配置启动 + 自动证书
4. **专注自动化场景**：CI/CD 管道、容器编排、微服务文件交换

## 八、关键数据

| 指标 | sproxy | FileBrowser | Cloudreve | Filestash | frp | Chisel |
|------|--------|-------------|-----------|-----------|-----|--------|
| 代码行数（Go） | ~24k | — | — | — | — | — |
| 测试代码行数 | ~37k | — | — | — | — | — |
| 外部依赖数 | 3 | — | 多 | 多 | 少量 | 极少 |
| 子模块数 | 7（含 ext） | 1 | 1 | 1 | 2 | 1 |
| 传输协议数 | 5 | 1(HTTP) | 1(HTTP) | 1(HTTP) | 4 | 2 |
| 启动时间 | <1s | <1s | <1s | <1s | <1s | <1s |
| 二进制大小 | ~15MB | ~10MB | ~20MB | ~15MB | ~10MB | ~8MB |
| 许可证 | Apache-2.0 | Apache-2.0 | GPL-3.0 | AGPL-3.0 | Apache-2.0 | MIT |

## 九、跨国/跨墙场景分析

跨墙场景中，协议选择由 **流量特征的可识别性** 决定（DPI 检测 + 主动探测），而非技术指标。

### 9.1 各协议跨墙适用性

| 协议 | GFW 识别难度 | 流量伪装度 | CDN 兼容 | 推荐度 |
|------|-------------|-----------|----------|--------|
| **TCP 隧道**（POST /tunnel） | ❌ 易识别（自定义 Content-Type + 固定帧长密文） | ❌ 低 | ✅ 好 | ⭐ |
| **WS over TLS（WSS）** | ✅ 难识别（大量 Web 应用使用 WS，流量混杂） | ✅ 高 | ✅ 非常好 | ⭐⭐⭐⭐ |
| **QUIC over UDP** | ⚠️ 中等（Google/Cloudflare 大量使用，但国内 UDP 限速） | ⚠️ 中 | ⚠️ 部分 | ⭐⭐⭐ |
| **gRPC** | ❌ 易识别（Content-Type 和 HTTP/2 SETTINGS 帧指纹明显） | ❌ 低 | ✅ 好 | ⭐ |
| **WebRTC** | ✅ 难识别（UDP 流量与视频通话无法区分） | ✅ 高 | ❌ 不支持 | ⭐⭐⭐ |

### 9.2 CDN 前置比协议选择更重要

```
无 CDN 前置：
  客户端 ──→ 服务器 IP（直接暴露）→ GFW 易针对性封锁

有 CDN 前置（推荐）：
  客户端 ──→ Cloudflare（SNI 显示 CDN 域名）──→ 服务器 → 极难封锁
```

sproxy 已有 TLS + ACME 自动证书 + WebSocket 传输能力，可配合 CDN 使用。

### 9.3 跨墙最佳实践

- **推荐方案**：WebSocket over TLS（WSS）+ Cloudflare 前置
  - WS 是唯一有消费端、有人维护的 ext 传输
  - CDN 前置后 SNI 不可见，流量特征与正常 WebSocket 无法区分
  - 端口 443 标准 HTTPS 端口，自定义 WS 路径
- **不应做的**：纯 TCP 隧道过墙（DPI 特征明显）、gRPC 过墙（指纹太明显）、自实现协议混淆层（GFW 跟进封锁快于迭代）

### 9.4 关联文档

详细分析见 [sproxy 架构分析与协议选择](./2026-08-04-sproxy-architecture-protocol-selection.md#五跨国跨墙场景协议分析)。

## 十、附录

### 9.1 分析范围

本分析覆盖以下开源项目的最新版本（截至 2026-08-04）：

- **File Browser**：v2.31.2（已归档）
- **Cloudreve**：v3.8.3
- **Filestash**：latest
- **PsiTransfer**：v2.1.1
- **frp**：v0.61.1
- **Chisel**：v1.10.1
- **ngrok**：v3（商业）

### 9.2 参考来源

- sproxy 代码仓库：`github.com/cocomhub/sproxy`
- 各项目 GitHub 页面及 README 文档
- sproxy 项目 docs/ 目录下的架构、API、配置文档

### 9.3 更新维护

本文档应随 sproxy 功能迭代和开源生态变化定期更新，建议每季度至少复审一次。