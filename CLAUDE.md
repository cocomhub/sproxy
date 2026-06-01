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