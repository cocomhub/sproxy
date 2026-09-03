# 配额磁盘封顶实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 让所有写路径（upload/chunked/cloud/sync/archive/version）的配额账本恒等于磁盘占用：内部文件写前预感知真实大小并一次性预留，外部下载（cloud download）用 QuotaWriter 边写边记自动补留，chunked 改为 seek 直写临时名（不再有合并窗口 2× 峰值），meta 桶纳入租户配额。

**架构：** `pkg/quota` 保持 Pool/Scope/Reservation 三层（路径化叠加），新增仅外部下载使用的 `QuotaWriter`。内部路径统一「预感知大小 → TryReserve(size) → 写入 → 完成后 ReleaseUsage(old)」。chunked 上传改为「init 预留+创建整临时文件 `.inflight-<hash>-<id>.part` → 分块 seek 直写 → complete 全文件校验→rename」。`ScanAndRecalculate` 不再跳过 meta 桶，reconcile 白名单加 meta 并懒建 meta 子 Scope。`bucket_limits` 配置（相对租户根路径 map）可选配功能桶/子目录上限。

**技术栈：** Go 1.26 标准库 + `gopkg.in/yaml.v3`（配置）。`pkg/quota`（账本）、`pkg/storage`（os.Root/Tenant 布局）、`pkg/server`（handlers）、`pkg/sync`（FS 抽象）、`pkg/server/syncmgr`、`pkg/syncexec`。不新增第三方依赖——singleflight 语义由现有 per-uploadID 读写锁承担，跨 worker 由 O_EXCL 临时名 + rename 原子替换兜底。

---

## 文件结构

按职责拆分，每个任务可独立测试：

- `pkg/quota/quota.go`（修改）— 新增 `QuotaWriter` 类型 + `BoundWriter`；`Scope` 增加 `WriteFileQuota`（内部路径整文件工具）。`Reservation` 保持。
- `pkg/quota/quota_writer_test.go`（新增）— QuotaWriter/BoundWriter/WriteFileQuota 全部行为的 TDD 测试。
- `pkg/server/config.go`（修改）— 新增 `BucketLimits map[string]int64`（相对租户根）字段 + 默认 + SIGHUP 范围。
- `pkg/server/handlers.go`（修改）— `quotaBucketFor` 扩展支持路径子 Scope；装配 meta 子 Scope + bucket_limits。
- `pkg/server/storage_manager.go`（修改）— `ScanAndRecalculate` 不再 SkipDir meta、归集 meta 桶；reconcile 白名单加 meta。
- `pkg/server/quota_reconcile.go`（修改）— 白名单 meta。
- `pkg/server/chunked_upload.go`（修改）— init（临时名+预留）、chunk（seek 直写+BoundWriter）、complete（全文件校验+mismatch_chunks+ReleaseUsage）。
- `pkg/server/upload_store.go`（修改）— 会话伴随文件持久化分片 checksum 表；新增 `TempFilePath`/`TempFileCommit`/`VerifyChunkChecksums`；清理（DeleteSession/cleanupExpired）删除临时名。
- `pkg/server/upload_handler.go`（修改）— 覆盖写统一 ReleaseUsage + 移除写路径 Adjust（对齐用 WriteFileQuota）。
- `pkg/server/cloud_download.go`（修改）— 下载流接 QuotaWriter（自动补留+失败保留+Finish）；移除手写 Adjust。
- `pkg/syncexec/executor.go`（修改）— 按 Stat size 预预留（sync push/pull 的本地写侧）。
- `pkg/server/cloud_archive_handler.go`（修改）— archive 解压接预预留（tar 条目 size）。
- `pkg/server/archive.go`（修改）— archive/version 恢复预预留调整。
- `pkg/server/version*.go` / `rename_handler.go`（修改）— 相关写路径对齐。

测试文件：`pkg/quota/quota_writer_test.go`、`pkg/server/quota_write_path_test.go`（已有，扩展）、`chunked_upload_test.go`、`chunked_owner_test.go`、`store_test.go`、`upload_handler_test.go`、`cloud_download_test.go`、`sync_handler_test.go`、`syncmgr/*`、`storage_manager_test.go`（扩展）。

---

### 任务 1：QuotaWriter + BoundWriter + WriteFileQuota（`pkg/quota`）

**文件：**
- 创建：`pkg/quota/quota_writer.go`
- 修改：`pkg/quota/quota.go`（新增方法，不改现有 API）
- 测试：`pkg/quota/quota_writer_test.go`

设计落点（对齐规格 `pkg/quota` 的 QuotaWriter 语义）：

```go
// quota_writer.go（pkg/quota 内）
// QuotaWriter 仅用于外部下载（cloud download 等大小不可知路径）：
// 写前预留（已知 Content-Length 用其值，否则占位 1 GiB），写中 commitUp，
// 预留不够自动补留，写失败保留 reserve，Finish 结算。
type QuotaWriter struct {
    scope   *Scope
    written int64 // 已确认 committed 的实际写入量
    reserved int64 // 已立账预占（写后递减）
}
func (w *QuotaWriter) Write(p []byte) (int, error) { /* commitUp 不足自动补留；失败保留 */ }
func (w *QuotaWriter) Finish(success bool, oldSize int64)

// BoundWriter：通用限定长度写入（内部路径 seek 直写 / 新分片与重传分片都用），
// 超限返回错误丢弃超出。防客户端字节越界写坏相邻分片（配合乱序安全性）。
type BoundWriter struct { f *os.File; offset, limit, written int64 }
func (w *BoundWriter) Write(p []byte) (int, error)

// WriteFileQuota：内部整文件工具（upload/archive/version/sync 本地写侧）：
// TryReserve(size) → 写 → Finish(true)；失败保留 reserve 由上层重试用。
func (s *Scope) WriteFileQuota(ctx context.Context, f *os.File, size int64, r io.Reader, oldSize int64) (int64, error)
func (s *Scope) VerifyChunkChecksum(f *os.File, offset int64, want string) (ok bool, err error) // seek 读回该分片重算，供重启逐块校验
```

- [ ] **步骤 1：编写失败的 `TestQuotaWriterReserveThenCommit`**（写前 TryReserve、Write 后 committed 增 reserved 减）与 `TestQuotaWriterAutoTopUp`（预留不足自动补留，补留失败 ErrStorageFull）与 `TestQuotaWriterFailureKeepsReserve`（Write 失败保留 reserved，重试复用后再 Finish(true) 才释放多余）
- [ ] **步骤 2：运行测试确认失败**：`cd /d/workdir/leon/cocomhub/sproxy && go test -count=1 ./pkg/quota/` → 编译失败「undefined: QuotaWriter」
- [ ] **步骤 3：实现 `QuotaWriter.Write`**：`commitUp(amount, actual)` 语义——本次写量超过剩余预留时先 TryReserve(差额) 再 commitUp；TryReserve 失败返回 ErrStorageFull，且**不回调已写量**（preserve）。失败返回 (n, err) 但不动 reserved/written 后续计数（Write 非幂等按每次调用记账，注意调用点约定：失败写不调用 Finish）。
- [ ] **步骤 4：实现 `QuotaWriter.Finish(success, oldSize)`**：成功→释放未用 reserve（`scope.Release(w.reserved)`，或写成 `w.reserved` 归还）+ 覆盖写 `scope.ReleaseUsage(oldSize)`；放弃→`scope.ReleaseUsage(w.written)` 回拨已 commit 部分+释放剩余 reserve（`scope.Release(w.reserved)`）。
- [ ] **步骤 5：实现 `BoundWriter`**：`limit` 为分片最大长度，Write 前算 `room := limit - written`；room<=0 返回 `io.EOF`/error；len(p)>room 截断到 room。Offset 由调用方 seek，Writer 只保护长度。
- [ ] **步骤 6：实现 `Scope.WriteFileQuota`** 与 `VerifyChunkChecksum`。WriteFileQuota：TryReserve(size) → io.Copy(f, r) → Finish(true, oldSize)；Copy 失败会怎样——TryReserve 已立账，Copy error 时调 Finish(false) 回拨并返回 error。
- [ ] **步骤 7：全部测试通过**：`go test -count=1 ./pkg/quota/` 且 `golangci-lint run ./pkg/quota/` 0 issues
- [ ] **步骤 8：Commit**（本包独立可交付，pre-commit `make build` 不破坏编译）

---

### 任务 2：bucket_limits 配置 + meta 子 Scope 装配

**文件：** `pkg/server/config.go`、`pkg/server/config_test.go`（扩展）、`pkg/server/handlers.go`

- [ ] **步骤 1：先写配置测试**（断言 `BucketLimits` 解析 `bucket_limits: {user/videos/hd: 10485760}`，默认空 map，SIGHUP 范围注释说明 bucket_limits 需重启）
- [ ] **步骤 2：config.go 新增** `BucketLimits map[string]int64` 字段 + `Default()` 空 map + 文档注释（相对租户根路径）
- [ ] **步骤 3：handlers.go 修改 `quotaBucketFor`**：先查 `BucketLimits[path]`、再看桶子 Scope、最后租户总；`ensureTenantQuotaLocked` 懒建 meta 子 Scope（上限 0）+ 读 bucket_limits 建子目录子 Scope（`scope.Scope(relPath, limit)`）
- [ ] **步骤 4：测试 bucket_limits 生效**（放在 `quota_write_path_test.go` 或 `config_test.go`）
- [ ] **步骤 5：`golangci-lint run ./pkg/server/` 0 issues + 相关测试通过**
- [ ] **步骤 6：Commit**

---

### 任务 3：meta 桶纳入扫描

**文件：** `pkg/server/storage_manager.go`、`pkg/server/quota_reconcile.go`、`pkg/server/storage_manager_test.go`（扩展）

- [ ] **步骤 1：先写扫描测试**（构造 meta 桶文件，断言 tenantBuckets[tenant]["meta"] 含字节、不再 SkipDir）
- [ ] **步骤 2：实现 ScanAndRecalculate**：meta 不再 SkipDir，走 `case "meta":` 归集 `addTenantBucket(tenant, "meta", size)`；totalUsage 累加 meta（临时文件/Session 等一并计）
- [ ] **步骤 3：实现 reconcileQuotaScopes**：白名单 meta（`quotaBucketNames` 加 "meta" + 更新 `ensureTenantQuotaLocked` 循环）
- [ ] **步骤 4：全 server 测试**：`go test -count=1 ./pkg/server/`——重点确认 `/api/stats` 的 UsageByCategory/Usage 不再因 meta 加入而破坏既有断言（如有破坏的调整后续任务，但扫描本身不改 totalUsage 语义——meta 计入 totalUsage 改变 /api/stats 返回，需同步更新 stats 测试）
- [ ] **步骤 5：Commit**

---

### 任务 4：chunked init 改造（临时名 + 预留 + seek 直写）

**文件：** `pkg/server/chunked_upload.go`、`pkg/server/upload_store.go`、`pkg/server/upload_store.go`（session 持久化）

- [ ] **步骤 1：写失败的 init 测试**（`TestChunkedInitCreatesTempFileAndReserves`：临时名存在+Truncate 大小+预留量正确；同名冲突拒绝；already_exists 不建临时名）
- [ ] **步骤 2：运行确认失败**
- [ ] **步骤 3：实现 init**：①查正式名 checksum（already_exists 跳过，不建临时名）②查同名 session 同 checksum 复用续传、不同 checksum 直拒（Conflict）③`TryReserve(TotalSize)` 于租户 user 桶/租户总 Scope（507 时清理 session）④创建 `.inflight-<sha256(正式名)>-<uploadid>.part` + `Truncate(TotalSize)` + O_EXCL（防跨 worker 冲突）。session 记录 tempPath + 预留量。
- [ ] **步骤 4：实现 chunk seek 直写**：读取块 → 块 checksum 校验 → `f.Seek(i*chunkSize, io.SeekStart)` → `BoundWriter`（limit=chunkLen(i)）写入 → `MarkChunkReceived(i, checksum)`。**不再写独立 .chunk 文件到 chunk 桶**。covered 单块重复 upload 校验幂等逻辑需保留（`ReceivedChunks[i] && ChunkChecksums[i]==checksum` → skip）。
- [ ] **步骤 5：配置/会话存储更新**：session.json 持久化新增 `ChunkChecksums`（已有字段）+ 分片 size 表；cleanupExpired/DeleteSession 删除临时名（`os.Remove(temp)`）+ 释放预留（Reservation.Release / ReleaseUsage(written)，与现在 Release 一致）；recoverSessions 读取临时名状态。
- [ ] **步骤 6：测试补强**（乱序写入 complete 正确、BoundWriter 越界拒绝、两分片并发写不同 offset、续传恢复）
- [ ] **步骤 7：全 server 测试** `go test -count=1 ./pkg/server/` + lint 0 issues
- [ ] **步骤 8：Commit**

---

### 任务 5：complete 全文件校验 + mismatch_chunks + 覆盖写 ReleaseUsage

**文件：** `pkg/server/chunked_upload.go`（complete 与 merge 部分）

- [ ] **步骤 1：写失败的 `TestCompleteFullVerifyAndMismatchChunks`**（篡改某分片后 complete → 返回 mismatch_chunks 列表，客户端据此只重传坏分片再 complete 成功）与 `TestCompleteOverwriteReleaseUsage`（覆盖写后 ReleaseUsage(old) 生效、committed == 新大小）
- [ ] **步骤 2：运行确认失败**（现状 complete 用 Adjust 且无 mismatch；确认失败行为）
- [ ] **步骤 3：实现 complete 新逻辑**：对临时名**全文件** SHA-256 == file_checksum → `root.Rename(tmp, rel)` → 写 checksum store → 覆盖写 `ReleaseUsage(old)`（移除 `Adjust(prev,actual)`）；不匹配→逐分片 seek 重算与保存 checksum 表比对→返回 `mismatch_chunks`。
- [ ] **步骤 4：重启恢复「逐分片 seek 重算比对」**：recoverSessions 读伴随文件 → 对临时名逐分片 seek 重算 checksum→匹配复用、不匹配需要重传（bitmap 置 false）。补一个恢复后 complete 的测试（数据可重传前提下）。
- [ ] **步骤 5：全文件校验失败保留 + 客户端自动重传逻辑**（客户端收到 mismatch→重传→再 complete）
- [ ] **步骤 6：全 server 测试 + lint 0 issues**
- [ ] **步骤 7：Commit**

---

### 任务 6：sync push/pull 预预留 + archive/version 对齐

**文件：** `pkg/syncexec/executor.go`、`pkg/server/syncmgr/manager.go`（reconcile 调整）、`pkg/server/cloud_archive_handler.go`、`pkg/server/archive.go`、`pkg/server/version*.go`

- [ ] **步骤 1：写失败的 sync 预预留测试**（pull 落盘前按 file size TryReserve，配额不足文件失败可重试、保留已成功字节、不全量重传）
- [ ] **步骤 2：实现 sync pull 预预留**：在 `pkg/syncexec.Executor.Run` 本地写侧按文件 size `scope.TryReserve` + 写后 `ReleaseUsage` 对齐（parse 决策保持 push 走远端 chunked 不动）
- [ ] **步骤 3：archive 解压按 tar 条目 size 预留** / version 恢复按源 Stat 预留（同样模板）
- [ ] **步骤 4：全测试 + lint 0 issues**
- [ ] **步骤 5：Commit**

---

### 任务 7：cloud download QuotaWriter 接入（外部下载）

**文件：** `pkg/server/cloud_download.go`

- [ ] **步骤 1：写失败的 `TestCloudQuotaWriterAutoTopUp`**（响应超过占位 1 GiB 不 507 且自动补留；响应阻塞时预留不足 507 中止）、`TestCloudWriteFailureKeepsPartialAndReserve`（写失败 .partial 保留、重试复用）
- [ ] **步骤 2：实现**：下载响应流包 `QuotaWriter`（预留 = Content-Length 或占位 1 GiB）；写完 `Finish(true, old)`；失败 `Finish(false)` / 保留 .partial。移除 cloud 的 `Adjust` 手写路径。
- [ ] **步骤 3：cloud_archive（既有 load tar）同样对齐**（archive 桶预留）
- [ ] **步骤 4：全 server 测试 + lint 0 issues**
- [ ] **步骤 5：Commit**

---

### 任务 8：收尾——统一记账模板 + 测试矩阵闭包

**文件：** 全部相关写路径 + 测试

- [ ] **步骤 1：重构去重**：各写路径统一「TryReserve(size) → 写 → Finish(true, old)/Rollback」；grep 确认写路径无 `Adjust(` 残留（reconcile 保留）；meta/所有临时文件计入对应桶/总 Scope
- [ ] **步骤 2：测试矩阵补全**（规格测试矩阵逐项断言）：覆盖写防丢、超限即 507、失败保留 reserve、分片级重传、BoundWriter 越界、乱序、中断回收、meta 配额、singleflight 并发、跨 worker O_EXCL、bucket_limits、Adjust 仅 reconcile
- [ ] **步骤 3：全量验证**：`make build` + `golangci-lint run ./pkg/... ./cmd/... ./pkg/tunnel/xfer/ext/ws/... ./pkg/tunnel/xfer/ext/quic/... ./pkg/tunnel/xfer/ext/grpc/... ./pkg/tunnel/xfer/ext/webrtc/... ./pkg/tunnel/hub/ext/kad/...` + `go test -count=1 ./pkg/... ./cmd/... -race` 全绿
- [ ] **步骤 4：Commit**（收尾提交）

---

## 自检

**1. 规格覆盖度：**

| 规格章节 | 对应任务 |
|---------|---------|
| 配额语义（租户根/bucket_limits/meta） | 任务 2、3 |
| 记账模型（内部预预留 + QuotaWriter 外部） | 任务 1、6、7 |
| 覆盖写 ReleaseUsage、Adjust 退出写路径 | 任务 5、7、8 |
| chunked 方案 B（临时名、seek、mismatch、续传） | 任务 4、5 |
| sync/cloud 边写边预留 | 任务 6、7 |
| meta 纳入配额 | 任务 3 |
| 错误处理（507/保留 reserve） | 任务 1、4、7 |
| 测试矩阵 | 任务 8 |
| 配置 bucket_limits | 任务 2 |

无遗漏。两处强化：续传恢复（重启逐分片 seek 重算比对）补充进任务 5 步骤 4；乱序/BoundWriter 越界单测补进任务 4 步骤 6。

**2. 占位符扫描：** 无 TODO/待定。`BoundWriter` 在任务 1 定义为导出类型，任务 4/8 使用一致。（`pkg/server` 不能访问 `pkg/quota` 未导出符号，所以必须导出。）

**3. 类型一致性：** `QuotaWriter`/`BoundWriter`/`WriteFileQuota`/`VerifyChunkChecksum` 均在任务 1 定义，后续任务使用一致。
