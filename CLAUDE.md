# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 上级目录 `../CLAUDE.md` 与 `../AGENTS.md` 为工作区通用指南（中文回复、UTF-8 无 BOM、SPDX 许可证头、最小改动等），全部适用于本子项目；以下内容仅补充 sproxy 专属要点，与上级冲突时以本文件为准。

## 项目定位

`github.com/cocomhub/sproxy` 是一个**轻量文件上传/下载/删除服务 + 加密隧道**，附带 `sclient` 客户端二进制。Go 1.26，依赖（新增）`github.com/spf13/cobra`、`github.com/spf13/viper`、`github.com/adrg/xdg` + 原有 `gopkg.in/yaml.v3`。

> 历史：早期版本曾包含 `/{host}/{filepath...}` HTTPS 透明转发与 `/bandwidth` 端点，已于重构移除，定位收敛为文件服务 + 隧道。

## 常用命令

```bash
make build                # fmt + 自动发现 ./cmd/* 下所有 main 包，逐个产出到 build/bin/<name>
make build-sproxy         # 只构建 sproxy（模式：build-<cmd-name>）
make build-sclient        # 只构建 sclient
make run                  # build + 用 build/config.yaml 运行 sproxy
make fmt                  # addlicense + go fix + gofmt -s（gofumpt 已注释，不跑）
make clean                # 删除 build/bin
make show-version         # 打印当前构建二进制的版本
```

**无 `make test`、无 `make lint`**。测试直接：

```bash
make test               # go vet + go test -race ./...
make test-packages      # 分组运行测试，简化定位失败包
make cover              # 覆盖率摘要（total: xx%）
make cover-html         # 覆盖率 HTML 报告到 build/coverage/cover.html
go test ./...
go test -run TestName ./pkg/server/...     # 单测
go test -race ./pkg/tunnel/...
```

现有测试位置：`pkg/server/integration_test.go`、`pkg/server/chunked_upload_test.go`、`pkg/tunnel/example_test.go`。

`addlicense` 由 `make fmt` 强制注入 SPDX 头；本地缺失时：`go install github.com/google/addlicense@latest`。

版本元数据通过 `-ldflags "-X main.Version=... -X main.BuildAt=..."` 注入到 `cmd/sproxy/main.go`、`cmd/sclient/main.go` 中的 `Version` / `BuildAt` 包级变量，**不要手工改这些常量**。

## 仓库结构

```
cmd/
  sproxy/   # 服务端：root.go（cobra 入口）+ main.go（版本变量）
  sclient/  # 客户端：root.go + upload/download/delete/list/tunnel/genkey/config/version/cd.go
pkg/server/          # Config / Handlers / ChecksumStore / UploadStore / RateLimiter / validate.go
pkg/client/          # FileClient（Go 库）+ chunked.go + config.go
pkg/tunnel/          # 基于 AES-256-GCM 的加密隧道（流式帧协议）
web/static/          # 嵌入式 Web UI（index.html，支持子目录浏览）
certs/               # 测试用证书
config.example.yaml  # 参考配置
fileclient.sh        # 旧 shell 客户端（保留作参考）
```

## 关键路由（`pkg/server/handlers.go`）

`RegisterRoutes` 在 `cmd/sproxy/root.go` 中挂到 `http.NewServeMux`：

- `GET /` — 301 重定向到 `/ui/`
- `GET /ui/` — 嵌入式 Web UI 静态文件
- `GET /healthz` — 文本 `OK`
- `GET /version` — 文本 `Version: x\nBuildAt: y`
- `POST /upload` — multipart 字段名 `file`，**必须**带 `X-File-Checksum`（SHA-256）头；文件已存在时按 checksum 比对幂等返回。文件名通过 `ValidateFilePath` 校验，支持子目录路径（如 `dir/file.txt`）
- `GET /download?filename=<name>` — `ValidateFilePath` 校验防穿越；响应头返回 `X-File-Checksum`
- `POST /delete?filename=<name>` — **必须**带 `X-File-Checksum` 头，匹配后才删
- `GET /api/files?subdir=path` — 返回 `{files: [{name, size, checksum, mod_time, is_dir}]}` 结构化列表；`subdir` 可选参数用于查看子目录
- `DELETE /api/files?filename=<name>` — 仅靠 Bearer auth 删除（保留兼容，Web UI 已迁移到 `POST /delete`）
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

所有超时字段（`server_timeouts.*`）使用 Go duration 语法（`"30s"`、`"5m"`）。`max_header_bytes` 默认 1 MiB。

`tunnel_key` 必须是 64 个十六进制字符（32 字节 AES-256 密钥），否则启动失败。生成密钥：`sclient genkey`。

SIGHUP 重载范围有限：仅 `log_level`/`log_format`/`auth_token` 等"软配置"会生效；`addr`/`uploads_dir`/`tunnel_key`/`rate_limit`/`server_timeouts`/`max_header_bytes` 需要重启进程。

## sclient CLI（`cmd/sclient/`）

基于 **cobra** + **pflag**，无手动解析。子命令：

| 命令 | 用途 |
|------|------|
| `upload <file>...` | 上传文件，路径保留目录结构 |
| `download <filename> [output]` | 下载文件 |
| `delete <filename>` | 删除文件 |
| `list` | 列出文件（支持 `--subdir`，受 `cd` 影响） |
| `tunnel [flags] <url>` | 隧道请求 |
| `genkey` | 生成 64 hex 密钥 |
| `config [show\|set <k> <v>]` | 配置管理 |
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

- AES-256-GCM + 随机 12 字节 nonce，nonce 前置于密文
- 统一帧协议（`application/x-tunnel-frame`）：`[4B BE metaLen][encrypted metadata][stream chunks...]`，其中 stream chunk = `[2B chunkLen][nonce|ciphertext|tag]`，默认 64 KB / chunk
- `NewHandler(key)` 返回标准 `http.Handler`，可嵌入任意 `http.ServeMux`
- `Client.Do(req)` 是标准库风格客户端，发加密请求并解密响应

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

### 测试约束
1. **纯标准库测试** — 不使用 testify、gomock、gomega 等第三方断言/模拟库。延续现有 `t.Fatalf`/`t.Errorf` 模式。
2. **127.0.0.1 回环绑定** — 所有含 HTTP 服务的测试必须监听 127.0.0.1（`httptest.NewServer` 默认行为即 loopback），**禁止**监听 `0.0.0.0` 或 `localhost`（后者在 Windows 可能触发防火墙授权弹窗）。
3. **Windows 兼容** — 所有测试必须在 Windows 上通过（除标注 `//go:build !windows` 的 Unix-only 测试外）。路径分隔符使用 `filepath.Join` / `filepath.ToSlash` 处理跨平台差异。
4. **全局状态隔离** — 测试 `cmd/sproxy` 和 `cmd/sclient` 时须用 `t.Cleanup` 恢复包级全局变量（`cfgPtr`、`currentDir`、`cfgFile` 等）。
5. **Viper 隔离** — 测试优先使用 `viper.New()` 创建独立实例而非 `GetViper()` 全局单例（`LoadFromViper(v *viper.Viper)` 已接受参数）。

### 现有测试辅助函数查找
- **`pkg/testutil/`** — 跨包通用（TestKey, DiscardLogger, SHA256Hex, CaptureStdout, CaptureStderr）
- **`pkg/server/server_test_common_test.go`** — server 包内共享（testKey, testLogger, withHeader）
- **`pkg/server/integration_test.go`** — `newTestServer` + `newTestServerWithAllRoutes` 等变体
- **`pkg/client/client_test.go`** — `newMockServer`（sproxy 兼容的 mock 服务端）
- **`test/e2e_test.go`** — `startSPROXY`（构建真实二进制并启动的端到端测试辅助）

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

## 如何使用

当任务匹配某个 skill 时，使用 `Skill` 工具加载对应 skill 并严格遵循其流程。绝不要用 Read 工具读取 SKILL.md 文件。

如果你认为哪怕只有 1% 的可能性某个 skill 适用于你正在做的事情，你必须调用该 skill 检查。
<!-- superpowers-zh:end -->
