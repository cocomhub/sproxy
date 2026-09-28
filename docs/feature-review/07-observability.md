# 审查：通知与可观测性（指标/追踪/事件流/审计/通知中心）

- **批次**：7
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`（含 #496 通知中心 + #497 metrics 认证）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] 通知中心去抖键不含 Result（同 action+object 的失败与成功共享去抖窗口）
- **位置**：`pkg/server/notify.go:243-251`（dispatchTo 去抖）
- **问题**：去抖 key = `action + "\x00" + object + "\x00" + channel`——不含 evt.Result。同一 action+object 的「失败通知」与后续「成功通知」在同窗口内会被去抖跳过，用户可能收不到恢复/成功通知。
- **影响**：低——默认 debounce 窗口短（可配）；且阈值告警引擎（P1）会补「恢复通知」语义。但现状下失败→恢复同窗口会被吞。
- **建议**：key 加 `evt.Result` 段（失败/成功各自去抖）。

## 通过项（无问题面）

### 指标（#497）
- 40+ `sproxy_*` 指标 + **metrics_token 认证**（`subtle.ConstantTimeCompare`，query/Bearer 双通道，仅门 /metrics 零回归）+ xfer TCP 连接级指标（sproxy_xfer_tcp_conns/messages/bytes_*）。

### 追踪骨架
- telemetry span + slog + traceparent 传播 + OTLP 导出（telemetry.enabled + otlp_endpoint）。

### 事件流
- `/api/events` EventSource（`events.go`）：cursor 回放（`Replay(owner, lastCursor)` 断线重连恢复）+ **per-owner ring 隔离**（`ring(owner)`——订阅只收自己 owner 的事件，跨 owner 不可见）。

### 审计
- 环形缓冲 + 落盘（`audit_store.go` Append 原子 append audit.log）+ 启动载入 + 过滤查询/导出；审计行独立 JSON logger。

### 通知中心（#496）
- **注册表**：`RegisterNotifier`（同 RegisterBackend/RegisterTransform 模式）+ rules action glob 路由（`ruleMatch`：action 精确或 "*"、object 空=全部或精确）。
- **去抖/重试**：`dispatchTo` 窗口去抖 + `sendWithRetry` 指数退避（retry 次 + ctx/done 提前退出）。
- **历史有界**：`addHistory` historyCap 截断 + `History()` 倒序快照（防并发读写）。
- **异步 dispatch**：`Dispatch` 持锁匹配后解锁 + `wg.Go` 异步发送（不阻塞主路径）。
- **渠道**：wecom（markdown webhook）+ serverchan（sct_key form）+ email（SMTP，#496 后续片）+ webhook 通用（#496 后续片）；`httpClientForNotify` = IsolatedTransport 基座（R19 门禁合规）。
- **SSRF 面**：webhook URL 来自**受信配置**（`notify.channels.wecom.webhook`，非用户请求输入）——`//nolint:gosec` G704 注释明示（同云下载 provider URL 语义）。

### 到期提醒
- SK 轮换提前量（credentials.rotation.notify_before）——进程内日志告警（无主动外发，roadmap 已记残余）。

### 测试
- `notify_test.go`（#496）+ `metrics_auth_test.go`（#497）+ `events_test.go` + `audit_test.go`；`go test` 全绿。

## 验证方式

- 源码逐路径审查（通知中心全流程 + 事件流 owner 隔离 + 审计落盘 + metrics 认证）
- `go test -count=1 -timeout 180s -run 'TestNotify|TestMetrics|TestEvent|TestAudit' ./pkg/server/...` → **ok**
