# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

> 上级目录 `../AGENTS.md` 与 `../AGENTS.md` 为工作区通用指南（中文回复、UTF-8 无 BOM、SPDX 许可证头、最小改动等），全部适用于本子项目；以下内容仅补充 sproxy 专属要点，与上级冲突时以本文件为准。

## 依赖策略

- **Go 标准库优先**：功能可用标准库实现时优先使用标准库。
- **`golang.org/x/` 系列**：Go 团队维护的准标准库（如 `golang.org/x/crypto`、`golang.org/x/sys`、`golang.org/x/net` 等）可自由使用，无需额外评审。
- **第三方库**：新增非 `golang.org/x/` 的第三方依赖需审慎评估，优先选择纯 Go 实现、API 稳定、社区活跃的库。

`github.com/cocomhub/sproxy` 是一个**轻量文件上传/下载/删除服务 + 加密隧道**，附带 `sclient` 客户端二进制。Go 1.26，依赖（新增）`github.com/spf13/cobra`、`github.com/spf13/viper`、`github.com/adrg/xdg` + `gopkg.in/yaml.v3` + `golang.org/x/sys` + `golang.org/x/crypto`。

> 历史：早期版本曾包含 `/{host}/{filepath...}` HTTPS 透明转发与 `/bandwidth` 端点，已于重构移除，定位收敛为文件服务 + 隧道。

## 执行偏好

- **子代理开发**：多步骤实现计划优先使用 `subagent-driven-development` 技能，禁用 worktree，直接在当前分支开发。
- **worktree**：除非用户明确要求，不使用 git worktree。
- **使用中文思考**

## 协作与流程硬规则（pi agent 必读）

> 完整版（含用户已确认的设计决策与全部踩坑记录）：`docs/superpowers/learnings/2026-09-13-agent-operating-rules.md`；
> CI/合并细节：`docs/superpowers/learnings/2026-09-13-ci-merge-process.md`。以下是必须无条件遵守的硬规则：

1. **等 CI 全绿再合并**：本仓 `master` 有 ruleset 必检 7 项（`Test`×2 / `E2E`×2 / `Test Sub-Modules` / `UI E2E` / `SonarQube`；判定以 `gh pr checks` 全绿为准）
   ⇒ 轮询 `gh pr checks` 到 `total≥14 且 pending=0`，**不用 `--auto`**；**合并后删分支**（远端 + 本地）。
2. **Benchmark job 超 10 分钟**⇒ `gh api -X POST .../runs/<id>/cancel` 后 `.../rerun`（rerun 产生**新 job id**，必须动态取）。
3. **不要开纯文档 PR**：`*.md`/`docs/**` 在 `paths-ignore` 内 ⇒ 不触发 CI ⇒ 必检项永不满足；且 `ruleset.bypass_actors=[]` ⇒ **`--admin` 也绕不过**（实测 `Head branch is out of date`）⇒ **文档改动必须搭在代码 PR 里**（必要时加一个真实门禁让 CI 跑起来，如 `internal/archcheck/docs_rules_test.go`）。
4. **CI 等待期并行做下一片**；上片合并后 `git rebase --onto origin/master <已合并提交>` 再开 PR（PR 里不得夹带已合并提交），推自有分支用 `--force`。
5. **TDD + 变异验证**：先写红灯测试（要有失败输出）；声称测试能抓 bug 前先**断言变异已命中**（否则「无输出」= 假绿）。
6. **提交与推送**：只 `git add` 本任务文件；多重 `-m`；**不加署名行**；推送走 https（SSH 不可用）；提交前
   `export PATH="$PATH:$(go env GOPATH)/bin"`（pre-commit 需 `golangci-lint`/`addlicense`）。
7. **禁用 `git stash`**（本仓有他人遗留 stash，会弹错 WIP）；不要用 sed/python 多行改 Makefile（用 Edit 工具）。
8. **子 module 改动**：新增跨 module 依赖要补 `require`+`replace`，并在 **`GOWORK=off`** 下独立构建/测试通过。
9. **接口字段用接口类型**（避免 typed-nil 陷阱）；领域包不得 import 装配层（`pkg/server`）。
10. **Web UI 改动必须带自动化测试 + 过真实浏览器 e2e**（用户明示）：纯函数补 `node --test` 单测、交互/渲染补
    Playwright e2e（`web/e2e`，必检项 `UI E2E Tests`）、新 JS 登记进 Makefile `web-test`（门禁 R10 守）。
    `make web-test` 现已挂进 ui-e2e job。
11. **交付自检**：`gofmt -l` **与 `goimports -l`** 均无输出（CI 的 golangci-lint 启用 goimports，`gofmt` 覆盖不到 import 分组）、
    `go build ./...`+`make build-all`、`make lint`+`make lint-all` 0 issues、`go test ./pkg/... ./internal/...`、
    `make test-all`、`-race`；门禁类自测必跑 `make deadcode-check`、`go test ./internal/archcheck/`（含 R14 睡眠棘轮、
    R18 并发注册门禁等）；收尾片还要 `make check-ci`（含 70% 覆盖率门禁）+ `make test-e2e`。
    **本地全绿后才 push 触发 CI**（见第 14 条）。
12. **CHANGELOG 由 release-please 生成，不再手工维护**：`CHANGELOG.md` 与版本号是 release-please 的**单一事实源**
    （`release-please-config.json` + `.github/workflows/release-please.yml`）。硬要求落在**提交信息**上：
    ① 类型正确（`feat`→Added、`fix`→Fixed、`perf`/`refactor`/`deps`→Changed；破坏性变更加 `!` 或 `BREAKING CHANGE:`）；
    ② subject 写成**用户可读的能力描述**——它会直接成为 changelog 条目。**`CHANGELOG.md` 不得保留 `## [Unreleased]` 段**
    （release-please 以第一个版本标题为插入锚点，该段因 `[` 命中正则 ⇒ 新版本段被插到它上面，且它从不被消费）；
    删除对外 API 用 `remove(<scope>): ...` 提交类型（已映射 `### Removed`），其余无法用类型表达的条目在
    **release PR** 里一次性补进该版本段。`chore`/`docs`/`ci`/`test`/`build`/`style` **也会**进 changelog（`release-please-config.json` 已移除 `hidden`，
    统一落在 `### Changed` 段；即 Conventional 类型全枚举 ⇒「未匹配类型」为空集，任何提交都不会从 CHANGELOG 消失）
    （内容重要时改用 `feat`/`fix`）。发布流程见 `RELEASING.md`；门禁 **R12** 守配置与规则的一致性。
13. **测试并发注册门禁（R18）**：新增测试**直接满足设计**——顶层 `func TestX(t *testing.T)` 默认必须 `t.Parallel()`；
    无法并发的测试必须**显式记录**（`internal/archcheck` 的 `TestSerialRatchet` 会拦截），豁免条件（任一即可）:
    ① 用例体内含 `t.Setenv/t.Chdir/os.Chdir`； ② 函数体内含标记注释 `// sproxy:serial: <短理由>`；
    ③ `internal/archcheck/serial_budgets.tsv` 白名单棘轮（**只减不增**； 上行须同步
    `docs/testing/virtual-time-conversions.md` 登记理由）。
    历史教训：一次 +975 处 t.Parallel 的批量修补花费一个完整周期——**不要让下一次出现同类二次返工**。
14. **本地先过后触发 CI**：CI 里所有可本地执行的 job（lint / test / test-cover / e2e / web-test /
    notest / deadcode-check / check-loopback / 棘轮与并发门禁 )**必须在本地全绿后才 push 触发 GitHub CI**；
    逐 job 失败根因从 `gh api repos/{owner}/{repo}/actions/jobs/<id>/logs` 精确取证后修复，禁止靠猜。
15. **PR 复用纪律**：简单项直接复用**当前最新的 OPEN PR**（追加 commit）；需要特殊设计的内容
    （如一个新的测试基建门禁、一次性大改)另开后续 PR，避免历史 PR 无限膨胀。
16. **禁止 amend 已合并到远端 master 的 squash 提交**：`git commit --amend` 一旦作用在
    已 push 且合并的 squash 上，会造成历史改写且与远端 diverge ——
    修复内容必须走**新分支 + 新 PR**。本教训来自 PR #273 事故（改写已合并 squash 导致 PR diff 混入上一 PR 全部 179 文件）。
17. **测试网络客户端必须隔离（硬规则）**：测试里**禁止**使用 `http.DefaultClient` / 共享的
    `http.DefaultTransport`——并行用例的 `httptest.Server.Close()` 会打断其它用例在途的 idle 连接，表现为
    `transport connection broken: http: CloseIdleConnections called`（本仓已在 `pkg/client`(FileClient)、
    `pkg/testutil/syncmock`、`cmd/sclient` 多次实证）。做法：每测试自建 `&http.Client{Transport: &http.Transport{}}`
    （生产侧库默认也应为每实例独立连接池）。


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
    tunnel.go           # 传统隧道模式（NewLocalHandler, Client.Do）
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

`RegisterRoutes` 在 `cmd/sproxy/root.go` 中挂到 `http.NewServeMux`。支持两层认证：主 mux 走 SproxySig 请求签名（`authMiddleware`，凭据 Ring 非空时启用；`api_keys` 仍走独立 Bearer 多用户模式），`localMux` 走隧道密钥（`POST /tunnel` 内部路由时跳过认证）。

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
- `PUT /api/config` — 更新运行时配置（含 `max_storage_bytes` 动态调整）

### Hub 中继管理（需配置 `hub.enabled: true` + `RouteTable`）
- `GET /api/hub/nodes` — 列出已注册节点
- `DELETE /api/hub/nodes/{id}` — 移除节点
- `GET /api/hub/stats` — Hub 统计

### 隧道
- `POST /tunnel` — `tunnel.NewLocalHandler(nil, localMux)`，AES-256-GCM 加密的请求转发

## 配置（`pkg/server/config.go`）

### 加载方式（viper，来自 `cmd/sproxy/root.go`）

1. 默认值（`Default()`）
2. 配置文件 YAML（`--config` 指定，默认 `sproxy.yaml`）
3. 环境变量（前缀 `SPROXY_`，如 `SPROXY_ADDR`、`SPROXY_STORAGE_ROOT`）
4. CLI 标志（`--addr`、`--storage-root`、`--no-tls`、`--allow-no-auth`）

优先级：CLI 标志 > 环境变量 > 配置文件 > 默认值。

配置**文件不存在时**：不报错，仅使用默认值+flag/env 覆盖（不再自动创建默认配置文件）。

`LoadConfig(path)` 函数保留用于测试兼容，不由新 CLI 调用。

### 完整配置字段

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `addr` | string | `:18083` | 监听地址 |
| `storage_root` | string | `./storage` | 多租户存储根（`<tenant>/{user,cloud,archive,chunk,version,meta}/` 桶布局） |
| `tunnel_key` | string | 已废除（忽略） | **已废除**：隧道密钥由凭据 Ring 中条目的 SK 经 HKDF 自动派生；配置该键仅历史兼容 |
| `log_level` | string | `info` | debug/info/warn/error |
| `log_format` | string | `text` | text/json |
| `max_header_bytes` | int | 1048576 | 最大 HTTP 头字节数 |
| ~~`max_upload_bytes`~~ | — | 1 GiB（硬编码） | **已不可配置**：普通上传请求体上限固定为 `internal/size.UploadBodyLimit`（1 GiB），超限 413 |
| `server_timeouts.read_header` | duration | `5s` | |
| `server_timeouts.read` | duration | `30s` | |
| `server_timeouts.write` | duration | `30s` | |
| `server_timeouts.idle` | duration | `60s` | |
| `server_timeouts.shutdown` | duration | `30s` | graceful shutdown 超时 |
| `tls.enabled` | bool | true | |
| `tls.cert_file` / `tls.key_file` | string | | |
| `tls.auto_tls` | bool | true | 自动生成 ECDSA P-256 自签证书 |
| `tls.client_ca` | string | | mTLS CA 证书路径 |
| `access_keys` | []AccessKey | 已废除（忽略） | **已废除**：SproxySig 凭据改由服务端凭据 Ring 承担（`<storage_root>/<owner>/meta/credentials.json` store 化）；yaml 该键被忽略，登记/轮换走 `sclient trust` / `POST /api/credentials/register` |
| `api_keys.enabled` / `.keys` | | 关闭 | 多用户 API 密钥（独立 Bearer 特性，与 store 凭据互斥，优先） |
| `rate_limit.enabled` / `.requests` / `.window` | | 关闭 | tunnel handler 限流 |
| `chunk_size` | int | 4 MB | 分块上传每块大小 |
| `max_chunk_size` | int | 64 MB | 客户端最大分块大小 |
| `max_chunk_upload_bytes` | int | 8 MB | 服务端单块请求体上限 |
| `upload_session_ttl` | duration | 24h | 未完成上传会话过期时间 |
| `versioning.enabled` / `.max_versions` | | 关闭 | 文件版本管理 |
| `hub.enabled` / `.node_id` | | 关闭 | 中继 Hub 配置（`relay_token` 已废除：注册准入由凭据 Ring 的 SproxySig AK+HMAC proof 提供） |
| `hub.transports.ws.enabled` / `.listen` | | 关闭 | WebSocket 传输 |
| `cors.allowed_origins` | []string | | CORS 配置 |
| `cloud_download.concurrent` | int | 3 | 云端下载并发数 |
| `cloud_download.sync_threshold` | size | 100MB | 同步阈值 |
| `provider.default` | string | | 云端下载提供者 |
| `provider.timeout` / `.retry` | | | 提供者超时/重试 |
| `max_storage_bytes` | int64 | 0（不限） | 存储上限 |

所有超时字段使用 Go duration 语法（`"30s"`、`"5m"`）。`tunnel_key` 已废除（配置忽略，见 `docs/config.md`）；隧道密钥由凭据 SK 经 HKDF 自动派生。

SIGHUP 重载范围有限：仅 `log_level`/`log_format` 等"软配置"会生效；`addr`/`storage_root`/`owner_quotas`/`rate_limit`/`server_timeouts`/`max_header_bytes`/`tls.enabled` 需要重启进程（`tunnel_key`/`access_keys` 已随凭据 store 化移除——凭据管理与轮换走 `sclient trust`/`/api/credentials`，与 SIGHUP 无关）。

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

- **传统模式**：`NewLocalHandler(key, localMux)` → 标准 `http.Handler`，每个请求创建一个 HTTP POST（`key` 参数占位，真实密钥由认证层放入请求 ctx）
- **多路复用模式（推荐）**：`NewTunnel(mux, key)` → 在已有 mux 连接上创建隧道，`Tunnel.Do(req)` 通过虚拟流完成 HTTP 请求-响应交换
- AES-256-GCM + 随机 12 字节 nonce，nonce 前置于密文
- 统一帧协议（`application/x-tunnel-frame`）：`[4B BE metaLen][encrypted metadata][stream chunks...]`，其中 stream chunk = `[2B chunkLen][nonce|ciphertext|tag]`，默认 64 KB / chunk
- mux 层帧协议：`[4B StreamID][1B FrameType][1B Flags][2B PayloadLength][Payload...]`，帧类型含 `FrameData`/`FrameOpen`/`FrameClose`/`FrameCloseWrite`/`FramePing`/`FramePong`
- 心跳：30s Ping，90s 超时断开
- 隧道密钥由认证层根据 AK→SK 派生并放入请求 ctx，**不可热替换**（原 `UpdateKey` 已删除）

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
1. **E2E 测试配置隔离** — 启动 sclient 子进程时，必须用 `--config` 指向临时配置文件，不要只用 `--server` flag。`--server` 不会阻止加载本地 `~/.config/sproxy/sclient.yaml` 中的 server_url/凭据等配置，导致测试行为被本机配置污染。
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

Skills 位于 `.Codex/skills/` 目录，每个 skill 有独立的 `SKILL.md` 文件。

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
- **workflow-runner**: 在 Codex / OpenClaw / Cursor 中直接运行 agency-orchestrator YAML 工作流——无需 API key，使用当前会话的 LLM 作为执行引擎。当用户提供 .yaml 工作流文件或要求多角色协作完成任务时触发。
- **writing-plans**: 当你有规格说明或需求用于多步骤任务时使用，在动手写代码之前
- **writing-skills**: 当创建新技能、编辑现有技能或在部署前验证技能是否有效时使用

</details>

## 如何使用

当任务匹配某个 skill 时，使用 `Skill` 工具加载对应 skill 并严格遵循其流程。绝不要用 Read 工具读取 SKILL.md 文件。

如果你认为哪怕只有 1% 的可能性某个 skill 适用于你正在做的事情，你必须调用该 skill 检查。
<!-- superpowers-zh:end -->