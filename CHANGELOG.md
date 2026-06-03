<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Changelog

本文件遵循 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/) 风格。
版本号遵循 [SemVer 2.0.0](https://semver.org/lang/zh-CN/)。

## [Unreleased]

### Added
- Phase 1（代码质量与覆盖）：
  - 测试覆盖率达标：pkg/server 71.6%、pkg/client 60.2%、pkg/tunnel 83.3%
  - 新增 `internal/size` 测试、`cmd/sproxy` 测试、`cmd/sclient` 测试
  - Web UI 新增重命名按钮（调用 POST /rename）
  - 修复 `context.TODO()` 占位符（替换为 `context.Background()`）
  - 修复进程中 `sync.Map uploadCache` 包级变量污染（迁移为 FileClient 结构体字段）
  - 修复 `json.Encode` / `os.MkdirAll` 被忽略的错误（记录日志）
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

## [0.2.0] - 2026-06-01

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

## [0.1.0]

初始公开版（无正式 release tag）。提供：

- 文件上传 / 下载 / 删除 / list / mkdir / rmdir / 分块上传 / 分块下载 API
- AES-256-GCM 加密隧道（`POST /tunnel`）
- 嵌入式 Web UI（`/ui/`）
- sclient 配套客户端（cobra + viper + XDG）
