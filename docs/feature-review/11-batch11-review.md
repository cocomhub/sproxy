# 审查：批次 11——新合并功能（s3 闭环/grpc 限流/stats/回收站）

- **批次**：11
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `544cf3f5`（#531-#537 区间）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过（2 项 P2 建议加固，非阻塞）

**发现数**：P0 0 / P1 0 / P2 2 / P3 2

## 发现清单

### [P2] S3 complete 不校验 ETag / meta key（#531）
- **位置**：`pkg/server/s3_multipart.go:110-145`（s3CompleteMultipart）
- **问题**：complete 按客户端声明的 part 序号拼接 part 文件——**不校验 ETag**（part 内容未验证，S3 协议要求 complete 时客户端提交各 part ETag 服务端应核对）且 **meta 存的 init key 未读**（complete 的 key 与 init 的 key 无一致性校验）。
- **影响**：uploadID 是 16B crypto/rand 不可猜测，跨会话拼接需 uploadID 泄露——风险低；ETag 校验是 S3 协议正确性 + 内容完整性纵深。
- **建议**：complete 读 meta 校验 key 一致 + 校验客户端 ETag 与落盘 part 的 md5 匹配（防 part 被篡改/错拼）。

### [P2] S3 complete 目标文件无配额/checksum 记账
- **位置**：`pkg/server/s3_multipart.go`（s3CompleteMultipart）
- **问题**：拼接后的目标文件**不经配额 Scope 预留/提交**（普通上传有 routeUpload 双账本）——大文件分块上传可绕过 owner 配额。
- **建议**：complete 落盘前 TryReserve(合计大小) + Commit（对齐普通上传配额语义）。

### [P3] quotaStatusOf 水位整数截断（#537）
- **位置**：`pkg/server/stats.go:76-79`
- **问题**：`usage*100/maxB` 整数除法截断——79.9% 显示 79%（阈值 80/95 判断略保守）。
- **建议**：可接受（百分比展示非关键路径）；如需精确用 float + 四舍五入。

### [P3] trashGCLoop 只遍历 SyncTenantList（#534）
- **位置**：`pkg/server/handlers_endpoints.go:101-122`
- **问题**：GC 遍历 `SyncTenantList()`（内存缓存租户）——未访问过的租户 trash 不清理（延迟到下次访问）；同 versionGC 的已知局限（全仓磁盘扫描有 cost）。
- **建议**：可接受（TTL 7d 宽容，延迟清理无害）；记录为演进约束。

## 通过项（无问题面）

- **#531 S3 分块闭环**：init（16B crypto/rand uploadID）/ upload-part / complete（按 PartNumber 排序拼接 + 清 parts）/ abort（遍历 chunk 桶清 parts + meta）完整；**且已含批次10 加固**（MaxBytesReader 64MiB + partNumber 校验 + s3MaxParts=10000）——TDD 2 用例 + 变异命中（abort 清理禁用→红）。
- **#532 grpc 会话数上限**：`grpc.MaxConcurrentStreams(128)` 服务端并发流上限（防单连接无限流 DoS）。
- **#533 stats 面板通知区**：notify-format.js 纯函数（node --test 3）+ showStats 并行拉 /api/notify/history + R10 门禁（web-test 登记 + Playwright e2e PASS）。
- **#534 回收站 GC + WebUI**：config trash.{ttl=7d,gc_interval=1h} + trashGCLoop（ticker+stop+WaitGroup 同 versionGC）+ CleanupTrash 按 TTL 清理 + WebUI 按钮/面板（trashTableHtml 纯函数 + 恢复委托）+ Playwright e2e。
- **#537 /api/stats quota 段**：quotaStatusOf 纯函数（nil 安全 + 不限→0）+ 认证用户→本租户 Scope / admin→全局聚合 + TDD（上传 100B → watermark>0）+ 变异命中。
- **聚焦测试全绿**：`go test -run 'TestS3|TestTrash|TestQuotaStatus|TestGrpcTransport' ./pkg/server/... ./pkg/tunnel/xfer/ext/grpc/...` 均通过。

## 验证方式

- 源码逐路径审查（s3 complete 校验面 + grpc 限流 + quota 水位 + trash GC 循环）
- `go test -count=1 -timeout 120s -run 'TestS3|TestTrash|TestQuotaStatus|TestGrpcTransport' ./pkg/server/... ./pkg/tunnel/xfer/ext/grpc/...` → **ok**
