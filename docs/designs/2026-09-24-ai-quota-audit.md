# AI 配额/审计（11.9-⑦）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：/api/ai/* 调用记账（审计事件 + 配额扣减）
> 只读源码：pkg/server/audit_store.go（审计落盘）、pkg/server/notify.go（NotifyCenter 挂点先例——RecordAudit 末尾异步 dispatch）
> 衔接：[2026-09-24-ai-file-insight.md](./2026-09-24-ai-file-insight.md)、[2026-09-24-ai-vector-search.md](./2026-09-24-ai-vector-search.md)（被记账的调用方）

## 1. 背景与目标

- **现状**：`AuditStore`（内存全量 + append-only JSON lines 双写）与 `AuditRing`（内存环）已记录 `action/actor/mesh/object/result/detail/ts`；`NotifyCenter.Dispatch` 挂 RecordAudit 末尾（异步 goroutine）——**挂点模式现成**。
- **目标**：所有 `/api/ai/*` 与 AI 后台任务（向量 embedding、摘要、标签）调用统一记账：审计事件（`ai.*` action 族）+ **配额扣减**（per-owner 调用次数/预算，超限 429）。查询端点 `GET /api/ai/quota`（owner 自见）+ `GET /api/audit?action=ai.*`（已有 audit 端点过滤，扩展 action 前缀匹配）。
- **非目标**：不做计费/支付；不做跨节点配额一致（单节点 per-owner 内存计数器即可，集群配额属 cluster 后续片）；不做配额持久化迁移（重启可清零重计，文档注明——可选 P2 落盘）。

## 2. 组件与接口

### 2.1 审计事件扩展（`pkg/server/audit.go` 现有 AuditEvent/AuditFilter）

```go
// 新增 action 常量（现有 action 命名风格沿用）：
const (
    ActionAIEmbed     = "ai.embed"     // 向量 embedding（object = owner/rel）
    ActionAISummarize = "ai.summarize" // 摘要（object = rel）
    ActionAITag       = "ai.tag"       // 标签
)
// AuditFilter 扩展：Action 前缀匹配（action 字段带 "."，现有精确匹配扩展为
// "ai." 前缀族——`GET /api/audit?action=ai.` 返回全部 AI 调用）。
```

- **复用现有挂点**：`RecordAudit(evt)` 不变——AI 调用点在成功/失败分支各自 `RecordAudit`（result=ok/failed + detail=错误/耗时/tokens）。`NotifyCenter.Dispatch` 自动继承（规则可配 `action: ai.*` 通知）。
- **落盘**：`audit.persist_dir` 已配置时 AI 事件自然落盘（append-only 日志可回放——AI 成本审计审计面现成）。

### 2.2 配额（`pkg/server/ai_quota.go` 新文件）

```go
// AIQuota 是 per-owner AI 调用配额（内存计数器，进程内有效）。
type AIQuota struct {
    mu     sync.Mutex
    limits map[string]AILimit  // owner → 限制
    usage  map[string]AIUsage  // owner → 当前用量
    logger *slog.Logger
}
type AILimit struct {
    DailyCalls int // 每日调用次数上限（0 = 不限）
    DailyTokens int // 每日 token 估算上限（0 = 不限；按响应近似）
}
type AIUsage struct {
    Day  string // YYYY-MM-DD（按天重置）
    Calls int
    Tokens int
}
// CheckAndCharge 检查配额并扣减：超限返回 ErrQuotaExceeded（调用方 → 429）。
// 跨天自动重置（Day 与当前日期不符 → 清零）。
func (q *AIQuota) CheckAndCharge(owner string, estTokens int) error
// Usage 返回 owner 当前用量（GET /api/ai/quota 用）。
func (q *AIQuota) Usage(owner string) AIUsage
```

- **扣减点**：AIInsight.Summarize/Tag（file-insight I2 片）调用 `llmgate` **前** CheckAndCharge（estTokens = 抽样文本长度/4 近似）；向量 worker（vector-search V4 片）每次 EmbedBatch 前同样扣减。**查询端点不扣**（只读）。
- **审计与配额关系**：配额是前置门（429 不产生 LLM 调用，也不记 ai.* 成功事件——记 `result=quota_exceeded` 一条即可，防审计量放大）；审计是后置账（每真实调用一条）。
- **配置**（config.go 新增 `ai.quota` 段）：

```go
type AIQuotaConfig struct {
    Enabled   bool          `yaml:"enabled"`    // 默认 false 零回归
    Default   AILimit       `yaml:"default"`    // 未显式配置 owner 的兜底限制
    Overrides map[string]AILimit `yaml:"overrides"` // per-owner 覆盖
}
```

### 2.3 端点

```go
// GET /api/ai/quota → 200 {owner, calls, tokens, limit, day}（受认证，owner 自见；
// 管理员可 ?owner= 查他人——与既有 owner 隔离语义一致：仅 admin 凭据可越权）
// 429 响应体：{"error":"ai quota exceeded", "usage":{...}}
```

## 3. 数据流

```
POST /api/ai/summarize?filename=...
  → AIQuota.CheckAndCharge(owner, estTokens)
      → 超限 → 429 + RecordAudit{ai.summarize, result=quota_exceeded} → 结束
  → llmgate.Advise 成功 → RecordAudit{ai.summarize, result=ok, detail=tokens/耗时}
  → 失败 → RecordAudit{ai.summarize, result=failed, detail=err}（502，见 file-insight）
  →（notify 规则匹配 ai.* 时异步通知）

后台向量 worker：
  EmbedBatch 前 CheckAndCharge → 超限 → audit(ai.embed, quota_exceeded) + 跳过该批（周期兜底重试）
```

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| 配额未启用 | CheckAndCharge 恒通过（Enabled=false → nil 配额）零回归 |
| 超限 | 429 + quota_exceeded 审计（不调 LLM，不重复扣） |
| 跨天 | Day 不匹配 → 清零重计（惰性重置） |
| 并发扣减 | mutex 串行（AI 调用低频，无性能问题） |
| 审计落盘失败 | 现有语义：内存照记 + Warn（audit_store 尽力而为） |
| 配额计数器重启丢失 | 文档注明（P2 可落盘） |

## 5. 测试 + 变异点

1. `TestAIQuota_CheckAndCharge`：限 3 次 → 第 4 次 ErrQuotaExceeded（**变异：超限不拦 → 红**）。
2. `TestAIQuota_DailyReset`：Day 变更 → 清零（注入时钟/改 Day 字段——**变异：不重置 → 红**）。
3. `TestAIQuota_DisabledNoop`：Enabled=false → 恒通过（零回归断言）。
4. `TestAIQuota_Overrides`：per-owner 覆盖生效（**变异：忽略 overrides → 红**）。
5. `TestAIInsight_QuotaExceeded_429`：insight 端点超限 → 429 + 不调 mock 网关（断言 mock 调用数=0——**变异：仍调网关 → 红**）。
6. `TestAIInsight_SuccessRecordsAudit`：成功调用 → RecordAudit 恰好一次（fake recorder 计数；**变异：不记 → 红**）。
7. `TestAuditFilter_AIPrefix`：`action=ai.` 前缀匹配族（**变异：精确匹配不扩前缀 → 红**）。
8. `TestQuotaEndpoint`：GET /api/ai/quota 认证 + owner 自见 + 429 响应体形状。
9. 集成：insight 全链路 → audit 落盘（persist_dir）→ 重载 AuditStore 可查到 ai.summarize 记录。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **Q1** | AIQuota（限制/用量/日重置/overrides）+ 单测 1–4 | 单测绿 | 无 |
| **Q2** | ai.* 审计事件 + AuditFilter 前缀扩展 + 单测 7 | 单测绿 | 无 |
| **Q3** | AIInsight/向量 worker 扣减接线 + 端点 /api/ai/quota + 单测 5–6、8 | 单测绿 | Q1、Q2、I2、V4 |
| **Q4** | 集成 9 + docs/config.md `ai.quota` 段（R15） | 集成绿 + 文档门禁绿 | Q3 |

Q1 ∥ Q2；Q3 依赖两者；Q4 最后。

## 7. 风险与零回归保证

- **零回归**：`ai.quota.enabled=false` → AIQuota 不装配（nil）→ CheckAndCharge 恒通过 → AI 功能不受影响；审计扩展只增 action 常量与前缀匹配，既有 `action=upload` 精确语义不变（向后兼容：空 action=全部，现有调用无 action 参数不受影响——**需回归确认**：AuditFilter 若 action 参数现在走精确匹配，加前缀匹配必须不改变空/精确行为）。
- **成本失控风险**：默认关闭 + per-owner 日限 + 429 前置门（超限不调 LLM）→ 账单有界。
- **审计量放大风险**：quota_exceeded 只记一条（不每次请求刷屏）；后台 worker 批处理（一批一条）→ 审计量可控。
- **时钟/重置风险**：按本地日重置（注入时钟可测）；集群部署多节点各有独立计数器——文档注明（集群配额后续片）。
- **准确性风险**：tokens 是估算（文本/4 近似）——文档注明「估算配额，非精确计费」。
