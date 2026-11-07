<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 弱网大文件下载失败与断点续传（2026-11）

> 背景：用户报告 `sclient download <file>` 在弱网下大文件「一直失败」，伴随
> `写入文件失败: decrypt stream: read chunk: unexpected EOF` 与失败后刷屏 Usage。
> 本文件沉淀根因、修复口径与仍有效的判断，供后续维护者参考（非现行事实源；
> 现行语义见 `docs/cli.md` 的 download 一节）。

## 根因（三个独立缺陷叠加）

1. **整请求超时掐断长下载**：`FileClient` 的 `http.Client.Timeout` 默认 300s，且
   `WithTunnel` 把同一超时传给了 `tunnel.NewClient`（隧道外层 `POST /tunnel`）。
   `http.Client.Timeout` 覆盖**响应体读取全程**，弱网下单次大文件下载必然超过 300s ⇒
   中途取消。隧道面的响应体是解密流（`pkg/tunnel/stream.go`），底层被切断后表现为
   `decrypt stream: read chunk: unexpected EOF`。
   - 仓库已有 `WithNoClientTimeout(ctx)` 机制（2026-10-23 为 `/upload/complete` 引入），
     但**下载路径从未使用**，且**隧道面完全不识别**该标记（只对直连面生效）。
2. **`--resume` 对下载是空操作**：`WithChunkedResume` 的 `opt.resume` 只被**上传**路径读取；
   `getDownloadParams` 从不消费它。单流下载用 `os.Create` 截断、失败即 `os.Remove`；
   分块下载同样 `os.Create` + 失败删文件 ⇒ 任何重跑都从头再来，再次撞上 300s。
3. **CLI 报错转储 help**：根命令未设 `SilenceUsage/SilenceErrors`，cobra 在 RunE 返回错误时
   打印 `Error: ...` + 完整 Usage；部分命令又自己 `WriteErrLine` 一次，`main` 再打印一次
   ⇒ 同一错误刷 2–3 遍。

## 修复口径（仍有效）

- **长下载不受整请求 Timeout 约束**：下载相关请求（`/download`、`/download/chunk`、
  `OpenDownload`）一律带 `WithNoClientTimeout`；隧道面新增 `tunnel.WithNoTimeout(ctx)`
  标记（`pkg/tunnel`），`client.doRequestPrepared` 在隧道分支透传。直连面沿用既有
  `noClientTimeoutFrom` 逻辑。**判据**：任何「覆盖流式响应体读取全程」的客户端级 Timeout
  都不适用于长下载——要靠 ctx/连接级机制兜底，而不是整请求挂钟。
- **续传落 sidecar**（`<输出文件>.sproxy-resume.json`，原子写）：
  - 单流：`offset` 已写字节；重试用 `Range: bytes=<offset>-`；失败保留部分文件 + sidecar
    （`--resume` 时），成功删除。服务端不认 Range（返回 200 全量）⇒ 截断重来。
  - 分块：`chunk_size` + base64 完成位图；重入跳过已完成块；每完成 N 块/1s 节流落盘。
  - 身份校验：`filename/kind/size/checksum/mtime/chunk_size` 任一不匹配 ⇒ 判陈旧、删断点、
    从头下载（**绝不**把不同文件的字节拼到一个输出里）。下载尾部仍走原有 checksum / 零窗兑底。
- **CLI 错误单点打印**：根命令 `SilenceUsage + SilenceErrors`，由 `main` 统一输出
  `sclient: <err>`；被点名命令移除重复 `WriteErrLine`。
- **单次调用内自愈**：单流续传带退避重试（1s/2s/…封顶 30s，共 5 次），4xx 视为致命不重试。

## 仍有效的判断 / 残余

- `--chunked` 的 `tryDownloadChunk` 每次把整块读入内存（`io.ReadAll`）；大文件自适应
  `chunk_size` 可增长到 64 MiB，×并发（默认 8）⇒ 峰值内存可较高。**未改**（既有行为，
  非本次缺陷面）。
- 续传 sidecar 与输出文件同目录；若用户在下载中断后自行移动/篡改部分文件，则判陈旧重下
  （fail-safe，不会静默产出损坏文件，但会丢已下进度）。
- `/download` 的 Range 由服务端 `http.ServeContent` 支持；外部卷/加密卷的 Range 语义已在
  服务端既有实现覆盖（`armRangeVerify` 等），本次未改服务端。
- 隧道 `HTTPClient.Timeout` 仍是所有请求的默认上限（300s）；本次只让**下载类**请求可跳过。
  若未来有其它长时流式响应（如大文件流式转发），同样应显式标记 `WithNoTimeout`。
