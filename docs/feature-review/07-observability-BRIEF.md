# 批次 7 审查：通知与可观测性

> 本批 2 路并发对抗审查：R7.1 指标+追踪+事件流 / R7.2 审计+通知中心。
> 基线：master `e428acbe`（含 #496 通知中心 + #497 metrics 认证）。产出文件 `07-observability-*.md`。

## 审查目标功能（roadmap 7.1 + 7.3 已落地）

- **R7.1 指标 + 追踪 + 事件流**：
  - 指标：40+ `sproxy_*` Prometheus 指标入 `/metrics`（文件/云下载/mux 重传与流控/卷 I/O 与迁移
    进度/带宽限速/拨号回退/缓冲水位调整）；`/metrics` 加认证（`metrics_token`，query/Bearer 常量
    时间比较）；xfer TCP 连接级指标（sproxy_xfer_tcp_conns/messages/bytes_*）。
  - 追踪骨架：telemetry span + slog + traceparent 传播 + OTLP 导出（telemetry.enabled + otlp_endpoint）。
  - 事件流：/api/events EventSource 文件变更流（upload/delete/rename/version/share，游标可回放），
    Web UI 实时刷新。
- **R7.2 审计 + 到期提醒 + 通知中心**：
  - 审计：环形缓冲 + 落盘（audit.log 原子 append）+ 过滤查询/导出。
  - 到期提醒：SK 轮换提前量（credentials.rotation.notify_before）——仅进程内日志告警。
  - 通知中心（#496）：NotifyCenter（Register 注册表 + rules action glob 路由 + 去抖窗口 + 指数
    退避重试 + 有界历史 /api/notify/history + 渠道自检 /api/notify/test）；wecom/serverchan 双渠道；
    事件源 = RecordAudit 异步 dispatch。
  - metrics 认证（#497）：metrics_token（query/Bearer 常量时间比较，仅门 /metrics 零回归）。

## 关键文件

- R7.1：pkg/server/metrics.go、pkg/server/metrics_auth_test.go（#497）、pkg/tunnel/tracing/*.go、
  pkg/server/events.go（EventSource 游标回放）
- R7.2：pkg/server/audit*.go、pkg/server/credential_rotation.go、pkg/server/notify.go +
  notify_test.go（#496）、pkg/server/routes.go（notify 端点）

## 审查维度与关注点

### 正确性
- metrics_token 常量时间比较实现（长度泄漏？）；query/Bearer 双通道一致性
- 通知中心去抖/重试/历史的有界性（内存泄漏？）；rules action glob 匹配语义
- 事件流游标回放（断线重连、游标持久化、事件顺序）

### 安全性（本批重点）
- /metrics 未认证时信息泄漏面（40+ 指标暴露内部拓扑？）；metrics_token 泄露风险
- 审计落盘权限（audit.log 可被篡改/删除？）；审计行是否含敏感字段（AK/SK/密码）
- 通知中心 webhook 目标 SSRF（wecom/serverchan URL 是否用户可控）
- 事件流鉴权（谁能订阅 /api/events？跨 owner 事件泄漏）

### 可用性 / 可维护性
- 通知渠道失败恢复；指标命名/文档一致性（40+ 指标是否全部有文档）
- 测试覆盖（notify_test/metrics_auth_test 是否覆盖分支）

## 输出格式

每路审查产出 `07-observability-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
