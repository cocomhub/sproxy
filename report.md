# REPORT：智能运维 LLM 根因建议（roadmap 11.9-⑥）

## 状态

**DONE**（PR #573 已创建，CI 全绿）

## Commit

- `89c3de99` feat(ai): 智能运维 LLM 根因建议——llmgate 网关 + AlertEngine 接线 + ai.enabled 默认关

## 改动文件（9 个，+785）

| 文件 | 说明 |
|------|------|
| `pkg/llmgate/llmgate.go`（新） | LLM 网关客户端：POST OpenAI 兼容 `/chat/completions`；`netutil.IsolatedTransport` 基座 + ResponseHeaderTimeout；响应体 64KiB 截断（LimitReader）；Timeout 默认 10s；APIKey 只进 Authorization Bearer 头 |
| `pkg/llmgate/ai_test.go`（新） | llmgate 单测 6 例：200 解析 content（trim）/ 非 2xx / 畸形 JSON / 超大响应截断 / 超时 / 空 content |
| `pkg/server/ai_advisor.go`（新） | `AIAdvisor`（enabled=false 或无 key → gate=nil 恒空）；prompt 构造纯函数（system 固定、user 仅数据）；建议截断 ≤1000 字；`SetAdvisor` 注入点 + `newAIAdvisorFromConfig` 装配（key 空 → Warn 可观测降级） |
| `pkg/server/ai_advisor_test.go`（新） | server 接线测试 6 例：Enabled 追加建议 / NoKey 回退模板 / 网关失败仍通知 / Disabled 快照 / DispatchCallsAdvisor 调用计数 / ConfigValidate |
| `pkg/server/alerts.go` | `AlertEngine` 增加 `advisor` 字段 + `dispatch` 同步调用 `Advise`（失败返回 "" 追加为空 = 固定模板原样） |
| `pkg/server/notify.go` | `NotifyConfig` 增加 `AIAdvisor AIAdvisorConfig` 子段（yaml `ai_advisor`） |
| `pkg/server/config_validate.go` | `api_key_ref` 非法环境变量名拒绝；空 + enabled=true 允许（运行期降级） |
| `pkg/server/routes.go` | 告警引擎装配处 `newAIAdvisorFromConfig` → `SetAdvisor` |
| `docs/roadmap.md` | 11.9 第二层第 6 项标记「已落地 2026-09-24」+ 11.14 补设计批加实施说明 |

## 测试证据

```text
go test -count=1 -race ./pkg/llmgate/ ./pkg/server/  → ok（llmgate 2.1s / server 50.9s）
go test -count=1 ./internal/archcheck/              → ok（10.4s，R18/R14/分层全绿）
make build                                          → 通过（sproxy + sclient）
make lint                                           → 0 issues
goimports -l / gofmt -l                             → 无输出
```

## 变异命中

- `SetAdvisor` 实现改为 `e.advisor = nil` → `TestAlertEngine_DispatchCallsAdvisor` **红**（"fire 应恰好调用一次 Advise，got 0"）→ 还原 `e.advisor = a` → 绿。证明测试能抓「LLM 调用接线丢失」。

## TDD 红灯先行

- llmgate 测试先行编译失败（undefined New/Config/maxResponseBytes）→ 实现后绿。
- server 测试先行编译失败（undefined AIAdvisor/NewAIAdvisor/NotifyConfig.AIAdvisor 等）→ 实现后绿。

## PR

https://github.com/cocomhub/sproxy/pull/573（OPEN，**CI 全绿**：Build×6 + E2E×2 + Lint + SonarQube + Test×2 + Test Sub-Modules + UI E2E + Conventional Commits + Detect docs-only 全部 pass）

## 环境备注

- golangci-lint v2.13.2 的文件锁与其它并行 worktree 会话的 pre-commit 存在竞争（`parallel golangci-lint is running`）——已用「等待 golangci 进程连续空闲 ~90s + 独立 GOLANGCI_LINT_CACHE」方式完成提交。
- 临时并行配置 `.golangci.parallel.yml` 已删除（不入提交）。

## 残余（片4，不在本期）

Anthropic 原生 messages API 专路、建议缓存/去抖（同 key 窗口不重复调用）、`/api/ai/advisor` 手动触发端点；api_key_ref 重启生效（SIGHUP 不热载，roadmap 已确认）。
