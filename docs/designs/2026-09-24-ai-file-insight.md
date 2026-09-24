# AI 文件洞察（11.9-⑤）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：/api/ai/summarize + /api/ai/tag（LLM 网关）
> 引用：[2026-09-24-ai-advisor.md](./2026-09-24-ai-advisor.md)（pkg/llmgate 已设计——本设计直接复用其 Client）
> 只读源码：pkg/files/search_index.go（文本抽样）、s3_server.go/dav.go（旁路写先例）、notify.go（异步 goroutine 先例）
> 衔接：[2026-09-24-ai-quota-audit.md](./2026-09-24-ai-quota-audit.md)（调用记账）、[2026-09-24-ai-privacy.md](./2026-09-24-ai-privacy.md)（落盘加密）

## 1. 背景与目标

- **现状**：无任何 LLM 调用端点；`pkg/llmgate` 已在 ai-advisor 设计（OpenAI 兼容 chat/completions，标准库 + netutil，10s 超时，64KB 响应上限）。
- **目标**：`POST /api/ai/summarize?filename=`（文件 → LLM 摘要）与 `POST /api/ai/tag?filename=`（文件 → 标签列表），结果落盘缓存（at-rest 加密，ai-privacy）供重复查询；**默认关闭**（`ai.insight.enabled=false`），无 key/网关失败 → 明确错误（非静默降级）。
- **非目标**：不做对话/多轮；不做多模态（图片视频不入 LLM 文本——抽样失败返回明确错误）；不做 RAG 组装。

## 2. 组件与接口

### 2.1 复用 `pkg/llmgate.Client`

直接复用 ai-advisor §2.1 的 `llmgate.Client.Advise(ctx, system, user) (string, error)`（OpenAI 兼容 `/v1/chat/completions`）。差异点：文件洞察是**用户显式触发**（非告警后台），失败语义从「回退模板」改为「返回错误给调用方」——同网关、不同错误策略。

### 2.2 `pkg/server/ai_insight.go`（新文件）

```go
type AIInsight struct {
    gate  *llmgate.Client // nil = 未启用
    cache *InsightCache   // 摘要/标签缓存（落盘加密，ai-privacy）
    logger *slog.Logger
}
func NewAIInsight(gate *llmgate.Client, cache *InsightCache, logger *slog.Logger) *AIInsight

// Summarize 生成文件摘要：抽样文本（≤4KiB，复用 files 抽样逻辑）→ LLM → 截断（≤2000字）→ 缓存。
func (a *AIInsight) Summarize(ctx context.Context, owner, rel string) (string, error)
// Tag 生成标签：同一抽样文本 → LLM 输出逗号分隔标签 → 规范化（≤10 个，每 ≤20 字符）→ 缓存。
func (a *AIInsight) Tag(ctx context.Context, owner, rel string) ([]string, error)
```

- **抽样文本源**：复用 `search_index.go` 的 `sampleTokens` 语义——首 4KiB 文本（`root.OpenDecrypted(rel)`，经 at-rest 解密读取）。非文本（抽样为空）→ 明确错误 `ErrNotTextFile`（400）。
- **prompt**：system 固定（摘要助手/标签助手角色 + 输出约束）；user = 文件名 + 抽样文本。**文本作为消息数据传递**（API 层隔离，无拼接注入面）。
- **缓存**：`InsightCache`——key `(owner, rel, kind)` → 结果 + 源文件 mtime/rev；mtime 未变命中缓存（零 LLM 调用）；文件变更 → 缓存失效（ai-event-pipeline 事件路由删除）。落盘：`<tenant>/meta/insight/<owner>.bin`（gob + 加密，ai-privacy 加密卷）。

### 2.3 端点

```go
// POST /api/ai/summarize?filename=<rel> → 200 {summary} | 400 非文本 | 401 | 502 网关失败
// POST /api/ai/tag?filename=<rel> → 200 {tags:[...]} | 同上
// GET /api/ai/summarize?filename=<rel> → 200 命中缓存直接返回（可选，P2）
```
- 受既有 authMiddleware 保护；owner 隔离（只能洞察自己文件——`ownerFromRequest` + `ValidateFilePath` + tenantFor 同现有 handler 模式）。
- **失败语义**：`llmgate` 调用失败（超时/非2xx/解析失败）→ 502 + 错误信息（不是静默空结果——洞察是显式用户操作，必须明确告知）。不缓存失败。

### 2.4 配置（config.go 新增 `ai.insight` 段）

```go
type AIInsightConfig struct {
    Enabled   bool   // 默认 false 零回归
    Provider  string // openai | anthropic | ollama（复用 llmgate Provider）
    APIKeyRef string // 环境变量名（SPROXY_AI_API_KEY 惯例）
    Model     string
    Timeout   time.Duration // 默认 30s（比告警宽松——用户显式等待）
    CacheTTL  time.Duration // 默认 24h
}
```

## 3. 数据流

```
POST /api/ai/summarize?filename=notes.txt
  → 认证 + 路径校验 → 租户根 → 检查缓存（mtime 未变 → 直接返回）
  → 未命中 → OpenDecrypted 抽样首 4KiB → 空 → 400 ErrNotTextFile
  → llmgate.Advise(system, user) → 截断 → 落缓存（加密）→ 200 {summary}
  → 失败 → audit 事件（ai.summarize result=failed）+ 502
每次调用 → RecordAudit（ai.summarize / ai.tag）+ 配额扣减（ai-quota-audit）
```

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| 未启用/无 key | 装配期 Warn（可观测降级）；端点 400「AI 洞察未启用」 |
| 网关失败/超时 | 502 + 明确信息；不缓存；audit 记 failed |
| 非文本文件 | 400 ErrNotTextFile（抽样为空） |
| 文件不存在/权限 | 404/403（现有 handler 语义） |
| 大文件 | 只抽样首 4KiB（成本有界，文档注明局限） |
| 缓存损坏 | 忽略 + 重新生成（幂等） |

## 5. 测试 + 变异点

1. `TestAIInsight_SummarizeCaches`：mock 网关 → 首次调用 LLM 一次 → 二次命中缓存零调用（**变异：不落缓存 → 红**）。
2. `TestAIInsight_MtimeInvalidatesCache`：文件 mtime 变 → 缓存失效重新调用（**变异：mtime 不参与 key → 红**）。
3. `TestAIInsight_GatewayFailure_Returns502`：mock 网关 500 → 502 + 不缓存（**变异：失败也缓存 → 红**）。
4. `TestAIInsight_NotTextFile`：空抽样 → 400（**变异：空文本也调 LLM → 红**）。
5. `TestAIInsight_TagNormalization`：LLM 输出乱格式 → 规范化 ≤10 标签（**变异：原样返回 → 红**）。
6. `TestAIInsight_Disabled`：enabled=false → 端点 400（零回归：未装配行为明确）。
7. `TestAIInsight_OwnerIsolation`：A 查 B 文件 → 403/404（**变异：跨 owner → 红**）。
8. `TestAIInsight_PromptStructure`：注入固定抽样文本 → 断言请求体 user 消息含文件名+文本、system 固定（防 prompt 注入回归）。
9. 集成：真实 llmgate + 假 HTTP 网关 → 全链路（上传 → summarize → 缓存命中）。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **I1** | InsightCache（key/mtime 失效/加密落盘）+ 单测 1–2、4 | 单测绿 | ai-privacy（加密） |
| **I2** | AIInsight（prompt/截断/标签规范化）+ 单测 3、5 | 单测绿 | I1、ai-advisor（llmgate） |
| **I3** | 端点 + 装配 + 配置 + 单测 6–8 + docs/config.md | 单测绿 + 文档门禁绿 | I2、ai-quota-audit（记账接线） |
| **I4** | 集成 9 + 缓存失效事件接线（ai-event-pipeline） | 集成绿 | I3、E1 |

I1→I2→I3；I4 ∥ I3。

## 7. 风险与零回归保证

- **零回归**：默认 `ai.insight.enabled=false` → AIInsight 不装配、端点 400（不暴露新面）；llmgate 是 ai-advisor 新包共用，无既有行为改动。
- **成本风险**：缓存 + mtime 失效 + 显式触发（非自动全文件）→ 调用量有界；文档注明。
- **内容外发风险**：文件抽样文本发送外部 LLM = 配置即同意（enabled 显式开启 + 文档明示）；APIKeyRef 环境变量不落盘（对齐 ai-advisor 同款）。
- **质量风险**：首 4KiB 摘要局限 → 文档注明；截断 ≤2000 字防超长响应。
- **网关共享风险**：与 ai-advisor 共用 llmgate.Client——各自独立构造（不同超时/模型），互不影响；测试各自 mock。
