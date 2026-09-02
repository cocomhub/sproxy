# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 上级目录 `../CLAUDE.md` 与 `../AGENTS.md` 为工作区通用指南（中文回复、UTF-8 无 BOM、SPDX 许可证头、最小改动等），全部适用于本子项目；以下内容仅补充 sproxy 专属要点，与上级冲突时以本文件为准。

## 依赖策略

- **Go 标准库优先**：功能可用标准库实现时优先使用标准库。
- **`golang.org/x/` 系列**：Go 团队维护的准标准库（如 `golang.org/x/crypto`、`golang.org/x/sys`、`golang.org/x/net` 等）可自由使用，无需额外评审。
- **第三方库**：新增非 `golang.org/x/` 的第三方依赖需审慎评估，优先选择纯 Go 实现、API 稳定、社区活跃的库。

`github.com/cocomhub/sproxy` 是一个**轻量文件上传/下载/删除服务 + 加密隧道**，附带 `sclient` 客户端二进制。Go 1.26，依赖（新增）`github.com/spf13/cobra`、`github.com/spf13/viper`、`github.com/adrg/xdg` + `gopkg.in/yaml.v3` + `golang.org/x/sys` + `golang.org/x/crypto`。

> 历史：早期版本曾包含 `/{host}/{filepath...}` HTTPS 透明转发与 `/bandwidth` 端点，已于重构移除，定位收敛为文件服务 + 隧道。

## 执行偏好

- **子代理开发**：多步骤实现计划优先使用 `subagent-driven-development` 技能，禁用 worktree，直接在当前分支开发。
- **worktree**：除非用户明确要求，不使用 git worktree。

## 工程原则（必须遵守）

1. **永不允许 lint 错误**：只要发现 lint 问题（任何模块，含改动前已存在的历史遗留），就必须修复，不得以"改动前就有"为由跳过。提交前全模块 lint（主 go.mod + 每个子 go.mod）必须 0 issues。
2. **cmd 避免复杂逻辑**：cobra 命令处理保持薄（flag 解析 + 调用 + IO 展示）。非命令行纯逻辑，若值得复用→抽独立 pkg；若不值得抽 pkg→放 cmd 内的 `internal/` 内部包，不留在 `package main`。
3. **抽象先薄包装委托保障一致**：逻辑下沉 pkg 时，先让 cmd 用**薄包装委托**新抽象并通过全量测试验证功能一致性/可靠性；随后**最终直接用新抽象，不保留薄包装委托**（薄包装是过渡，不是最终形态）。
4. **有价值测试场景在抽象中仍覆盖**：抽象后，原 cmd 测试中有价值的场景必须在抽象包里有等价测试（不能因"逻辑搬走了"而丢失覆盖）；抽象包测试是功能一致性的最终保障。

## 常用命令

```bash
make build           # 本地构建（含格式化）
make build-sproxy    # 只构建 sproxy（模式：build-<cmd-name>）
make build-sclient   # 只构建 sclient
make build-ci        # CI 构建（跳过格式化）
make test            # 快速单元测试（已取消 vet/check-loopback 依赖）
make test-cover      # 测试 + 覆盖率收集
make test-packages   # 分组运行测试，快速定位失败包
make test-all        # 测试所有子 module（含 ext/ws、ext/quic、ext/grpc 等）
make build-all       # 构建所有子 module
make cover-check     # 覆盖率门禁检查（默认 70%）
make cover-html      # 覆盖率 HTML 报告到 build/coverage/cover.html
make cover-trend     # 覆盖率趋势追踪
make vet             # go vet
make lint            # golangci-lint
make bench           # 基准测试（-count=5，含数据目录追踪）
make bench-compare   # 比较最近两次 benchmark 结果
make check-loopback  # 检查测试地址是否使用不安全监听
make notest          # 检查所有包有测试文件（.notestignore 控制免检）
make gofix           # go fix ./...
make fmt             # addlicense + go fix + gofmt -s
make clean           # 删除 build 目录
make check-ci        # 全量检查入口（提交前使用）
make addlicense      # 仅注入 SPDX 头（不格式化）
make sonar-analyze   # SonarQube Cloud 分析
make sonar-remediate # SonarQube Cloud 修复
make tools           # 安装构建工具（addlicense、benchstat）
make githooks        # 安装 git hooks
make run             # build + 用 build/config.yaml 运行 sproxy
make show-version    # 打印当前构建二进制的版本

Windows 首次运行需安装 make：
  pwsh scripts/install-make.ps1

所有 CI job 通过 `make <target>` 调用，不写裸 go 命令。
```

`addlicense` 由 `make fmt` 强制注入 SPDX 头；本地缺失时：`go install github.com/google/addlicense@latest`。

版本元数据通过 `-ldflags "-X main.Version=... -X main.BuildAt=..."` 注入到 `cmd/sproxy/main.go`、`cmd/sclient/main.go` 中的 `Version` / `BuildAt` 包级变量，**不要手工改这些常量**。

### 单测技巧

```bash
# 运行单个包测试（默认已开启 -race）
go test -count=1 ./pkg/server/...

# 运行单个测试函数
go test -count=1 -run TestValidateFilePath ./pkg/server/...

# 子 module 测试需 cd 进入对应目录
cd pkg/tunnel/xfer/ext/ws && go test ./...

# 覆盖率（排除 test/ tools/ 稀释）
go test -coverprofile=cover.out ./internal/... ./pkg/... ./cmd/...
```

## 多 module workspace

根 `go.work` 组合了以下独立 `go.mod` 模块（均需 `go.work` 或 `replace` 才能联动构建）：

| 模块路径 | 说明 |
|----------|------|
| `.` | 核心库（`go.mod`，`gopkg.in/yaml.v3` + `golang.org/x/sys` + `golang.org/x/crypto`） |
| `./cmd/sproxy` | sproxy 服务端二进制（cobra+viper，replace 指向根 module） |
| `./cmd/sclient` | sclient 客户端二进制（cobra+viper+xdg，replace 指向根 module） |
| `./pkg/tunnel/xfer/ext/ws` | WebSocket 传输层子模块（独立的 go.mod） |
| `./pkg/tunnel/xfer/ext/quic` | QUIC 传输层子模块 |
| `./pkg/tunnel/xfer/ext/grpc` | gRPC 传输层子模块 |
| `./pkg/tunnel/xfer/ext/webrtc` | WebRTC 传输层子模块 |
| `./pkg/tunnel/hub/ext/kad` | Kademlia DHT 路由表扩展子模块 |

构建/测试所有模块：`make build-all` / `make test-all`。单模块操作需 cd 进入目录。

## 仓库结构

```
cmd/
  sproxy/   # 服务端：root.go（cobra 入口）+ main.go（版本变量）
  sclient/  # 客户端：多文件组织（按子命令拆分）
    cd.go, upload.go, download.go, delete.go, list.go, stat.go
    tunnel.go, relay.go, genkey.go, config.go
    batch.go, batch_delete.go, batch_rename.go
    cloud_download.go, search.go, archive.go, diag.go, mv.go
    version.go, errors.go, output.go, root.go
    internal/sclientcfg/  # viper 配置提供者
pkg/
  server/            # 核心服务逻辑：Config / Handlers / ChecksumStore
                     # UploadStore / RateLimiter / auth / validate
                     # cloud_download / downloader/ / storage_manager
                     # archive / share / versioning
  client/            # FileClient Go SDK + chunked upload/download
  tunnel/            # AES-256-GCM 加密隧道 + 分层传输架构
    tunnel.go           # 传统隧道模式（NewHandler, Client.Do）
    tunnel_mux.go       # 多路复用隧道模式（NewTunnel, Tunnel.Do/Serve）
    handler_client.go   # 客户端 handler 实现
    stream.go           # 流式读写
    mux/                # 虚拟流多路复用器（Stream RWC + 帧协议 + 心跳 + 重传）
    hub/                # 星型中继：RouteTable / 节点注册 / 中继转发
      ext/kad/          # Kademlia DHT 路由表扩展（独立 go.mod）
    p2p/                # 点对点直连（P2PConn + 中继穿透）
    xfer/               # 传输层抽象（Conn{ Send/Receive/Close }）
      internal/tcp/     # TCP 传输实现（内置）
      ext/ws/           # WebSocket 传输（独立 go.mod）
      ext/quic/         # QUIC 传输（独立 go.mod）
      ext/grpc/         # gRPC 传输（独立 go.mod）
      ext/webrtc/       # WebRTC 传输（独立 go.mod）
      xfertest/         # 跨传输实现的测试工具套件
    tracing/            # 分布式追踪（span + slog 集成）
  plugin/            # 可插拔组件注册表
  provider/          # 配置提供者抽象（用于 viper 解耦）
  testutil/          # 跨包测试辅助工具
    mockserver/         # mock HTTP server
    mockdht/            # mock DHT
    mockxfer/           # mock xfer.Conn
internal/
  shortid/           # 短 ID 生成（base62，6-12 字符）
  size/              # 人类可读字节大小解析（"1GiB" → int64）
web/static/          # 嵌入式 Web UI（index.html，支持子目录浏览）
test/                # 端到端测试（构建真实二进制 + 子进程启动）
tools/               # 开发工具（gencoverview, genbenchview, genreport, gentimingview）
certs/               # 测试用证书
config.example.yaml  # 参考配置
```

## 分层传输架构（`pkg/tunnel/`）

sproxy v2 引入了可插拔传输层抽象，从下到上共 4 层（详见 `docs/architecture.md`）：

```
应用层: HTTP 路由 + sclient CLI + FileClient Go SDK
  ↑  hub 层: 节点注册 / 路由表 / 流中继 (RouteTable / RelayStreamHandler)
  ↑  tunnel 层: HTTP 请求-响应交换 (Tunnel.Do/Serve, AES-256-GCM)
  ↑  mux 层: 虚拟流多路复用 (Stream RWC + 心跳 30s/90s)
  ↑  xfer 层: 传输层抽象 (Conn{ Send/Receive/Close })
       ├── TCP (内置, xfer/internal/tcp)
       ├── WebSocket (xfer/ext/ws, 独立 module)
       ├── QUIC (xfer/ext/quic, 独立 module)
       ├── gRPC (xfer/ext/grpc, 独立 module)
       └── WebRTC (xfer/ext/webrtc, 独立 module)
```

**关键接口（`pkg/tunnel/xfer/core.go`）：**

```go
type Conn interface {
    Send(ctx context.Context, msg []byte) error
    Receive(ctx context.Context) ([]byte, error)
    io.Closer
}
```

任何传输层只需实现这 3 个方法，通过 `xfer.Register()` 即可接入上层复用系统。

## 关键路由（`pkg/server/handlers.go`）

`RegisterRoutes` 在 `cmd/sproxy/root.go` 中挂到 `http.NewServeMux`。支持两层认证：主 mux 走 SproxySig 请求签名（`authMiddleware`，配置 `access_keys` 时启用；`api_keys` 仍走独立 Bearer 多用户模式），`localMux` 走隧道密钥（`POST /tunnel` 内部路由时跳过认证）。

### 基础
- `GET /` — 301 重定向到 `/ui/`
- `GET /ui/` — 嵌入式 Web UI 静态文件（CSP: default-src 'self'）
- `GET /healthz` — 文本 `OK`
- `GET /version` — 文本 `Version: x\nBuildAt: y`
- `GET /metrics` — Prometheus 风格的 metrics

### 文件操作（需 `X-File-Checksum` 头）
- `POST /upload` — multipart 字段名 `file`，文件名通过 `ValidateFilePath` 校验，支持子目录路径
- `GET /download?filename=<name>` — `ValidateFilePath` 校验防穿越；支持 `Range` header
- `POST /delete?filename=<name>` — 匹配 checksum后才删
- `POST /rename?from=<old>&to=<new>` — 重命名/移动文件

### 目录操作
- `POST /mkdir?dirname=<name>` — 创建空目录
- `POST /rmdir?dirname=<name>` — 删除空目录

### API
- `GET /api/files?subdir=path` — JSON `{files: [{name, size, checksum, mod_time, is_dir}]}`
- `HEAD /api/files/stat?filename=<name>` — 单文件元信息（响应头）
- `GET /api/files/search?q=<query>&subdir=<subdir>` — 文件名搜索（子字符串匹配）
- `POST /api/batch/delete` — 批量删除（JSON body: `{files: [...]}`）
- `POST /api/batch/rename` — 批量重命名（JSON body: `{operations: [{from, to}]}`）

### 分块上传/下载
- `POST /upload/init` — 初始化分块上传会话
- `POST /upload/chunk` — 上传一个分块
- `GET /upload/status?upload_id=<id>` — 查询分块上传进度
- `POST /upload/complete?upload_id=<id>` — 完成分块上传
- `GET /download/chunk?filename=<name>&offset=<n>&size=<n>` — 分块下载

### 文件版本管理（需配置 `versioning.enabled: true`）
- `GET /api/versions?filename=<name>` — 列出版本历史
- `POST /api/versions/restore?filename=<name>&version=<id>` — 恢复指定版本
- `DELETE /api/versions?filename=<name>&version=<id>` — 删除指定版本

### 文件分享
- `POST /api/share` — 创建分享链接（JSON body: `{filename, password?, expire_in?}`）
- `GET /s/{token}` — 通过分享 token 访问文件

### 云端下载
- `POST /api/cloud/download` — 创建云端下载任务
- `POST /api/cloud/download/batch` — 批量创建云端下载任务
- `GET /api/cloud/tasks` — 列出云端下载任务
- `GET /api/cloud/tasks/{id}` — 查询单个任务
- `POST /api/cloud/tasks/{id}/cancel` — 取消任务
- `DELETE /api/cloud/tasks/{id}` — 删除任务

### 存档（archive 压缩/解压缩）
- `POST /api/archive` — 创建存档任务（压缩/解压缩）
- `GET /api/archive-dir` — 获取可存档目录列表

### 统计 & 存储
- `GET /api/stats` — 服务端统计信息
- `PUT /api/storage/config` — 更新存储配置（动态调整 max_storage_bytes）

### Hub 中继管理（需配置 `hub.enabled: true` + `RouteTable`）
- `GET /api/hub/nodes` — 列出已注册节点
- `DELETE /api/hub/nodes/{id}` — 移除节点
- `GET /api/hub/stats` — Hub 统计

### 隧道
- `POST /tunnel` — `tunnel.NewHandler(key)`，AES-256-GCM 加密的请求转发

## 配置（`pkg/server/config.go`）

### 加载方式（viper，来自 `cmd/sproxy/root.go`）

1. 默认值（`Default()`）
2. 配置文件 YAML（`--config` 指定，默认 `sproxy.yaml`）
3. 环境变量（前缀 `SPROXY_`，如 `SPROXY_ADDR`、`SPROXY_STORAGE_ROOT`）
4. CLI 标志（`--addr`、`--storage-root`、`--tunnel-key`）

优先级：CLI 标志 > 环境变量 > 配置文件 > 默认值。

配置**文件不存在时**：不报错，仅使用默认值+flag/env 覆盖（不再自动创建默认配置文件）。

`LoadConfig(path)` 函数保留用于测试兼容，不由新 CLI 调用。

### 完整配置字段

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `addr` | string | `:18083` | 监听地址 |
| `storage_root` | string | `./storage` | 多租户存储根（`<tenant>/{user,cloud,archive,chunk,version,meta}/` 桶布局） |
| `owner_quotas` | map[string]int64 | 空 | 按 owner 配额上限（显式 owner > `"*"` 默认 > 0 不限制） |
| `tunnel_key` | string | 空（自动生成） | 64 hex chars AES-256 密钥 |
| `log_level` | string | `info` | debug/info/warn/error |
| `log_format` | string | `text` | text/json |
| `max_header_bytes` | int | 1048576 | 最大 HTTP 头字节数 |
| `max_upload_bytes` | int64 | 1 GiB | 单次上传最大字节数 |
| `server_timeouts.read_header` | duration | `5s` | |
| `server_timeouts.read` | duration | `30s` | |
| `server_timeouts.write` | duration | `30s` | |
| `server_timeouts.idle` | duration | `60s` | |
| `server_timeouts.shutdown` | duration | `30s` | graceful shutdown 超时 |
| `tls.enabled` | bool | true | |
| `tls.cert_file` / `tls.key_file` | string | | |
| `tls.auto_tls` | bool | true | 自动生成 ECDSA P-256 自签证书 |
| `tls.client_ca` | string | | mTLS CA 证书路径 |
| `access_keys` | []AccessKey | 空 | SproxySig 请求签名认证（每 mesh 一对 AK/SK：`{key, secret, mesh_id?}`；配置后除 `/healthz`、`/version`、`/ui/`、`POST /tunnel` 外全 HTTP 面验签） |
| `api_keys.enabled` / `.keys` | | 关闭 | 多用户 API 密钥（独立 Bearer 特性，与 access_keys 互斥，优先） |
| `rate_limit.enabled` / `.requests` / `.window` | | 关闭 | tunnel handler 限流 |
| `chunk_size` | int | 4 MB | 分块上传每块大小 |
| `max_chunk_size` | int | 64 MB | 客户端最大分块大小 |
| `max_chunk_upload_bytes` | int | 8 MB | 服务端单块请求体上限 |
| `upload_session_ttl` | duration | 24h | 未完成上传会话过期时间 |
| `versioning.enabled` / `.max_versions` | | 关闭 | 文件版本管理 |
| `hub.enabled` / `.node_id` / `.relay_token` | | 关闭 | 中继 Hub 配置 |
| `hub.transports.ws.enabled` / `.listen`（预留，未消费）/ `.path`（已废弃，固定 `/ws`，非默认值仅告警忽略） | | 关闭 | WebSocket 传输 |
| `cors.allowed_origins` | []string | | CORS 配置 |
| `cloud_max_concurrent` | int | 3 | 云端下载并发数 |
| `cloud_sync_threshold` | size | 20MiB | 同步阈值（handler 提交时大小未知恒异步，字段保留供未来按大小同步） |
| `cloud_max_batch_urls` | int | 100 | 批量/组下载单次最大 URL 数；超过服务端返回 400 使创建失败 |
| `cloud_download_timeout` / `cloud_download_idle_timeout` | duration | 30m / 1m | 单次下载整体/空闲超时 |
| `cloud_max_retries` / `cloud_retry_delay` | int / duration | 10 / 10s | 瞬时失败重试 |
| `cloud_archive_max_bytes` | int64 | 0（不限） | 单次云归档原始文件大小总和上限；归档走 O_EXCL 防覆盖 + TryReserve 配额 |
| `provider.default` | string | | 云端下载提供者 |
| `provider.timeout` / `.retry` | | | 提供者超时/重试 |
| `max_storage_bytes` | int64 | 0（不限） | 存储上限 |

所有超时字段使用 Go duration 语法（`"30s"`、`"5m"`）。`tunnel_key` 必须是 64 个十六进制字符（32 字节 AES-256 密钥），否则启动失败。生成密钥：`sclient genkey`；生成 SproxySig AccessKey/AccessKeySecret：`sclient access-key create [--mesh <name>]`（输出 AK/SK 供服务端 `access_keys` 配置与客户端 `access_key`/`access_key_secret` 使用）。

SIGHUP 重载范围有限：仅 `log_level`/`log_format` 等"软配置"会生效；`addr`/`storage_root`/`tunnel_key`/`rate_limit`/`server_timeouts`/`max_header_bytes`/`access_keys`/`owner_quotas` 需要重启进程。

## 认证：SproxySig 请求签名（`pkg/sproxysig`）

服务端配置 `access_keys: [{key, secret, mesh_id?}]`（每 mesh 一对）后，除
`/healthz`、`/version`、`/ui/`、`POST /tunnel` 外的全部 HTTP 面（文件/信令/节点列表/
服务发现/网关/云端下载）走 **SproxySig v1 请求签名**，替代旧 `auth_token` 明文 Bearer。
`api_keys`（多用户 Bearer）与 access_keys 互斥，api_keys 优先。

- **协议头**：`Authorization: SproxySig v=1 ak=<AK> ts=<unix_ms> exp=<unix_ms> nonce=<16B hex> body_sha256=<hex|UNSIGNED> sig=<hex>`
- **签名**：`sig = HMAC-SHA256(SK, "sproxy-sig/v1\n" + ak + "\n" + ts + "\n" + exp + "\n" + nonce + "\n" + method + "\n" + path + "\n" + query + "\n" + body_sha256)`（canonical 为换行拼接，path 用 `EscapedPath()`，query 用 `RawQuery`）
- **服务端校验**：取 AK→SK、重算 sig 恒时比较、`now≤exp`、`exp−ts≤max_ttl(15min)`、nonce 防重放池；带 body 请求客户端**发送前预计算哈希**，服务端流式累加、EOF 比对（防篡改）
- **无 body 请求** `body_sha256 = sha256("")`（`sproxysig.EmptyBodyHash()`）
- 客户端（sclient/FileClient/mesh/relay/信令）统一 `--access-key`/`--access-key-secret`（或配置 `access_key`/`access_key_secret`），Secret 只存本端计算签名、永不上线
- Web UI 用 **WebCrypto** 计算 HMAC（`crypto.subtle`），AK/SK 存 `sessionStorage`（关页即清）；未配置 AK/SK 时不发签名头（无认证兼容）
- 生成 AK/SK：`sclient access-key create [--mesh <name>]`（AK=`sk[-<mesh>]-<16hex>`，SK=32B 随机 hex）

## sclient CLI（`cmd/sclient/`）

基于 **cobra** + **pflag**，无手动解析。子命令：

| 命令 | 用途 |
|------|------|
| `upload <file>...` | 上传文件，路径保留目录结构 |
| `download <filename> [output]` | 下载文件 |
| `delete <filename>` | 删除文件 |
| `batch <file>` | 从文件逐行读取命令批量执行 |
| `batch-delete <file>` | 批量删除（从文件读取文件名列表） |
| `batch-rename <file>` | 批量重命名（从文件读取 from/to 对） |
| `list` | 列出文件（支持 `--subdir`，受 `cd` 影响） |
| `stat <filename>` | 查询单文件元信息 |
| `search <query>` | 搜索文件名 |
| `mv <from> <to>` | 重命名/移动文件 |
| `archive <name> <path>...` | 创建归档 |
| `cloud-download <url>...` | 创建云端下载任务 |
| `tunnel [flags] <url>` | 隧道请求 |
| `relay start/status/...` | 中继节点（连接 Hub）：`relay start --hub wss://.../ws --token T --node-id N [--service name:addr] [--dial-allow] [--dial-allow-cidr CIDR]` |
| `relay dial --node <id> --tcp <addr> [-l :port]` | 经 hub 中继拨号到目标节点出口（任意 TCP） |
| `p2p connect --peer <id> --tcp <addr> [-l :port]` | WebRTC 打洞直连对端（数据面不经 hub） |
| `p2p listen [--node-id N]` | 作为对端监听 WebRTC 直连（信令经 hub） |
| `mesh connect <service> [-l :port]` | 连接 mesh 服务（webrtc 直连优先，hub 中继回落；`--gateway <addr>` 经本地 mesh node 网关复用已建直连链路） |
| `mesh status` | 列出 hub 上的 mesh 服务（`--gateway <addr>` 改查本地 mesh node 直连拓扑/链路类型） |
| `mesh node [flags]` | 单进程常驻 mesh 节点（注册+中继+webrtc 直连+自动对等发现+本地网关）：`--hub` `--node-id` `--token` `--service` `--dial-allow` `--discover` `--discover-interval` `--gateway-addr` |
| `access-key create [--mesh <name>]` | 生成一对 AccessKey/AccessKeySecret 打印（供服务端 `access_keys` 配置与客户端 `access_key`/`access_key_secret`） |
| `genkey` | 生成 64 hex 密钥 |
| `config [show\|set <k> <v>]` | 配置管理 |
| `diag` | 诊断连接问题 |
| `version` | 版本 + 配置信息 |
| `cd [path]` | 切换当前目录 |
| `pwd` | 打印当前目录 |

### mesh 内网穿透（双重 NAT 场景）

任意节点经 hub 注册后可互相寻址；数据面优先 webrtc 打洞直连、失败回落 hub 中继。

**服务宣告 + 访问**：
```bash
# 节点 A（被访问方）宣告本地服务，作为出口节点（--dial-allow 允许出站拨号）
sclient relay start --hub wss://hub:18083/ws --token T --node-id nodeA \
  --service ssh:127.0.0.1:22 --dial-allow

# 节点 B（访问方）连接服务：连接前自动注册自身（webrtc 信令用）；
# webrtc 直连优先，但需对端同时运行 p2p listen 且信令通过校验，否则默认回落 hub 中继
sclient mesh connect ssh -l :2222   # 然后 ssh -p 2222 user@127.0.0.1
```

**云端主动推数据到本地**（方向对称）：
```bash
# 本地端（被访问方）先跑：注册 + 宣告本地 2090 服务 + 允许出站拨号（B9 精确放行宣告地址）
sclient relay start --hub wss://hub:18083/ws --node-id local \
  --token T --insecure --dial-allow --service app:127.0.0.1:2090

# 云端节点：经 hub 中继拨本地端 2090 服务（SproxySig 需 --access-key/--access-key-secret，
# 与服务端 access_keys 一致；自签 TLS hub 需 --insecure）
sclient relay dial --node local --tcp 127.0.0.1:2090 \
  -s https://hub:18083 --access-key <AK> --access-key-secret <SK> --insecure
# 云端即可向该连接写入数据，数据经 hub 中继到达本地端服务
```
说明：`relay dial` 双向可用——任意节点可作 caller 拨向另一节点，实现云端→本地主动推送
（无需本地端先发起数据流）。本地端 `--service app:127.0.0.1:2090` 兼作 mesh 服务宣告与
出口拨号精确放行（`NewServiceDialPolicy`）；`--insecure` 仅用于自签证书开发/测试，生产用真实证书。

> 注意：`relay start --hub` 默认指向 `ws://127.0.0.1:18084/ws`，与 sproxy 默认监听端口
> `:18083` **不同**；请始终显式 `--hub` 指定实际 hub 地址（`ws://host:port/ws`）。
> 另：`relay start --hub` 传 WS 端点（`ws(s)://host/ws`）；`p2p` / `mesh connect --hub`
> 传 HTTP 基址（`http(s)://host`）即可，也接受 `ws(s)` 自动归一（S123）。

### mesh node 常驻 + 自动对等发现 + 完全服务互访

`mesh node` 取代 `relay start` 作为常驻出口节点（单进程单注册，稳定 node-id + 服务宣告 +
per-node secret + 断线指数退避重连），并叠加两层能力：

**自动对等发现（全节点互联）**：`--discover`（默认开）周期经 hub 节点列表发现其他 mesh
node，并行 webrtc 自动直连并保持（半拨号去重：低 ID 拨高 ID，每对一条链接），形成
full-mesh 拓扑。`--discover-interval` 控制发现周期（默认 10s）。

**本地网关 + 完全服务互访**：mesh node 恒监听 loopback 网关（`--gateway-addr`，默认
`127.0.0.1:18085`）。`mesh connect --gateway <addr>` 先经本地 mesh node 网关**复用已建立
的直连链路**路由到目标服务（零重新打洞），本地节点无到目标的已建链路时回落常规拨号
（webrtc 打洞 / hub 中继，不回归既有路径）。`mesh status --gateway <addr>` 查询本地节点
直连拓扑（node-id + 服务宣告 + 已建链路及链路类型 `webrtc-direct`）。

```bash
# 节点 node-svc（服务宿主）：宣告 echo 服务，自动对等发现开
sclient mesh node --hub ws://hub:18083/ws --token T --node-id node-svc \
  --service echo:127.0.0.1:2222 --dial-allow

# 节点 node-ap（访问方，低 ID）：自动拨号 node-svc，本地网关 127.0.0.1:18085
sclient mesh node --hub ws://hub:18083/ws --token T --node-id node-ap \
  --discover --discover-interval 10s --gateway-addr 127.0.0.1:18085

# 任一机器上：经 node-ap 网关复用已建直连链路访问 node-svc 的 echo
sclient mesh connect echo -l :2222 --gateway 127.0.0.1:18085
sclient mesh status --gateway 127.0.0.1:18085   # 直连拓扑 / 链路类型
```

> 说明：每条已建直连链路**两侧都注册进各自链路池**——拨号侧 `dialPeer` set + accept 侧
> （discovery 拨号临时身份 `disc-<base>-<unixnano>` 经 `parseDiscoveryPeerID` 恢复真实
> node ID 后注册，`removeIf` 防重连竞态），故**任意节点的网关都能双向路由**到任意已建
> 链路对端的服务（完全服务互访，半拨号去重仍保持每对一条链接）。拨号侧链路上同时跑
> `relay.Serve`（接受对端网关回拨）。
>
> **disc- 身份防冒充**：discovery 临时注册（`disc-<base>-<unixnano>`）由 hub 注册时强制
> 校验——base 必须等于 `real_node_id` 且持有该真实节点 per-node secret 的 HMAC 证明
> （fail-closed 拒绝，`hub.ParseDiscNodeID` 单一实现供注册校验与 accept 侧解析共用），
> 故 accept 侧解析的 base 即 hub 已验证、不可伪造；accept 侧另有半拨号序校验
> （`peerID<nodeID`）作纵深，根除冒充他人污染链路池的投毒/MITM。
>
> **网关安全边界**：网关仅监听 loopback（`--gateway-addr` 传非 loopback 地址会被
> fail-closed 拒绝——远程访问应经 mesh 本身而非网关，防被用作开放 mesh 中继）；mesh
> node 配置了 `access_key`/`access_key_secret`（SproxySig）时，网关对 connect/status
> 请求校验相同凭据（mesh connect/status 自动签名），未授权本地进程无法复用网关路由。

### sclient 当前目录（`cd`/`pwd`）

`cmd/sclient/cd.go` 提供工作目录概念：
- `cd <path>` 切换目录，后续 upload/download/list/delete 等命令以当前目录为基准
- `cd /` 回到根目录，`cd ..` 返回上级
- `cd` 无参打印当前目录
- `pwd` 打印当前目录
- 相对路径自动拼接 `currentDir`；`/` 开头的绝对路径绕过当前目录

### 配置路径

基于 XDG（`github.com/adrg/xdg`）：
- Linux: `~/.config/sproxy/sclient.yaml`
- macOS: `~/Library/Application Support/sproxy/sclient.yaml`
- Windows: `%LOCALAPPDATA%/sproxy/sclient.yaml`

旧路径 `~/.sclient.yaml` 读取并提示迁移。`--config` flag 可完全覆盖默认路径。

环境变量前缀 `SCLIENT_`（如 `SCLIENT_SERVER_URL`）。

**多环境**：`SCLIENT_ENV=prod` 时默认加载 `sclient.prod.yaml`（同一目录下），便于维护
prod/staging/dev 多套配置。**通用 mesh 配置键**（`sclient config set` 支持）：
- `server_url` — 服务器地址
- `access_key` / `access_key_secret` — SproxySig 认证 AK/SK（服务端配置了 `access_keys` 时需要；Secret 只本端计算签名，永不上线）
- `hub_url` — mesh/relay/p2p 共用 hub 地址（http(s)/ws(s)，可带 /ws 路径）
- `relay_token` — hub 中继注册 token（与 relay start --token / hub.relay_token 一致）
- `node_id` — 本节点默认 ID（为空回落主机名）

mesh connect / relay start / p2p / mesh node 的 `--hub`/`--token`/`--relay-token`/
`--node-id`/`--access-key`/`--access-key-secret` 未显式指定时按 `CLI flag > 配置文件 >
默认值` 回落（relay start 的 `--hub` 默认值已改为空，本地默认 `ws://127.0.0.1:18084/ws`
在 runRelayStart 内解析）。

## 多层级目录支持

- 所有 handler 使用 `ValidateFilePath`（`pkg/server/validate.go`）校验用户路径
- 允许 `/` 作为目录分隔符，拒绝 `..`（路径穿越）、绝对路径、空字节、Windows 非法字符
- 服务端自动 `os.MkdirAll(filepath.Dir(target))` 创建中间目录
- ChecksumStore 的 key 包含完整相对路径（如 `dir1/dir2/file.txt`）
- API 返回的 `name` 字段使用 `filepath.ToSlash` 格式
- `GET /api/files?subdir=path` 按层级查询，默认返回根目录顶层文件
- Web UI 支持面包屑导航进入/返回子目录
- sclient `cd` 命令记录当前工作目录

## tunnel 包要点（`pkg/tunnel/`）

- **传统模式**：`NewHandler(key)` / `NewLocalHandler(key, localMux)` → 标准 `http.Handler`，每个请求创建一个 HTTP POST
- **多路复用模式（推荐）**：`NewTunnel(mux, key)` → 在已有 mux 连接上创建隧道，`Tunnel.Do(req)` 通过虚拟流完成 HTTP 请求-响应交换
- AES-256-GCM + 随机 12 字节 nonce，nonce 前置于密文
- 统一帧协议（`application/x-tunnel-frame`）：`[4B BE metaLen][encrypted metadata][stream chunks...]`，其中 stream chunk = `[2B chunkLen][nonce|ciphertext|tag]`，默认 64 KB / chunk
- mux 层帧协议：`[4B StreamID][1B FrameType][1B Flags][2B PayloadLength][Payload...]`，帧类型含 `FrameData`/`FrameOpen`/`FrameClose`/`FrameCloseWrite`/`FramePing`/`FramePong`
- 心跳：30s Ping，90s 超时断开
- `UpdateKey` 支持运行时热替换密钥，旧密钥保留短时窗口供存量连接使用

## 编码与日志

- 日志统一 `log/slog`（Text 或 JSON handler，按 `log_format` 切换）；新代码不要混入 `zap` / `logrus`
- 中文文案禁止 GBK/ANSI；Windows 终端注意 UTF-8 输出，避免"文件正确但终端乱码"误判
- 错误优先 `fmt.Errorf("...: %w", err)` 包装；handler 内不要把原始 error 直接抛给客户端，使用 `UploadResponse{Success,Message}` JSON 格式回包

## 测试规范

### 测试工具集
跨包可复用的测试辅助函数位于 **`pkg/testutil/`**（`github.com/cocomhub/sproxy/pkg/testutil`）：
- `TestKey()` — 64 hex char AES-256 测试密钥
- `DiscardLogger()` — 输出到 io.Discard 的 slog.Logger
- `SHA256Hex(data []byte)` — SHA-256 → hex string
- `CaptureStdout(fn)` / `CaptureStderr(fn)` — 捕获 CLI 输出

放置在 `pkg/` 而非 `internal/`，以兼顾未来 cmd 独立为 go module 时的可达性。

更多测试辅助：
- **`pkg/server/server_test_common_test.go`** — server 包内共享（testKey, testLogger, withHeader）
- **`pkg/server/integration_test.go`** — `newTestServer` + `newTestServerWithAllRoutes` 等变体
- **`pkg/client/client_test.go`** — `newMockServer`（sproxy 兼容的 mock 服务端）
- **`test/e2e_test.go`** — `startSPROXY`（构建真实二进制并启动的端到端测试辅助）
- **`pkg/tunnel/xfer/xfertest/`** — 跨传输实现的通用测试套件（`harness.go`, `pipe.go`, `suite.go`）
- **`pkg/testutil/mockserver/`** — mock HTTP server
- **`pkg/testutil/mockdht/`** — mock DHT
- **`pkg/testutil/mockxfer/`** — mock xfer.Conn

### 测试约束
1. **纯标准库测试** — 不使用 testify、gomock、gomega 等第三方断言/模拟库。延续现有 `t.Fatalf`/`t.Errorf` 模式。
2. **127.0.0.1 回环绑定** — 所有含 HTTP 服务的测试必须监听 127.0.0.1（`httptest.NewServer` 默认行为即 loopback），**禁止**监听 `0.0.0.0` 或 `localhost`（后者在 Windows 可能触发防火墙授权弹窗）。
3. **Windows 兼容** — 所有测试必须在 Windows 上通过（除标注 `//go:build !windows` 的 Unix-only 测试外）。路径分隔符使用 `filepath.Join` / `filepath.ToSlash` 处理跨平台差异。
4. **全局状态隔离** — 测试 `cmd/sproxy` 和 `cmd/sclient` 时须用 `t.Cleanup` 恢复包级全局变量（`cfgPtr`、`currentDir`、`cfgFile` 等）。
5. **Viper 隔离** — 测试优先使用 `viper.New()` 创建独立实例而非 `GetViper()` 全局单例（`LoadFromViper(v *viper.Viper)` 已接受参数）。

### 测试注意事项
1. **E2E 测试配置隔离** — 启动 sclient 子进程时，必须用 `--config` 指向临时配置文件，不要只用 `--server` flag。`--server` 不会阻止加载本地 `~/.config/sproxy/sclient.yaml` 中的 tunnel_key 等配置，导致测试意外通过隧道通信。
2. **`-race` 下超时翻倍** — 含 goroutine 的测试（特别是 mux/p2p）在 `-race` 下运行时间显著增加。Context timeout 设置时留足余量，推荐正常值的 3 倍。
3. **覆盖率测量排除`test/`和`tools/`** — `go test -cover ./...` 包含 E2E 测试包和工具包会稀释 total 覆盖率。正确做法：`go test -cover ./internal/... ./pkg/... ./cmd/...`
4. **Makefile 修改优先用 Edit tool** — sed 处理 Makefile 的多行模式（反斜杠续行、`$$` 转义、`{` `}`嵌套）极其脆弱。复杂修改用 Read + Edit 工具。

### 测试模式清单

| 模式 | 适用场景 | 示例文件 |
|------|----------|----------|
| **table-driven** | 多种输入/状态的函数级单元测试 | `handlers_test.go`, `gzip_test.go`, `cd_test.go` |
| **表驱动 + subtest** | 参数化场景分组执行 | `gzip_test.go:TestGzipMiddleware_TableDriven` |
| **httptest.Server** | HTTP handler 黑盒集成测试 | `integration_test.go:newTestServer` |
| **httptest.NewRecorder** | middleware 白盒测试 | `gzip_test.go`, `cors_test.go` |
| **mock server** | 客户端测试（模拟服务端） | `client_test.go:newMockServer` |
| **build+subprocess** | 二进制级别端到端测试 | `test/e2e_test.go:startSPROXY` |
| **fuzz** | 边界条件自动探索 | `validate_fuzz_test.go`, `calcchunksize_fuzz_test.go` |
| **chaos** | crash 恢复测试 | `e2e_test.go:TestChaos_*` |
| **concurrent** | 竞态检测 | 各 `_test.go` 中含 `sync.WaitGroup` 的测试 |

### 已知的技术债务
- `pkg/server/rename_handler.go:66` TOCTOU 竞态窗口（Stat 与 Rename 之间，后续优化原子）
- `pkg/server/cloud_download.go:791` URL→ID O(n) 遍历（数百 URL 时建索引）
- `pkg/server/config_api.go:199` rateLimiter.UpdateConfig 热更新未接线（TODO）

<!-- superpowers-zh:begin (do not edit between these markers) -->
# Superpowers-ZH 中文增强版

本项目已安装 superpowers-zh 技能框架（20 个 skills）。

## 核心规则

1. **收到任务时，先检查是否有匹配的 skill** — 哪怕只有 1% 的可能性也要检查
2. **设计先于编码** — 收到功能需求时，先用 brainstorming skill 做需求分析
3. **测试先于实现** — 写代码前先写测试（TDD）
4. **验证先于完成** — 声称完成前必须运行验证命令

## 可用 Skills

Skills 位于 `.claude/skills/` 目录，每个 skill 有独立的 `SKILL.md` 文件。

<details>
<summary>展开查看 20 个 skills 列表</summary>

- **brainstorming**: 在任何创造性工作之前必须使用此技能——创建功能、构建组件、添加功能或修改行为。在实现之前先探索用户意图、需求和设计。
- **chinese-code-review**: 中文 review 沟通参考——话术模板、分级标注（必须修复/建议修改/仅供参考）、国内团队常见反模式应对。仅在用户显式 /chinese-code-review 时调用，不要根据上下文自动触发。
- **chinese-commit-conventions**: 中文 commit 与 changelog 配置参考——Conventional Commits 中文适配、commitlint/husky/commitizen 中文模板、conventional-changelog 中文配置。仅在用户显式 /chinese-commit-conventions 时调用，不要根据上下文自动触发。
- **chinese-documentation**: 中文文档排版参考——中英文空格、全半角标点、术语保留、链接格式、中文文案排版指北约定。仅在用户显式 /chinese-documentation 时调用，不要根据上下文自动触发。
- **chinese-git-workflow**: 国内 Git 平台配置参考——Gitee、Coding.net、极狐 GitLab、CNB 的 SSH/HTTPS/凭据/CI 接入差异与镜像同步配置。仅在用户显式 /chinese-git-workflow 时调用，不要根据上下文自动触发。
- **dispatching-parallel-agents**: 当面对 2 个以上可以独立进行、无共享状态或顺序依赖的任务时使用
- **executing-plans**: 当你有一份书面实现计划需要在单独的会话中执行，并设有审查检查点时使用
- **finishing-a-development-branch**: 当实现完成、所有测试通过、需要决定如何集成工作时使用——通过提供合并、PR 或清理等结构化选项来引导开发工作的收尾
- **mcp-builder**: MCP 服务器构建方法论 — 系统化构建生产级 MCP 工具，让 AI 助手连接外部能力
- **receiving-code-review**: 收到代码审查反馈后、实施建议之前使用，尤其当反馈不明确或技术上有疑问时——需要技术严谨性和验证，而非敷衍附和或盲目执行
- **requesting-code-review**: 完成任务、实现重要功能或合并前使用，用于验证工作成果是否符合要求
- **subagent-driven-development**: 当在当前会话中执行包含独立任务的实现计划时使用
- **systematic-debugging**: 遇到任何 bug、测试失败或异常行为时使用，在提出修复方案之前执行
- **test-driven-development**: 在实现任何功能或修复 bug 时使用，在编写实现代码之前
- **using-git-worktrees**: 当需要开始与当前工作区隔离的功能开发，或在执行实现计划之前使用——通过原生工具或 git worktree 回退机制确保隔离工作区存在
- **using-superpowers**: 在开始任何对话时使用——确立如何查找和使用技能，要求在任何响应（包括澄清性问题）之前调用 Skill 工具
- **verification-before-completion**: 在宣称工作完成、已修复或测试通过之前使用，在提交或创建 PR 之前——必须运行验证命令并确认输出后才能声称成功；始终用证据支撑断言
- **workflow-runner**: 在 Claude Code / OpenClaw / Cursor 中直接运行 agency-orchestrator YAML 工作流——无需 API key，使用当前会话的 LLM 作为执行引擎。当用户提供 .yaml 工作流文件或要求多角色协作完成任务时触发。
- **writing-plans**: 当你有规格说明或需求用于多步骤任务时使用，在动手写代码之前
- **writing-skills**: 当创建新技能、编辑现有技能或在部署前验证技能是否有效时使用

</details>

## 如何使用

当任务匹配某个 skill 时，使用 `Skill` 工具加载对应 skill 并严格遵循其流程。绝不要用 Read 工具读取 SKILL.md 文件。

如果你认为哪怕只有 1% 的可能性某个 skill 适用于你正在做的事情，你必须调用该 skill 检查。
<!-- superpowers-zh:end -->

## 记录

### 跨文件隐式全局陷阱（formatSize/escHtml 转发删除事故）
- **教训**：`app.js` 曾定义 `formatSize`/`escHtml` 等转发函数（委托 `appRender`），为去重整体删除后，**页面其他文件（upload.js）仍有裸调用** → 运行期 `ReferenceError: escHtml is not defined`（上传时才触发）。
- **根因**：`node --check` 只做语法解析、**不做跨文件符号解析**；`make web-test` 只测 app.js 相关，`upload.js` 的调用点从未被单测覆盖；浏览器只测列表加载、没触发上传操作 → 错误路径完全未被触及。
- **规则（避免重演）**：
  1. **渲染工具一律走 `appRender.xxx` 命名空间**（`formatSize`/`escHtml`/`getChecksumPrefix`/`bytesToHex`），**禁止在 app.js/upload.js 内再定义同名函数**；
  2. 签名/SHA-256 一律走 `sclientCrypto`/`sclientSig`，同理禁止页面层重复定义；
  3. **所有引用跨文件隐式全局的文件**（upload.js 引用 `currentSubdir`/`showToast`）须在文件顶部 `// global：<名>（<提供文件>）` 注释显式声明调用方，加载序安全靠 index.html script 顺序（调用点运行期解引用）；
  4. **补 `node --check` 覆盖每个非 cli 的静态 JS**（Makefile web-test 已把 upload.js 纳入）；
  5. **可测入口**：进度文案纯函数双入口（`app-render.uploadProgressText`），避免埋 DOM 依赖；其余纯函数一律放 app-render 并在 app-render.test.js + upload.test.js 覆盖。
  6. **回归验证必须触发用户报错路径**：列表加载 ≠ 上传；至少实测一次真实上传 + 断点续传 + 列表刷新 + 分享/版本弹窗，并确认 console 无 error。
- 已固化：`Makefile` 加 `node --check web/static/upload.js`；`upload.js` 顶部显式 `// global` 声明；`appRender.uploadProgressText` 可测入口。

### 云端下载文件名生成规则（双端共享）
- 规则唯一实现：Go `pkg/cloudfilename`（`DefaultFromURL` / `Safe` / `ResolveFilename` / `ValidateEntries`）+ JS `web/static/cloudfilename.js`（UMD，浏览器全局 `cloudfilename` 与 Node 导出双用）。
- **一致性靠共享语料保证**：`pkg/cloudfilename/testdata/cases.json` 是权威语料，Go 测试 `TestDefaultFromURL_FromFixture` 与 JS 测试 `web/static/cloudfilename.test.js`（`make web-test`）都断言它 → 双端任一改规则导致不一致，测试立即失败。
- **`DefaultFromURL` 返回即安全文件名**（内部已调用 `Safe`），调用方无需再额外包装；JS 对应 `safeDefaultFromURL`。
- **`Entry` 类型**（URL + Filename）定义在 `pkg/cloudfilename`，client 与 server 共用（不再各自定义 CloudDownloadEntry / CloudBatchURL）。
- **`ResolveFilename(e Entry)`** 校验+解析文件名：显式 Filename 含非法字符返回哨兵错误 `ErrEntryUnsafeFilename`（**只校验不修改**，不静默改写保存名）；Filename 为空则按 URL 自动生成。
- **`ValidateEntries(entries)`** 统一做 URL scheme/host 校验 + 同 URL 不同 filename 去重（`ErrEntryDupURL`）；client 批量/组创建前调用，server `validateCloudDownloadURL` 用 `ValidateEntry` + `ResolveFilename`。
- wget 行为：路径以 `/` 结尾 → `index.html`（`/xx/?a=v` → `index.html_a=v`，`?` 被 Safe 替换为 `_`）；raw query 直接附加在文件名后；路径最后一段百分号解码。
- Go 用 `url.Parse`（path 已解码一次）+ `url.PathUnescape`（二次解码，**不把 `+` 转空格**）；JS 用 `parseURL`（从原始字符串提取 rawPath/rawQuery/hash）对齐 Go url.Parse，**不能**用 `new URL().pathname`（WHATWG 会折叠点段、归一化 query）。
- **Go/JS 语义差异（已对齐并录入语料）**：
  - 非法百分号编码 → 两端都返回 `"download"`，但 **Go 只校验 path 与 fragment**，query 原样保留（`?x=100%` → `file.txt?x=100%`）。JS 正则必须只查 `rawPath + hash`，**不能查整个 href**（否则 query 非法 `%` 误判 download）。
  - Go `url.Parse` **不做点段归一化**（`/dir/..` 保留）；JS 从原始字符串提取未归一化路径，不能直接读 `pathname`。
  - **query 原样保留（两端一致）**：JS 用 raw query（不做归一化），与 Go `parsed.RawQuery` 一致——`?x=中文`、`?x=a b`、`?x=%E4%B8%AD%E6%96%87` 均已入语料（历史曾因 JS 用 WHATWG `search` 归一化产生分歧，已修复）。
  - 已知接受的分歧（畸形输入，不入语料）：非法 UTF-8 字节序列（`%FF`）首次解码时 Go `url.Parse` 按字节解码、JS `decodeURIComponent` 抛错返回 download；双重编码 `%25FF` 二次解码时 Go `PathUnescape` 解出原始字节、JS 保留字面 `%FF`（JS 无法表示非法 UTF-8 字节）。
- `Safe` 替换 `\ / ? : < > | " *` 与 NUL 为 `_`；`=` `&` 保留；Trim 首尾空格与点；**抵御 Windows 保留设备名**（CON/NUL/PRN/AUX/COM1-9/LPT1-9 加 `_` 前缀）；**按 254 字节截断**（优先保留扩展名、不劈开 UTF-8 字符）；结果为空返回 `"download"`。
- 注意：`url.QueryUnescape` 会把 `+` 转空格，**不要**用它做路径段解码（历史上导致与 wget/JS 不一致）。

### If-Range 续传一致性校验
- `Result.ETag` 字段已加入；`writeFullBody` / `handleRangeResume` / `finalizePartial` 三条完成路径都会把 ETag 写入 `.partial.etag` 伴侣文件。
- 续传时发送 `If-Range` 头；服务端返回 200 = 不匹配 → 删除 partial+etag 回退全量下载；416 三条分支（finalize / 全量重下 / stale partial 全量重下）。
- **416 收尾必须有内容身份确认**：`416 + total==existingSize` 只有在**缓存 ETag 与 416 响应 ETag 匹配**时才走 `finalizePartial`；否则回退全量重下。否则"同尺寸但内容已变"的陈旧 partial 会被静默收尾为错误文件（数据损坏）。回归测试：`TestHTTPDownloader_RangeResume_416SameSizeStalePartialRedownloads`。
- **206 续传必须交叉校验 ETag**：`handleRangeResume` 接收 cachedETag，若响应 ETag 非空且与发送的 If-Range 不同（服务端忽略 If-Range），回退全量下载，防止新旧内容拼接成混合文件。回归测试：`TestHTTPDownloader_IfRange_ServerIgnores_206NewETag`。
- 教训：`finalizePartial` 曾只返回 ETag 却不 saveETag，被测试捕获——**三条完成路径的 ETag 落盘必须一致**。

### 文件名预览流程（Web UI）
- 用户输入 URL → 预览界面显示每个 URL 的自动生成文件名（已做 `filepathSafe`，**预览即最终保存名**，避免"预览 a/b 实际保存 a_b"）→ 用户可修改 → 确认后提交。
- 三个按钮（链式下载/仅提交/创建组）都经过预览确认流程；批量/组下载每个 URL 可独立指定保存文件名。
- 创建组时前端本地预检文件名冲突（与服务端 `CreateGroup` 规则一致），冲突在发送前拦截，避免 409 往返。

### 暗色模式对比度（WCAG AA）
- 文字 token：`--text-primary` #e8e8f0、`--text-secondary` #c0c4d0、`--text-muted` #989aad；背景层次 `--bg-page` #12122a / `--bg-container` #1a1a3e / `--bg-hover` #252550。
- 按钮/提示 token 全部按**白字 ≥4.5:1** 校准：primary `#3a6ea5`、danger `#b84040`、warning `#9c6a10`、share `#2e6f70`、toast-success `#1e8449`/error `#c0392b`/warning `#9c6a10`/info `#2b6cb5`。改暗色 token 时用相对亮度公式逐色验证（勿用原 #4a8cd6 之类浅色配白字）。
- **陷阱 1**：`style.css` 中 `@media (prefers-color-scheme: dark)` 块用 4 空格缩进、`[data-theme="dark"]` 块用 2 空格缩进 → `Edit` 的 `replace_all` 只能命中一种缩进。改暗色 token 必须**同时更新两处**并 curl 验证（`grep -c` 应为 2）。
- **陷阱 2**：index.html 内联样式与 app.js 动态 HTML 曾硬编码亮色（`#dir-bar` 背景 `#f0f4ff`、目录行 `#f8f9fa`、tab 激活 `#4a90d9`、`#333/#666/#888/#777/#555/#eee/#ccc`），暗色下直接不可读且被 Lighthouse 标记。新 UI 一律走 CSS 变量，禁止内联亮色。验证：`chrome lighthouse snapshot (dark)` 的 color-contrast 必须 score=1。
- 三个 tab 切换函数（stats/share/cloud）的 JS 里 `el.style.color` 也要用 `var(--tab-active)/var(--text-primary)/var(--text-secondary)`，不能硬编码。

### 组创建保证无文件冲突
- `CreateGroup` 在创建子任务前先校验所有 URL 的文件名（`ResolveFilename`：显式 filename 或按 URL 自动生成）是否唯一，冲突返回 409 并回滚（不泄漏任务与存储预留）。
- 去重命中（相同 URL 已属其他组）同样拒绝并入组；客户端可指定 `filename` 字段消除冲突。
- CLI `group` 命令在发送前用共享 `cloudfilename.ResolveFilename` 本地预校验并打印 URL→文件名计划；`--url-file` 每行 `URL<TAB>FILENAME` 可指定保存文件名。

### 禁止静默失败（原则 + 已修复清单）
- 任务终态（completed/failed/cancelled）的持久化失败不能只打 Warn：`saveTask`/`saveGroup` 写盘失败意味着重启后状态回滚。已改为**返回 error**，终态调用点（完成/fail/cancel/resume、组状态变更）记 **Error 日志**；进度类 dirty flush 仍容忍。
- 清理类 I/O（`_ = os.Remove(...)`）可容忍失败，但**状态流转与存储账本（ReservedSize）不能静默不一致**；释放前先归零防二次释放。
- 网络/超时错误要显式分类（可重试 vs 终态），避免"失败被吞掉后任务永久挂起"或"不可重试错误反复重试"。
- **已修复的静默失败/可靠性缺陷**（审查确认）：
  - 去重命中对副本重复启动下载（Critical）：`SubmitAndStart` 现在同步置 `running` 再 `go`，`running` 已置即跳过启动——防止同一 URL 无限并发下载 + 存储账本变负。
  - 完成路径覆盖取消/删除（Important）：写 `completed` 前复查任务存在且未被取消，取消的任务丢弃已下载文件。
  - 取消/启动边界竞态（Important）：executeDownload 启动前、取得信号量后复查 `cancelled`；排队取消不再 `failTask`（避免 cancelled→failed 反转）。
  - 组回滚误删既有任务（Important）：`CreateGroup` 回滚只删本次新建任务，去重吸收的既有任务仅清除组归属。
  - 失败任务存储账本欠计（Important）：`failTask` 按磁盘实际占用对账，保留 `.partial` 时账本不欠计。
  - CLI `wait` 静默吞轮询错误（Important）：轮询/初始获取失败改为返回 error，不伪造 failed。
  - 链式下载部分失败静默成功（Important）：`submitTasks` 提交失败即报错、`waitForTasks` 有失败任务即返回错误，不再无条件打印"完成"。

### 开发环境陷阱：sproxy 双进程
- `go build -o build/bin/sproxy`（无扩展名）与 `build/bin/sproxy.exe` 是两个可执行体；旧进程会一直占用端口，导致新二进制不生效（curl 拿到旧 CSS/旧行为）。
- 重启前必须**两个都杀**：`taskkill //F //IM sproxy.exe` 和 `taskkill //F //IM sproxy`；然后确认 `curl /healthz` 无响应再起新进程。

### 字符串转义注意事项
- Go 源文件中 `"\\"` 在 JSON/JS 编辑时需要双重转义。
- `tools.edit` 的 `old_string` 必须精确匹配源文件内容。
