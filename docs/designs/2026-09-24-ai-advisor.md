# 智能运维 LLM 设计（S3-⑥，roadmap 11.9-⑥）

> 状态：DRAFT（供 design-batch 审校）。只读源码：pkg/server/alerts.go、notify.go、config.go。

## 1. 背景与目标

**现状**：`AlertEngine` 触发告警（fire/recover）→ `dispatch` 组装**固定模板** `NotifyMessage{Title,Text}` → `NotifyCenter` 渠道分发（去抖/重试/历史）。告警只有事实描述（"磁盘水位 85% ≥ 阈值 80%"），无根因/处置建议，运维需人工排查。

**目标**：告警触发时经 LLM 网关（OpenAI/Anthropic 兼容接口）生成根因+处置建议文本，追加到通知正文；**无 key / 网关失败 → 回退固定模板**（fail-closed：绝不因 AI 失败吞掉通知）。默认关闭（`notify.ai_advisor.enabled=false`）零回归。

## 2. 组件与接口

### 2.1 `pkg/llmgate`（新包，标准库 HTTP + netutil）

```go
// 配置（构造期传入，只读）
type Config struct {
    Provider string // "openai"（默认）| "anthropic"；模型名/URL 归一在 New 内
    BaseURL  string // 空 = provider 默认（openai: https://api.openai.com/v1）
    APIKey   string
    Model    string // 空 = provider 默认（如 gpt-4o-mini）
    Timeout  time.Duration // 默认 10s
}

type Client struct { cfg Config; hc *http.Client /* netutil.IsolatedTransport 基座 + ResponseHeaderTimeout */ }

func New(cfg Config) *Client
// Advise 生成建议文本：构建 system+user 消息 → POST {provider}/chat/completions
// → 取 choices[0].message.content（trim）。
func (c *Client) Advise(ctx context.Context, system, user string) (string, error)
```

- **协议**：OpenAI 兼容 `/v1/chat/completions`（Anthropic 由 `New` 归一 BaseURL 到其兼容端点，最小实现走同一 JSON 形状）；仅支持 `message.content` 字符串形态（不做多模态/工具）。
- **HTTP**：`netutil.IsolatedTransport()`（R19 门禁：禁裸 `http.Transport` 字面量，与 `httpClientForNotify` 同型）；BaseURL 来自配置（管理员受信输入，SSRF 面=配置者自身，同 wecom webhook 语义注释）。
- 响应体上限保护（`io.LimitReader`，如 64KB）防恶意/畸形响应撑爆内存；非 2xx → error。

### 2.2 `pkg/server/ai_advisor.go`（新文件，同包 server）

```go
// AIAdvisor 告警建议器：告警上下文 → LLM 建议文本；nil 网关 = 恒空（零回归）。
type AIAdvisor struct {
    gate *llmgate.Client // nil = 未启用（enabled=false 或无 key）
    logger *slog.Logger
    // prompt 构造（纯函数，可测；测试注入固定模板断言追加形态）
}
func NewAIAdvisor(cfg AIAdvisorConfig, logger *slog.Logger) *AIAdvisor // enabled=false 或 api_key_ref 解析为空 → gate=nil
// Advise 异步（不阻塞告警 dispatch）：成功 → 返回建议文本；失败/未启用 → 返回 ""。
func (a *AIAdvisor) Advise(ctx context.Context, alertKey, title, text string) string
```

**接线**：`AlertEngine.dispatch`（alerts.go）内，fire/recover 通知组装后调用 `engine.advisor.Advise(...)`——**同步**（与渠道发送同级，告警低频），失败返回 "" → 追加为空即固定模板原样。`AlertEngine` 增加 `SetAdvisor(*AIAdvisor)` 注入点（nil = 现状，零回归）。

### 2.3 配置（config.go 新增 `notify.ai_advisor` 子段）

```go
type AIAdvisorConfig struct {
    Enabled    bool   `yaml:"enabled" mapstructure:"enabled"`                 // 默认 false 零回归
    Provider   string `yaml:"provider" mapstructure:"provider"`               // openai | anthropic（默认 openai）
    APIKeyRef  string `yaml:"api_key_ref" mapstructure:"api_key_ref"`         // 环境变量名，如 SPROXY_OPENAI_API_KEY
    Model      string `yaml:"model" mapstructure:"model"`                     // 空 = provider 默认
    Timeout    time.Duration `yaml:"timeout" mapstructure:"timeout"`          // 默认 10s
}
// NotifyConfig 新增字段：
AIAdvisor AIAdvisorConfig `yaml:"ai_advisor" mapstructure:"ai_advisor"`
```

- **APIKeyRef 语义**：引用环境变量名（不把密钥写进 yaml——对齐 credential master key 环境变量先例 `SPROXY_CREDENTIAL_MASTER_KEY`）；`Enabled=true` 但 key 解析为空 → 装配层 Warn + `gate=nil`（显式降级可观测，禁静默降级铁律）。
- 装配（`newNotifyCenterFromConfig` 旁）：`NewAIAdvisor` → `alertEngine.SetAdvisor`；`notify.enabled=false` 时 AI 自然不装配（告警无渠道可发）。

## 3. 数据流

```
AlertEngine.fire/recover
  → dispatch(ctx, channels, key, text)          // 现状，同步
  → advisor.Advise(ctx, key, title, text)        // 新增；gate=nil/失败 → ""
      ├─ 构造 prompt：system（运维助手角色+输出约束）+ user（source/object/阈值/正文）
      ├─ llmgate.Client.Advise → POST /v1/chat/completions（带 ctx 超时）
      └─ 成功 → 截断（如 ≤1000 字）→ 返回建议
  → msg.Text = text + "\n\n【AI 建议】\n" + adviseText   // 非空才拼接
  → ch.Send(ctx, msg)                             // 渠道发送不变（去抖/重试/历史不变）
```

- **追加形态**：仅追加不改写原正文；通知标题不变；恢复（recover）通知同样走 AI（可观测恢复建议），成本由去抖约束。
- **不落盘**：建议文本只进通知正文（渠道载荷/邮件/历史），不入审计。

## 4. 错误处理

- **网关失败**（连接/超时/非 2xx/解析失败）：`Advise` 返回 "" + Warn 日志（含 alertKey），**固定模板照发**——fail-closed：AI 是增强层，绝不允许它吞掉告警。
- **超时**：`ctx` 带 cfg.Timeout（默认 10s），告警 dispatch 最多被拖慢一个超时；渠道发送失败仍走 NotifyCenter 既有重试。
- **无 key / 未启用**：`gate=nil`，`Advise` 恒 ""，装配期 Warn 一次（可观测）。
- **prompt 注入**：告警正文可能含外部字符串（如 detail）——system prompt 固定、user 消息仅作数据（API 层自然隔离，不做拼接注入面）。
- **key 泄露**：APIKeyRef 只出现在环境变量，不落配置/日志；HTTP 头 Authorization Bearer 不回显。

## 5. 测试与变异点

1. `llmgate` 单测（httptest mock /v1/chat/completions）：200 正常解析 content / 非 2xx error / 畸形 JSON error / 超大响应截断（LimitReader）/ 超时（慢桩 + 短 Timeout）/ 空 content → 空串。变异：去掉 content 取值 → 红。
2. `TestAIAdvisor_EnabledAppendsAdvice`（server 包）：mock 网关返回固定建议 → dispatch 后通知文本含 "【AI 建议】"+建议（断言 mock 渠道收到的 Text）。变异：不拼接 → 红。
3. `TestAIAdvisor_NoKeyFallsBackTemplate`：enabled=true 但 key 空 → gate=nil → 通知文本 = 固定模板原样（无 AI 段）。变异：把"无 key"当错误吞通知 → 红。
4. `TestAIAdvisor_GatewayFailureStillNotifies`：mock 网关 500 → 通知仍发出（固定模板）+ Warn 日志。变异：失败 return 不发 → 红。
5. `TestAIAdvisor_DisabledZeroRegression`：enabled=false → 文本与未装配 AI 时逐字节一致（快照断言）。
6. `TestAlertEngine_DispatchCallsAdvisor`：注入 advisor 桩计数断言 dispatch 恰好调用一次（fire 一次通知一次）。
7. 配置校验：`api_key_ref` 空 + enabled=true → Validate 允许（运行期降级）+ 装配 Warn（不拒绝——部署可先配好等 key）。

## 6. 片划分

- **片1（S3-⑥-a）**：`pkg/llmgate` 包（Config/Client/Advise/超时/截断）+ 单测 1。
- **片2（S3-⑥-b）**：`pkg/server/ai_advisor.go`（AIAdvisor/prompt 纯函数/Advise）+ AlertEngine.SetAdvisor 接线 + 测试 2–6。
- **片3（S3-⑥-c）**：config 结构（AIAdvisorConfig/NotifyConfig 扩展）+ 装配（newNotifyCenterFromConfig 旁 / cmd/sproxy）+ docs/config.md `notify.ai_advisor` 段 + docs/notifications.md 补充。
- **片4（后续片，不在本期）**：Anthropic 原生 messages API 专路、建议缓存/去抖（同 key 窗口内不重复调用）、`/api/ai/advisor` 手动触发端点。

## 7. 风险与零回归保证

- **零回归**：默认 `enabled=false` → gate=nil → `Advise` 恒 "" → 通知文本与现状逐字节一致；`SetAdvisor` 缺省 nil 分支不加任何判断路径；llmgate 新包不影响既有构建。
- **告警延迟风险**：AI 调用同步阻塞 dispatch 最多 Timeout（10s）——告警低频可接受；如需并发可后续改为 goroutine+去抖（片4 缓存项已列）。
- **成本风险**：每次 fire/recover 一次调用；去抖已把同 key 通知限制为状态变化级，数量有界（与现状通知数一致）。
- **供应商差异风险**：先做 OpenAI 兼容单一实现（Anthropic 兼容端点同形状）；模型名/URL 可配，默认值文档注明。
- **prompt 泄露风险**：通知正文（含文件名/路径）发送到外部 LLM——文档明示 + 配置即同意（enabled 显式开启）；恢复/敏感源告警同样外发，属功能语义。
