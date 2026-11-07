# 审查：批次 12——新合并功能（加密卷/E2EE/多卷/传输层/mesh+联邦）

- **批次**：12
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `f84c5b29`（#539-#557 区间 17 个新实现）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过（1 项 P1 已开修复 PR #558）

**发现数**：P0 0 / P1 1 / P2 0 / P3 1

## 发现清单

### [P1] 加密卷分块下载读密文 + Seek 失败（#540）——已开修复 PR #558
- **位置**：`pkg/files/chunked_download.go:103`（DownloadChunk）
- **问题**：#540 改普通下载走 OpenDecrypted，但 **chunked_download 漏切**——加密卷 `/download/chunk` 用普通 `Open` 返回**密文**，且 `file.Seek(offset)` 对解密流（仅支持 Seek(0,Start)/(0,End)）失败。
- **修复**（PR #558）：`Root.IsEncrypted()` + DownloadChunk 加密卷分支 `OpenDecrypted` + `io.CopyN(io.Discard, offset)` 流式跳（正确性优先 O(offset)）+ `readFromStream` 按 length 读 + SHA-256。TDD + 变异命中（encrypted 恒 false → 重组密文不匹配红）。
- **影响**：at-rest 加密卷用户的分块下载（含客户端断点续传/并发拉流）会拿到密文或 500。

### [P3] X-Share-Watermark 种子公开给访问者（#546）
- **位置**：`pkg/server/share.go:649`
- **问题**：访问分享链接响应头带 WatermarkSeed（原图直出也带）——种子非机密（设计意图「防截图外流可追溯」），但持种子者可预期水印渲染细节。
- **建议**：可接受（种子不是密钥，威慑语义）；记录为演进约束——如需严格防伪需服务端强加水印（watermark 参数必选）。

## 通过项（无问题面）

- **A 组 #540/#556 at-rest 加密卷**：SetEncryption（32B key fail-closed）/ OpenDecrypted 流式 AES-256-GCM 64KiB 块 + 魔数头 / OpenFileEncrypted 写面 / SetCipher 块大小可配 + 未知算法 fail-closed / 租户子根加密传播；key 缺失 fail-closed（禁静默降级）。**除上述 chunked_download P1 外通过。**
- **B 组 #552/#553 加密归档/客户端 E2EE**：ArchiveRequest.cipher 预检 fail-closed（4xx 而非 pipe 内 200 中断）；E2EE（SPROXY-CIPHER-v1 魔数 + 流式 GCM，纯标准库）——与服务端 at-rest（SPROXY-FS-ENC-v1）格式互不兼容是**设计意图**（客户端自加密 = 传输+存储双保险，服务端无 key）。
- **C 组 #545/#541 S3 多桶/webdav 多卷**：splitS3Bucket（key 首段=卷名）+ s3TenantFor 经 volSet.Tenant 按 owner 解析（ACL 管，无越权）+ multipart 4 端点感知；davHandler ?volume= 显式选卷 + Authorize(owner) 校验 + 预建 user 桶。
- **D 组 #542/#546/#544 gzip/分享水印/传输指标**：gzipEligible 白名单（text/* json xml js wasm svg）+ SSE（/api/events）/WS（/ws）跳过（防缓冲断流/升级失败）；ShareLink.WatermarkSeed 持久化 + X-Share-Watermark 头；WS/QUIC Metrics() 原子计数 + /metrics 聚合（provider 注入破 import 环）。
- **E 组 #539/#551/#554/#555/#557/#550**：mesh RelayStreamRequest E2E/Path 透传；LWW 后写覆盖（未注入 Writer 恒 ErrReadOnly）；/remote/stats 授权同读面 + QuotaStatus；Alertmanager webhook v2 + Grafana annotations（空 URL fail-closed 变异命中）；删除传播二次 stat（mtime 不一致 → 保留 + skipped_conflict）；metrics_port 独立端口 + MetricsAuth 令牌门（query token / Bearer 双通道）。
- **聚焦测试全绿**：`go test -run 'TestEncrypt|TestCipher|TestE2EE|TestS3Bucket|TestWebDAV|TestGzip|TestWatermark|TestXferMetrics|TestLWW|TestFederated|TestAlertmanager|TestGrafana|TestMetricsPort|TestPropagateDeletes'` 全包通过。

## 验证方式

- 源码逐路径审查（加密卷读写分叉/gzip 跳过面/mesh E2E 透传/联邦授权/删除冲突语义）
- 聚焦测试全绿 + 修复 PR #558 TDD/变异命中
