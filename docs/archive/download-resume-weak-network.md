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

## 真实场景 E2E 补充（2026-11-07，tunnel + 8MiB/s 限速代理 + 强杀）

测试方式：起真实 `sproxy`（零凭据注册 + SproxySig 隧道）+ 真实 `sclient.exe`，客户端经
本地限速 TCP 代理（全局 8 MiB/s，模拟弱网）访问；下载中途 `TerminateProcess` 强杀，
再用同一输出路径续传；代理侧统计下行总字节，并与本地 SHA-256 比对。覆盖 0B/1B/64KiB/
100MiB（阈值边界）/100MiB+1/128MiB（4MiB 整数倍）/130MiB、损坏/陈旧/越界 sidecar、
`--resume=false`、`--chunked` 小文件、并发 1/8/16、3 文件并发、`--decrypt`（E2EE 密文）等。

**E2E 又暴露并已修的 3 个实现缺陷（均属“跨进程续传”正确性）：**

1. **单流 checkpoint 不落盘**：续传进度此前在 `io.Copy` 整体返回后才 `addOffset` ⇒ 进程被
   强杀时磁盘 offset 恒为 0（实测 offset=0 / 部分文件 28MiB），重入只能从 0 重下。**修法**：
   单流改为逐 64KiB 读写 + 逐块 `addOffset`（内部 1s/N 次节流落盘）。
2. **checkpoint 落后被判陈旧重下**：强杀时已写文件常大于最后一次落盘 checkpoint（实测
   offset=13.6MB / 文件=14.2MB），而校验要求 `文件大小 == checkpoint` ⇒ 每次都从头重下。
   **修法**：`0 <= checkpoint <= 文件大小 <= 远端大小` 即有效，从 checkpoint 续传（尾部 ≤1s
   数据被重新覆盖）。
3. **已收满但未收尾仍重下尾部**：文件已满但 sidecar 未更新（强杀在收尾前）。**修法**：
   加“已收满直接按大小+checksum 校验”快路径，命中即完成、零 GET。

**另一个顺带发现并修复的 Windows 可靠性问题**：`--decrypt` 的最终替换用裸 `os.Rename`
覆盖已存在文件，Windows 上约 2/5 概率瞬时 `Access is denied`（sharing violation）。**修法**：
有界退避重试（`renameReplaceWith`，8 次 × 20ms 指数；只重试 rename、绝不先删目标，
与 `pkg/storage.AtomicRename` 同策略）。

**E2E 性能结论**：sidecar 开销在限速代理下不可辨（全量 26.8s vs 28.5s，±5% 抖动）；
约 50% 断点续传耗时约为全量的 59%（15.8s vs 26.8s；差值 = 代理限速 + 强杀时在途分块重传）。

**仍未修的相邻发现（另片，不阻塞续传）**：本地卷**分块下载路径未应用带宽限速**
（`writeChunkSuccess` 直写 `c.w`，未包 `limitResponseWriter`；外部卷 `serveExternalChunk`
已包）⇒ 开启 `rate_limit.bandwidth` 时整文件下载受限、分块下载不受限（公平性缺口）。
