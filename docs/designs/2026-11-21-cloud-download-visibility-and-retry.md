# 云端下载：可见性、人工/自动重试与成功率设计

> 状态：设计（待评审）｜日期：2026-11-21｜触发：修复 24 个受损文件时，任务失败后**从列表消失**、
> 无法人工重试、也看不出"已经下载了多少/部分文件在哪"。

## 1. 现状与问题（均已在代码中核实）

| # | 现象 | 根因（代码事实） |
| --- | --- | --- |
| 1 | 失败任务 1 小时后**从列表消失**，无从重试 | `FailedTaskTTL` 默认 **1h**（`pkg/cloud/manager.go:195`），过期即删 |
| 2 | 重试**依赖 task id**，任务没了就没法重试 | 只有 `POST /api/cloud/tasks/{id}/resume` + `sclient cloud-download resume-download <id>`（`pkg/server/cloudtasks/download.go:420`） |
| 3 | 失败后**看不到进度/部分产物** | 失败即终态，`progress` 不持久化；cloud 桶里的 partial 与任务记录无关联 |
| 4 | 失败**不进事项中心**，无退避重试、无"达上限转人工" | `pkg/server/issues` 已有 `KindUploadFailed`（上传/转存失败，可自动重试）但**没有 cloud 下载失败类型**，链路未接入 |
| 5 | 单源失败即整体失败 | 每个文件只尝试 `extra.source_url` 一条来源；无多源候选、无内容校验后的换源 |
| 6 | 批量场景无编排 | 只能逐条 `cloud-download`，失败即中断，无汇总报告/断点续跑 |

> 补充事实：重启时 `pending`/`started-but-queued` 任务会被标 `failed`（`interrupted before start or while queued; resume to retry`，
> `pkg/cloud/manager_persist.go` failOrphanPendingTask），且占位不会自动回拨 ⇒ 同 URL 在本次进程内无法再下载，直到 TTL 过期。

## 2. 目标

1. **可见**：任意时间都能看到任务（含失败）与其进度快照、部分产物、失败原因分类、尝试次数。
2. **可重试**：人工可对"已消失的失败任务"按 **task id / URL / 目标路径 / 组** 重新提交；一键、幂等。
3. **可自动**：失败自动进入事项中心，按错误类别退避重试，达上限转人工（复用既有 issues 机制）。
4. **高成功率**：多源候选 + 完成后强制校验（checksum）+ 失败换源 + 分块续传。
5. **可观测**：指标 + 任务列表 `--watch` + 批量汇总报告。

## 3. 设计

### 3.1 任务保留与可见性（P1）
- `FailedTaskTTL` 默认 **1h → 7d**（可配）；`cleanupExpired` 只清理**文件**，**保留任务记录**（标记 `artifacts_purged`）。
- 新增 `TaskDiagnostics`（随任务持久化）：
  ```go
  type TaskDiagnostics struct {
      Stage          string    // resolving/downloading/verifying/transferring
      DownloadedBytes int64
      TotalBytes     int64
      Attempts       int
      LastErrorClass string    // source_unreachable|throttled|content_mismatch|target_conflict|disk_full|auth
      LastErrorAt    time.Time
      LastError      string
      Artifacts      []Artifact // cloud 桶内 partial/最终文件：路径、大小、checksum、是否校验通过
  }
  ```
- `sclient cloud-download list --status failed|all --since 7d`（默认展示 failed 与 running）。

### 3.2 人工重试（P2）
- `POST /api/cloud/tasks/retry`（body：`task_id` | `url` | `transfer_path` | `group_id`，`force` 可选）。
  实现要点：先用 task id 取**持久化的原始提交参数**（URL/target_file/transfer/meta_extra），原任务的
  `status` 允许 `failed/cancelled/expired`；重建新任务（新 id），保留 `retry_of` 指向原任务便于追溯。
- CLI：`sclient cloud-download retry <task-id>`、`sclient cloud-download retry --url <url> [--transfer-path <p>]`。
- 幂等：同 `(url, transfer_path)` 已有 running 任务 ⇒ 直接返回该任务（不重复下载）。

### 3.3 自动重试 + 事项中心（P3）
- 新事项类型 `KindCloudDownloadFailed`（`pkg/server/issues/issues.go`）：
  - 登记约定：`Volume`=目标卷、`Path`=目标 rel、`Detail`=URL/原因/尝试次数/进度（稳定 key ⇒ 合并同类）。
  - 执行器（复用 `ExecutorDeps` 模式）：调 3.2 的 retry 内核；**按错误类别**退避
    `source_unreachable|throttled`：2m→5m→15m→30m→1h→3h（上限 6 次）；`content_mismatch`：立即换源；
    `disk_full|auth`：**不自动重试**，直接转人工。
  - 达上限 ⇒ `needs_human`（与 `KindBlobPendingVerify` 同语义，UI/CLI 可见）。
- 与既有恢复（R37/WAL）一致：任务恢复仍走 `pkg/recovery`，不新造队列。

### 3.4 成功率提升（P4）
1. **多源候选**：`meta.extra.source_url` 若只有一条，则在 resolve 阶段收集**同分享内的同内容候选**（同 size+同 GCID/hash）；
   保留 `extra.fallback_sources[]` 供失败后自动尝试下一条。
2. **完成后强制校验**：下载产物 checksum 与**期望值**比对（修复场景取目标 meta 的 `checksum`；新任务取 resolve 得到的 hash）；
   不一致 ⇒ `content_mismatch` ⇒ 自动换源（不落盘覆盖，先校验再落盘）。
3. **分块续传**：`Save=true` 时保留 cloud 桶 partial 并在 `Artifacts` 记录已下载区间；重试复用，不从头下。
4. **源健康与限流**：按 host 维护失败率/速率滑窗，自动降低并发（复用 `pkg/server/ratelimit` 思路），失败率超阈值时暂停该源。
5. **目标冲突预检**：目标已存在且内容不同 ⇒ 先入 `needs_human` 或显式 `--overwrite`（避免像本次"覆盖失败悄无声息"）。

### 3.5 观测与批量（P5）
- 指标：`cloud_task_failed_total{reason}`、`cloud_download_retry_total{result}`、`cloud_download_partial_bytes`。
- `sclient cloud-download list --watch`（每 5s 刷新，显示阶段/字节/速率/重试次数）。
- 批量：`sclient cloud-download batch --file repair_plan.tsv --concurrency 2 --verify --continue-on-error`，
  完成输出汇总（成功/失败/跳过 + 每个失败的原因分类与建议动作）。

## 4. 分期与验收

| 期 | 内容 | 验收（含 TDD/变异） |
| --- | --- | --- |
| P1 | 失败保留 7d + 诊断字段 + list 过滤 | 失败任务 24h 后仍可见；诊断字段持久化（重启后仍在）；变异：把 TTL 改回 1h ⇒ 用例红 |
| P2 | retry API/CLI（按 id/url/path） | 过期失败任务可重建并跑通；幂等（同目标不重复）；变异：去掉幂等 ⇒ 红 |
| P3 | `KindCloudDownloadFailed` + 执行器 + 分类退避 | 模拟 source_unreachable 3 次后成功；disk_full 直接 needs_human；变异：去掉退避 ⇒ 红 |
| P4 | 多源 + 校验后再落盘 + partial 复用 | 多源场景第一条失败自动切第二条并校验通过；变异：跳过校验 ⇒ 红 |
| P5 | 指标 + watch + batch | batch 遇失败继续并汇总；指标可查；变异：fail-fast ⇒ 红 |

## 5. 对本次 24 个受损文件的即时处置

1. 用现有 `cloud-download` 单条重试（keepshare 时段性问题，建议 1–2 并发、失败后 15–30 分钟再试）。
2. P2 落地后改用 `cloud-download retry --url ... --transfer-path ...`（不依赖历史 task id）。
3. 全部完成后 `volume meta-audit --refresh` 复核 `missing_chunks` 归零；期间**不做任何清理类操作**。
