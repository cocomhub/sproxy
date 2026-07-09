# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 上级目录 `../CLAUDE.md` 与 `../AGENTS.md` 为工作区通用指南（中文回复、UTF-8 无 BOM、SPDX 许可证头、最小改动等），全部适用于本子项目；以下内容仅补充 sproxy 专属要点，与上级冲突时以本文件为准。

## 项目定位

`github.com/cocomhub/sproxy` 是一个**轻量文件上传/下载/删除服务 + 加密隧道**，附带 `sclient` 客户端二进制。Go 1.26，依赖（新增）`github.com/spf13/cobra`、`github.com/spf13/viper`、`github.com/adrg/xdg` + 原有 `gopkg.in/yaml.v3`。

> 历史：早期版本曾包含 `/{host}/{filepath...}` HTTPS 透明转发与 `/bandwidth` 端点，已于重构移除，定位收敛为文件服务 + 隧道。

## 执行偏好

- **子代理开发**：多步骤实现计划优先使用 `subagent-driven-development` 技能，禁用 worktree，直接在当前分支开发。
- **worktree**：除非用户明确要求，不使用 git worktree。

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
| `.` | 核心库（`go.mod`，仅 `gopkg.in/yaml.v3` + `golang.org/x/sys`） |
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
  ↑  hub 层: 节点注册 / 路由表 / 中继转发 (RouteTable / RelayHandler)
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

`RegisterRoutes` 在 `cmd/sproxy/root.go` 中挂到 `http.NewServeMux`。支持两层认证：主 mux 走 Bearer auth（`authMiddleware`），`localMux` 走隧道密钥（`POST /tunnel` 内部路由时跳过 Bearer auth）。

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
3. 环境变量（前缀 `SPROXY_`，如 `SPROXY_ADDR`、`SPROXY_UPLOADS_DIR`）
4. CLI 标志（`--addr`、`--uploads-dir`、`--tunnel-key`）

优先级：CLI 标志 > 环境变量 > 配置文件 > 默认值。

配置**文件不存在时**：不报错，仅使用默认值+flag/env 覆盖（不再自动创建默认配置文件）。

`LoadConfig(path)` 函数保留用于测试兼容，不由新 CLI 调用。

### 完整配置字段

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `addr` | string | `:18083` | 监听地址 |
| `uploads_dir` | string | `./uploads` | 上传目录 |
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
| `tls.enabled` | bool | false | |
| `tls.cert_file` / `tls.key_file` | string | | |
| `tls.auto_tls` | bool | false | 自动生成 ECDSA P-256 自签证书 |
| `tls.client_ca` | string | | mTLS CA 证书路径 |
| `auth_token` | string | 空 | Bearer token 认证 |
| `rate_limit.enabled` / `.requests` / `.window` | | 关闭 | tunnel handler 限流 |
| `chunk_size` | int | 4 MB | 分块上传每块大小 |
| `max_chunk_size` | int | 64 MB | 客户端最大分块大小 |
| `max_chunk_upload_bytes` | int | 8 MB | 服务端单块请求体上限 |
| `upload_session_ttl` | duration | 24h | 未完成上传会话过期时间 |
| `versioning.enabled` / `.max_versions` | | 关闭 | 文件版本管理 |
| `hub.enabled` / `.node_id` / `.relay_token` | | 关闭 | 中继 Hub 配置 |
| `hub.transports.ws.enabled` / `.listen` | | 关闭 | WebSocket 传输 |
| `cors.allowed_origins` | []string | | CORS 配置 |
| `cloud_download.concurrent` | int | 3 | 云端下载并发数 |
| `cloud_download.sync_threshold` | size | 100MB | 同步阈值 |
| `provider.default` | string | | 云端下载提供者 |
| `provider.timeout` / `.retry` | | | 提供者超时/重试 |
| `max_storage_bytes` | int64 | 0（不限） | 存储上限 |

所有超时字段使用 Go duration 语法（`"30s"`、`"5m"`）。`tunnel_key` 必须是 64 个十六进制字符（32 字节 AES-256 密钥），否则启动失败。生成密钥：`sclient genkey`。

SIGHUP 重载范围有限：仅 `log_level`/`log_format`/`auth_token` 等"软配置"会生效；`addr`/`uploads_dir`/`tunnel_key`/`rate_limit`/`server_timeouts`/`max_header_bytes` 需要重启进程。

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
| `relay [flags]` | 中继节点模式（连接 Hub） |
| `genkey` | 生成 64 hex 密钥 |
| `config [show\|set <k> <v>]` | 配置管理 |
| `diag` | 诊断连接问题 |
| `version` | 版本 + 配置信息 |
| `cd [path]` | 切换当前目录 |
| `pwd` | 打印当前目录 |

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
- `cmd/sproxy/root.go` 中 `runServer` 的信号处理 goroutine 在 `ListenAndServe` 失败时泄漏（`for sig := range signalChan` 永不退出）
- `test/e2e_test.go` 的 `findModuleRoot` 用文件系统遍历定位 `go.mod`，与已有的 `runtime.Caller` 方案冗余
- `pkg/tunnel/mux/mux.go` 中的 goroutine 在极端情况下可能泄漏（`retransmitLoop` 因 `releaseStream` vs `closeWithError` 竞争导致）
- `pkg/server/handlers.go` 中的 `parseDuration` 辅助函数可被 `time.ParseDuration` 替代（用于兼容两种格式的临时桥接）

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