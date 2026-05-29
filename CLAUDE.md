# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> 上级目录 `../CLAUDE.md` 与 `../AGENTS.md` 为工作区通用指南（中文回复、UTF-8 无 BOM、SPDX 许可证头、最小改动等），全部适用于本子项目；以下内容仅补充 sproxy 专属要点，与上级冲突时以本文件为准。

## 项目定位

`github.com/cocomhub/sproxy` 是一个**轻量文件上传/下载/删除服务 + 加密隧道**，附带 `sclient` 客户端二进制。Go 1.26，**严格仅依赖标准库 + `gopkg.in/yaml.v3`**。未经明确指示不得新增依赖。

> 历史：早期版本曾包含 `/{host}/{filepath...}` HTTPS 透明转发与 `/bandwidth` 端点，已于本次重构移除，定位收敛为文件服务 + 隧道。

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

现有测试位置：`pkg/server/handlers_test.go`、`pkg/tunnel/example_test.go`。

`addlicense` 由 `make fmt` 强制注入 SPDX 头；本地缺失时：`go install github.com/google/addlicense@latest`。

版本元数据通过 `-ldflags "-X main.Version=... -X main.BuildAt=..."` 注入到 `cmd/sproxy/main.go`、`cmd/sclient/main.go` 中的 `Version` / `BuildAt` 包级变量，**不要手工改这些常量**。

## 仓库结构

```
cmd/
  sproxy/   # 服务端：文件服务 + tunnel handler + Web UI
  sclient/  # 客户端：upload/download/delete/list/tunnel/config/genkey 子命令
pkg/server/          # HTTP handler / Config / ChecksumStore / RateLimiter
pkg/client/          # FileClient（Go 库）
pkg/tunnel/          # 基于 AES-256-GCM 的加密隧道（流式帧协议）
web/static/          # 嵌入式 Web UI（/ → /ui/）
certs/               # 测试用证书
config.example.yaml  # 参考配置
fileclient.sh        # 旧 shell 客户端（保留作参考）
```

## 关键路由（`pkg/server/handlers.go`）

`RegisterRoutes` 在 `cmd/sproxy/main.go` 中挂到 `http.NewServeMux`：

- `GET /` — 301 重定向到 `/ui/`
- `GET /ui/` — 嵌入式 Web UI 静态文件
- `GET /healthz` — 文本 `OK`
- `GET /version` — 文本 `Version: x\nBuildAt: y`
- `POST /upload` — multipart 字段名 `file`，**必须**带 `X-File-Checksum`（SHA-256）头；文件已存在时按 checksum 比对幂等返回
- `GET /download?filename=<name>` — `filepath.Base` 校验防穿越；响应头返回 `X-File-Checksum`
- `POST /delete?filename=<name>` — **必须**带 `X-File-Checksum` 头，匹配后才删
- `GET /api/files` — 返回 `{files: [{name, size, checksum}]}` 结构化列表
- `DELETE /api/files?filename=<name>` — 仅靠 Bearer auth 删除（保留兼容，Web UI 已迁移到 `POST /delete`）
- `POST /tunnel` — `tunnel.NewHandler(key)`，AES-256-GCM 加密的请求转发

## 配置（`pkg/server/config.go`）

YAML 由 `LoadConfig(path)` 读取，**文件不存在时会自动写一份默认配置**到该路径（`SaveConfig`），不会回退到内存默认值；要"只用内存默认值"必须传 `path == ""`。

CLI 标志覆盖优先级（`cmd/sproxy/main.go`）：`--addr` > `--uploads-dir` > `--tunnel-key` 分别覆盖 `Addr` / `UploadsDir` / `TunnelKey`。所有超时字段（`server_timeouts.*`）使用 Go duration 语法（`"30s"`、`"5m"`）。`max_header_bytes` 默认 1 MiB。

`tunnel_key` 必须是 64 个十六进制字符（32 字节 AES-256 密钥），否则启动失败。生成密钥：`sclient genkey`。

SIGHUP 重载范围有限：仅 `log_level`/`log_format`/`auth_token` 等"软配置"会生效；`addr`/`uploads_dir`/`tunnel_key`/`rate_limit` 需要重启进程。

## tunnel 包要点（`pkg/tunnel/`）

- AES-256-GCM + 随机 12 字节 nonce，nonce 前置于密文
- 统一帧协议（`application/x-tunnel-frame`）：`[4B BE metaLen][encrypted metadata][stream chunks...]`，其中 stream chunk = `[2B chunkLen][nonce|ciphertext|tag]`，默认 64 KB / chunk
- `NewHandler(key)` 返回标准 `http.Handler`，可嵌入任意 `http.ServeMux`
- `Client.Do(req)` 是标准库风格客户端，发加密请求并解密响应

## sclient CLI 解析约定（`cmd/sclient/main.go`）

`sclient` 的命令行解析是**手写的**，不是 cobra/pflag：

- 全局选项可以放在子命令前后任意位置，由 `parseGlobalOptions` 收集
- `tunnel` 子命令复用 `-X / -H / -d` 风格，但由 `tunnel` case 内的二次解析处理，**不要把它当全局选项**
- 新增命令时在 `main` 的 `switch cmd` 块里加 case，并在 `printHelp()` 同步帮助文本

## 编码与日志

- 日志统一 `log/slog`（Text 或 JSON handler，按 `log_format` 切换）；新代码不要混入 `zap` / `logrus`
- 中文文案禁止 GBK/ANSI；Windows 终端注意 UTF-8 输出，避免"文件正确但终端乱码"误判
- 错误优先 `fmt.Errorf("...: %w", err)` 包装；handler 内不要把原始 error 直接抛给客户端，使用 `UploadResponse{Success,Message}` JSON 格式回包
