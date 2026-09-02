// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

# 配额磁盘封顶设计：所有写路径账本 ≡ 磁盘占用

日期：2026-09-02
分支：feature/mesh-p6-owner
状态：已评审通过

## 背景动机

现有多租户配额账本（`pkg/quota` Pool/Scope/Reservation）只跟踪逻辑占位（committed + reserved），
从不核对真实磁盘占用，导致三类路径存在「账本未超但磁盘已写满」的缺口：

1. **分块上传合并窗口**：init 在 chunk 桶预留 TotalSize，complete 时把分块合并到 user 桶临时文件，
   临时文件写满前两处字节并存 → 物理峰值 2×TotalSize，但账本只有 1×TotalSize。
   配额落在 (TotalSize, 2×TotalSize] 区间时放行，磁盘却被写到 2×。
2. **sync pull / cloud download（创建时总大小未知）**：边下边落盘，账本只在任务结束时 reconcile 一次，
   中间字节完全无预留 → 长时间欠计窗口。
3. **meta 桶不计配额**：ScanAndRecalculate 对 `bucket=="meta"` 直接 SkipDir，checksum 账本、session.json、
   sync 任务状态等 meta 字节从不入账 → meta 可写满盘。

目标：任意时刻 **租户根目录实际磁盘占用 ≤ 该租户账本（committed + reserved）**；
所有写路径物理写入前账本已持有对应字节；磁盘物理占用恒被配额封顶。

## 配额语义（定稿）

- **path 层级**：`quota.Scope` 路径化叠加。租户根 Scope 上限 = `owner_quotas[owner]`（默认 `"*"` > 0，其余 0=不限），
  语义 = `<storage_root>/<tenant>/` 下所有字节 ≤ owner_quota。功能桶为子 Scope：
  `user/ cloud/ archive/ chunk/ version/ meta/`（新增 meta），上限默认 0（不单独限），但**可选配**。
- **桶独立**：六个功能桶互相独立，配额叠加，互不混用（`bucket_limits` 独立配置，map[path]int64，路径相对租户根，
  如 `user/videos/hd`）。`owner_quotas` 只管租户根；`bucket_limits` 管功能桶/子目录。
- **globalPool** 兜底所有租户总和 ≤ max_storage_bytes（沿用）。
- **临时文件一律算配额**（计入所属桶子 Scope）；meta 桶也计入（不再 SkipDir）。

## 记账模型（定稿）

### 内部路径一律预感知真实大小、预先预留（避免 QuotaWriter）

所有系统内部文件上传/下载/同步，写前必须先感知真实大小并 `TryReserve(size)` 一次预留，
写中不再逐段记账：

- multipart upload：要求 Content-Length（现状 handler.Size）
- chunked：init TotalSize → 预留+Truncate 整临时文件
- sync push / pull：引擎 Stat 源得真实 size（远端 ListDir/Stat 已返回 size）→ 按文件 `TryReserve(size)` 再写
- archive 解压：tar 条目 header 得 size → 按条目预留
- version 恢复：源版本文件 Stat 得 size

预留不足统一 507 拒绝，不碰正式名；临时名/会话保留供重试。

### QuotaWriter 直接持 Scope（方案 A）——仅外部下载

`pkg/quota` 新增一等公民 `QuotaWriter`，**仅用于创建时真实大小不可知的外部文件下载
（cloud download / 任意 URL 的动态内容）**：

- 写前 `TryReserve(已知估计)`，已知 Content-Length 用其值，不可知用占位（默认 1 GiB，可配置下限）
  先行预留；
- 每次 Write：把该次实际写入量从 reserved 划入 committed（`commitUp(amount, actual)`），
  已写字节计入；**当预留不够、commit 触顶时自动 `TryReserve` 新空间**（写入 commit 不足 → 自动预留新空间
  继续写；TryReserve 失败 → 507 中止）；
- **写入失败不退还 reserve**，也不销账——保留已立 account，供上层重试复用同一 account；
- **文件结束（成功完整写完）**：`Finish(true)` —— 释放未用 reserve + 覆盖写 `ReleaseUsage(oldSize)`；
- **文件放弃（不可重试/超时/取消/过期）**：`Finish(false)` —— `ReleaseUsage(written)` 把已 commit 部分
  从 committed 扣回（磁盘对应删 .partial/已写部分）+ 释放剩余 reserve。

```go
// pkg/quota 内新增（同包应用，必要时导出）：
type QuotaWriter struct {
    scope   *Scope
    written int64 // 已确认 committed 的实际写入量
    reserved int64 // 已立账预占（TryReserve 累加，Write 后递减）
}
func (w *QuotaWriter) Write(p []byte) (int, error)
func (w *QuotaWriter) Finish(success bool, oldSize int64)
```

- **Adjust 退出写路径**，仅保留给 reconcile（启动/周期对账校准磁盘 vs 账本差异）；现有 upload/chunked/cloud
  写路径手写 Adjust 的场景统一移除（normalize 到 TryReserve+Commit+ReleaseUsage）。
  这样消除 Adjust 的桶错位隐患（chunked complete 覆盖写 diff≈0 不记账的 I1 缺陷也随之消失）。

### 覆盖写统一时序（所有写路径）

1. `oldSize := stat(正式名)`（存在时；幂等 checksum 匹配直接跳过）
2. `TryReserve(newSize)`（不碰旧文件；不足 507 拒绝）
3. 写临时名/直写临时名 → 校验 → rename 原子替换
4. `ReleaseUsage(oldSize)`（旧文件字节从 committed 释放）

> 绝不能在 rename 前释放旧文件——磁盘必须保持新旧并存窗口，先释放会造成旧文件丢失风险。

## 内部 vs 外部文件分工（定稿）

内部文件（upload / chunked / sync push / sync pull / archive / version）：大小全部可预先感知 →
写前 `TryReserve(size)` 一次，无需逐段记账（boundedSeekWriter 只限长）；外部文件
（cloud download / 任意 URL 动态内容）：大小不可知 → QuotaWriter 边下边记 + 预留不够自动补留。

| 路径 | 大小可知 | 机制 |
|------|---------|------|
| upload（multipart） | 客户端报 Content-Length | TryReserve(size) + 流式写 + Commit(size)，覆盖写 + ReleaseUsage(old) |
| chunked（内部） | init 报 TotalSize | init TryReserve(TotalSize) + 创建整临时文件（Truncate）→ seek+boundedSeekWriter |
| sync push | 本地 Stat 得 size | TryReserve(size) → 远端 chunked（远端自身方案 B） |
| sync pull | 远端 ListDir/Stat 得 size | 按文件 TryReserve(size) → 直写 user 桶；无逐段记账 |
| cloud download（外部） | 不定 | QuotaWriter 边下边记；预留不够自动补留；完成 reconcile / 放弃 ReleaseUsage |
| archive 解压 | tar 条目 header 得 size | 按条目预留 + 直写 archive 桶 |
| version 恢复 | 源版本 Stat 得 size | 预预留 + 写 version 桶；旋转旧版本入账 |

## 分块上传方案 B（seek 直写 + 临时名 rename + 分片级重传）

### 临时文件标识

- 临时名 = `user/ <dir>/.inflight-<sha256(正式名)>-<uploadid>.part`，置于目标同目录。
- 会话伴随文件 `metadata.json`（session.json 已含 upload_id/filename/total_size/chunk_size/bitmap）额外持久化
  每分片 checksum 表；分片数据**不落盘独立 .chunk 文件**（seek 直写后内存态可丢）。

### init

1. 查正式名 checksum → 匹配：already_exists 跳过（不创建临时名）。
2. 查临时名/存活同名 session：同 checksum→复用（续传）；不同 checksum→**直接拒绝**（Conflict）。
3. `TryReserve(TotalSize)` 于租户总/可选 user 子目录 Scope；不足 507，不碰正式名。
4. 目标同目录创建临时名 + `Truncate(TotalSize)`；名字含正式名 hash，O_EXCL 防跨 worker 冲突。
   临时文件完整大小即预留 → 账本 = 磁盘物理（块按需分配，峰值仍 ≤ TotalSize = 预留）。

### chunk

- 内存读块 → 块 checksum 校验 → seek 直写 `offset=i*chunkSize` → `MarkChunkReceived(i, checksum)` 落 session bitmap。
- 内部路径（大小已预留）**不再逐块记账**；新分片与已 commit 分片重传同为 seek 直写，都用
  **boundedSeekWriter 防越界**（limit = chunkLen(i)，写超返回/截断，不写坏相邻分片）。
- 乱序写入安全：各段独立 offset seek + bound 限长，bitmap 逐段记录，配额与一致性都不依赖写入顺序，
  complete 前全段 commit 即可；并发分段写经 singleflight（进程内）+ O_EXCL 临时名（跨进程）互斥。
  写失败保留已 commit 分片，客户端只补传异常分片，不全量重传。

### complete

- 对**临时名全文件** SHA-256 == file_checksum 才 `root.Rename(tmp, rel)` → 写 checksum store → 删会话目录与
  已 commit → 覆盖写 ReleaseUsage(old)。
- 全文件不匹配 → 标记合并失败，**逐分片 seek 重算**与保存的分片 checksum 表比对，返回 `mismatch_chunks`
  客户端只重传异常分片 → 再 complete 通过为止。中途丢失数据 → 未置成功，客户端自动重传缺失分片
  （重启恢复：读伴随文件 → 逐分片 seek 重算比对，匹配复用、不匹配强制重传）。

## sync / cloud 边写边预留

- sync（内部）：引擎 Stat 源得真实 size → 按文件 `TryReserve(size)` 再写，不逐段记账；push 走远端
  chunked（远端自身方案 B），pull 本地直写 user 桶。写失败的任务中止但**保留已成功字节**，全部同步在一个
  任务内：成功文件保留、失败单文件不影响任务 status，缺失分片/文件由下次同步或引擎补传，不全量重传。
- cloud download（外部）：QuotaWriter 边下边记，预留不足自动补留；失败保留已写 .partial 与 reserve，
  重试复用；完成 reconcile / 放弃 ReleaseUsage。分片级续传按 .partial。
- meta 桶纳入租户配额：ScanAndRecalculate 不再 SkipDir meta，reconcile 白名单加 meta、懒建 meta 子 Scope。

## 错误处理

- 507 任意写路径（upload/chunked/cloud/sync/archive/version）：拒绝，不碰正式名，临时名+会话保留供重试。
- 内部路径：TryReserve(size) 失败即 507 拒绝，无写中记账，无欠计。
- 外部路径（cloud）：QuotaWriter 写中预留不够自动补留，补留失败 507 中止；写失败保留已写 .partial 与
  reserve，返回可重试错误，按 .partial 续传补传。
- 中断/超时/取消/过期：清账 Finish(false) + 删临时名/ .partial/会话目录 + ReleaseUsage(已写部分)。
- reconcile 仅校准（Adjust）；审计错误(disk>ledger) 记 Warn（周期扫描兜底）。

## 测试矩阵

- 覆盖写防丢（新内容替换旧，ReleaseUsage(old) 正确；新旧并存窗口不丢）
- 任意写路径超限即 507（不留半成品 on 正式名）
- 失败保留 reserve + 重试复用同一 account，成功后统一释放多余
- 分片级重传（complete 全文件校验失败 → 只补异常分片）
- boundedSeekWriter 防越界（新/重传分片都覆盖）
- 乱序写 chunk 后 complete 正确
- 中断/过期回收（删临时名 + 回拨）
- meta 配额生效（meta 增长 ≤ owner_quota + max_storage_bytes）
- singleflight 并发一致 / 跨 worker O_EXCL 同临时名唯一
- bucket_limits 子目录上限生效
- Adjust 仅用于 reconcile（无写路径 Adjust 残留）

## 配置变更

- 新增 `bucket_limits: map[path]int64`（相对租户根，示例 `user/videos/hd` → 上限字节）。
- 其余沿用现有 `owner_quotas`、`max_storage_bytes`。

## 影响面（需同步修改）

- `pkg/quota`：新增 QuotaWriter（仅外部下载用）；Reservation 保持（一次性）；暴露池内部原语供
  QuotaWriter 同包使用（含预留不够自动补留的 commitUp+TryReserve 组合）。
- `pkg/server/storage_manager.go` ScanAndRecalculate：meta 不再 SkipDir，归集 meta 桶；reconcile 白名单加 meta。
- `pkg/server/handlers.go`：懒建 meta 子 Scope；bucket_limits；sync/cloud 装配（sync 按 Stat size 预预留，
  cloud 接 QuotaWriter）；覆盖写统一 ReleaseUsage；移除写路径 Adjust。
- `pkg/server/chunked_upload.go / upload_store.go`：临时名直写、分片 checksum 持久化、complete mismatch_chunks。
- `pkg/server/upload_handler.go`、`pkg/server/cloud_download.go`、`pkg/syncexec`：如上（sync 路径改预预留）。
- `pkg/sync/http_transport.go`（远端 chunked 自身方案 B 保持）。

## 非目标（YAGNI）

- 不做多 worker 的跨实例分布式锁（O_EXCL 物理排他 + singleflight 进程内即足）。
- 不引入新的磁盘探测/预留系统调用（保留现有配额账本）。
- chunked 改动不迁移独立 .chunk 续传格式（新格式：临时名+bitmap 续传）。
