# 计量报告（11.10-⑩）

## 背景/目标
- 现状：`pkg/server/metrics.go` 只有**进程内全局 atomic 计数器**（requests/bytes/files，无 owner 维度）与带标签运维指标（volume/mesh/hub）；`/api/stats` 是全局瞬时快照。`max_storage_bytes` 配额在配置中，但**没有 per-owner 周期用量导出**（quota 是总量约束，无「这个月每个 owner 用了多少」的报表）。
- 目标：新增 usage report 导出——per-owner 周期（日/月）用量聚合（上传/下载字节、文件数、存储占用、请求数），支持 CSV/JSON 导出端点 + 定时生成，供配额审计/成本分摊。

## 组件与接口
- 数据源策略（关键设计决策）：**从既有结构增量派生，不新建旁路台账**
  - 存储占用：per-owner 可由 searchIndex 快照（`ensureOwner` 后汇总 size，owner 维度天然对齐）或 storage.Tenant 现有统计（落地时核对 `pkg/server` 的 storage_manager/checksum 台账是否有 per-owner 计数）——设计优先用**索引快照汇总 + checksum 台账一致性校验**。
  - 传输字节/文件数/请求数：`Metrics` 加 owner 维度。`metricsMiddleware` 已捕获状态码；`RecordUpload/Download` 目前无 owner——**新增 owner 维度计数**（`recordUploadForOwner(owner, bytes)` 等，经 `labeledCounters[ownerKey]` 复用现有模式，基数=owner 数，受配置界定）。
- `pkg/server`：
  - `usageStore`（新）：内存环（保留 N 周期）+ 周期落盘快照（`<storage_root>/meta/usage/<owner>/<YYYY-MM>.json`，原子写）；`RecordUsage(owner, kind, n)` 每日累计，跨日/跨月轮转。
  - `GET /api/usage/report?owner=&from=&to=&format=csv|json`：导出端点（owner 空 = 全部；格式默认 json）；权限：`api_keys` 管理员/Bearer 或 SproxySig owner 自查询（复用 auth 语义，只允许 owner 看自己或管理员看全部）。
  - `GET /api/usage/summary?owner=`：周期汇总（配额使用率 = usage/max_storage_bytes）。
  - 定时：`usage.interval`（默认每日 0 点轮转，`cron` 语义简化实现为 ticker + 启动时补轮转）。
- `Metrics` owner 维度接线：`Metrics` 增 `usageUpload/usageDownload *labeledCounters[ownerKey]`；`MetricsHandler` 输出 `sproxy_usage_upload_bytes_total{owner=...}` 等（Prometheus 面同步暴露，运维可对账）。

## 数据流
请求处理路径（upload/download 成功）→ `RecordUsage(owner, "upload_bytes", n)`（从 handler 上下文取 owner——authMiddleware 已写入 `reqActor`）→ usageStore 日桶累计 → 周期轮转落盘 → 报告端点读 store 聚合（日/月桶求和）→ CSV/JSON 序列化。

## 错误处理
- owner 不存在/无数据 → 空结果（200 + 空数组，不 404——报告语义）；from/to 非法 → 400；导出格式非法 → 400；落盘失败 → 日志 WARN（内存仍可读，降级为不持久）；配额超限本身仍走既有 413 路径（报告只是观测面，不改配额执行逻辑）。

## 测试+变异点
- usageStore 单测：日/月轮转、跨周期求和、快照往返、CSV 转义（逗号/引号/换行）；handler：权限矩阵（owner 自查询/管理员全量/越权 403）、from/to 过滤、格式协商。
- 变异验证：① 轮转逻辑不跨日 → 跨日累计错误测试红；② CSV 不转义 → 注入用例红；③ owner 越权检查缺失 → 403 用例红。

## 片划分
- 片1：`usageStore` + 周期轮转 + 落盘（纯 pkg/server 单测）。
- 片2：`Metrics` owner 维度 + Prometheus 输出 + handler 导出端点（权限矩阵测试）。
- 片3：docs/usage.md + config.example.yaml（`usage.interval`）+ 若配额审计面板文档并入。

## 风险与零回归
- 默认不启用（`usage.enabled: false` 零回归，不产生落盘与计数开销）；启用后 owner 计数只增不改既有全局计数语义（两套并存，全局仍是 Snapshot 契约）；labeledCounters 模式复用（基数受 owner 数界定，与 meshDial 同族）；不改配额执行路径（413 行为不变）。
