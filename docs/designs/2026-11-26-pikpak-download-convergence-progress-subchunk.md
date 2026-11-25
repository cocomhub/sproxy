# PikPak 下载：内核收敛 + 细粒度进度观测 + 子块续传设计

> 日期：2026-11-26｜触发：① sproxy 与 pikget 行为分叉；② 云端任务缺详细进度观测；
> ③ chunk(32MiB) 粒度续传浪费（接近完成的 chunk 被整块重下）。

## 1. 内核收敛（sproxy = pikget 的长期运行版本）

现状：两者**共用** `pkg/volume/ext/pikpak`（`HybridDownloader`），分片规划/manifest/GCID 校验/416 探测/
账号回落/WAL 均为同一实现；默认参数一致（ChunkSize 32MiB、Concurrency 4、ShareRatio auto、AutoDelete true）。

已识别 4 处行为差异与收敛动作：

| # | 差异 | 收敛动作 |
| --- | --- | --- |
| 1 | `Rebuild`（零尾扫描 + 定点补片）只在 pikget 有调用方 | 云端 damaged 恢复改调 `Rebuild`（共享实现，已有）；失败再退整份重下 |
| 2 | `ResetOnGCIDMismatch` sproxy 硬编码 true、pikget 未暴露 | 配置化（`pikpak.hybrid.reset_on_gcid_mismatch`）+ pikget `--reset-on-gcid-mismatch` |
| 3 | `Fallback`（hybrid 失败降级旧下载器）仅 sproxy | pikget 同接（同一兜底口径） |
| 4 | `ChunkProgress`/`Metrics` 仅各自一侧 | 统一：内核回调 + 服务端指标适配器（观测见 §2） |
| 5 | 提交同 URL 只复用 `pending/downloading`；`failed/cancelled/completed` 一律新建从 0 | 提交时命中 7d 内同参数 `failed/cancelled` ⇒ 复用 + ResumeTask；命中 `completed`（目标+期望 checksum 一致）⇒ 幂等短路返回 |
| 6 | 无机器化防漂移 | 加测试：断言两侧装配的关键参数集合一致（ChunkSize/Concurrency/ShareRatio/AutoDelete/Journal/Reset） |

## 2. 详细进度观测（打点 + 图表）

### 2.1 数据模型（内核产出，服务端透传，前端消费）
```go
// ProgressSample 是一次采样点（内核每 N 秒或每完成一个子块聚合时打点）。
type ProgressSample struct {
    At        time.Time
    Downloaded int64   // 已落盘字节（真值，非估算）
    Total      int64
    RateBps    int64   // 近窗口速率（滑动窗口，例如最近 10s）
    Stage      string  // resolving|downloading|verifying|transferring
    Source     string  // share | acct:<name> | mixed
}
// TaskProgress 随任务持久化（环形缓冲，例如最近 720 点 = 2 小时 @10s）。
```
- 打点位置：`runChunks` 每子块完成、账号回落切换、校验/转存阶段切换、失败收口。
- 持久化：随任务 JSON（环形缓冲 ≤64KiB/任务，超限丢最旧）⇒ 重启后历史仍在。
- 指标：`sproxy_download_bytes_total{volume,source}`、`sproxy_download_rate_bps`、
  `sproxy_download_chunk_state{state}`（pending/running/done/failed）。

### 2.2 页面（v2）
- 任务行展开：**迷你波形**（rate）+ 迷你块矩阵（进度条内嵌小块）+ 阶段时间轴（开始/结束/耗时）。
- 详情面板：速率折线（近 30 分钟）+ 累计字节/预计剩余 + 来源占比（share vs acct）+ 最近错误。
- 实现：纯前端（SVG/canvas，无新依赖），数据来自 `GET /api/cloud/tasks/{id}/progress?since=<ts>`
  （服务端只回新增样本，避免全量拉取）。

## 3. 子块续传（chunk 内 MB 级）

### 3.1 问题
当前恢复粒度 = chunk（32MiB）。一个 chunk 若已下 30MiB 就中断，重试要重下 32MiB ⇒ 浪费 ~94%。

### 3.2 设计（分层位图 + 连续聚合）
- 每个 chunk 维护 **子块位图**：`subBlockSize = 4MiB`（可配，默认 4MiB；32MiB chunk = 8 个子块）。
- 下载时按子块写入并**立即标记**（位图随 manifest 持久化，替代"chunk 完成才算"）。
- 恢复时只请求 `!done` 的子块区间；连续未完成子块**合并成单个 HTTP Range**（减少请求数）。
- 兼容：manifest 版本 +1；旧 manifest（无位图）视为"整 chunk 未完成"，行为零回归。
- 观测：位图直接驱动 UI 的"小块矩阵"（见 §2.2），一行显示 8 个小块；多 chunk 时按行聚合。

### 3.3 正确性
- 子块边界必须与 GCID 分块**无关**（GCID 仍按 512KiB/1MiB/2MiB 候选复算整文件）；
- 校验仍在整文件完成时进行（子块只是传输/续传单位）；
- 写入使用 `WriteAt(offset)`，天然支持乱序与并发；位图与文件内容一致性由"写完即标记"保证
  （崩溃窗口：标记前崩溃 ⇒ 重下该子块，安全）。

## 4. 分期

| 期 | 内容 | 验收 |
| --- | --- | --- |
| A | 内核收敛 #1–#4 + 防漂移门禁 | 两侧参数集合断言；云端 damaged 走 Rebuild；pikget 有 reset flag |
| B | 提交复用/幂等（#5） | 失败>重提交不再从 0；completed 幂等短路（`reused=true`） |
| C | 进度打点 + 持久化 + HTTP 增量接口 + v2 图表 | 每任务可见速率曲线/阶段/来源占比；重启后历史仍在 |
| D | 子块位图续传（4MiB）+ 兼容旧 manifest | 中断后重试只补缺失子块（用例：造 30/32MiB 已完成 ⇒ 仅请求 2MiB） |
| E | 页面块矩阵（小块聚合，一行多块） | 一屏可显示≥8 行×32 块，不出现"行数爆炸" |

## 5. 风险与回退
- 子块位图改变 manifest 语义 ⇒ 必须有 `manifest_version` 升级 + 旧版兼容分支（零回归）。
- 打点增加任务 JSON 体积 ⇒ 环形缓冲上限 + 采样降频（失败/空闲时 30s 一次）。
- 全部改动保持"一个 PR 一个维度"，每项带变异命中的用例。
