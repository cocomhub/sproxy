# 限流维度扩展（11.10-⑪）

## 背景/目标
- 现状：`pkg/server/ratelimit.go` 的 `RateLimiter` 只有**每 IP 令牌桶（limit/10）+ 全局滑动窗口**两个维度（`allowIPLocked` → `allowGlobalLocked` 回退链），可选 coordinator 多实例共享配额（key=归一化 IP）。无 per-endpoint、无全局并发上限。
- 目标：新增两个维度：① **per-endpoint 限流**（如 `/download`、`/upload` 独立配额，防单端点打爆全局）；② **全局并发上限**（同时处理中请求数上限，防连接洪泛拖垮后端）；现有 per-IP/全局窗口语义零变化（默认新维度关闭）。

## 组件与接口
- `pkg/server/ratelimit.go`：
  - 配置扩展（`ratelimit` 段，config.go 接线）：
    - `endpoints: map[string]EndpointLimit`（path → `{limit, window}`；空 = 不启用）；`endpoint_default: {limit, window}` 可选兜底。
    - `max_concurrent: int`（0 = 不启用）；`concurrent_semaphore: chan struct{}` 容量 = max_concurrent。
  - `RateLimiter` 新增字段：`endpointLimits map[string]endpointRule`、`endpointDefaults`、`sem chan struct{}`；`UpdateConfig` 同步热更新（PUT /api/config 路径，沿用现有 mu 语义）。
  - 新方法：`AllowEndpoint(path string) bool`（无匹配规则 → true 透传）；`AcquireConcurrent() bool`（`select { case sem <- struct{}{}: return true; default: return false }` 非阻塞；成功者必须配对 `ReleaseConcurrent()`——**保证 `defer` 配对**）；`Middleware` 内顺序：并发闸 → per-IP 桶 → per-endpoint 桶 → 全局窗口 → coordinator（放行链，全过才放行；任一拒绝 → 429 JSON，与现状一致）。
- path 归一化：`r.URL.Path` 原样作键（精确匹配 + `/` 前缀最长匹配；静态文件/隧道内部路由排除——**只限显式配置的端点**，不误伤 hub/mux 长连）。

## 数据流
请求 → `Middleware`：`AcquireConcurrent`（失败 429 + 计数）→ `allowIPLocked` → `AllowEndpoint(path)`（无规则透传）→ `allowGlobalLocked` → coordinator（可选）→ 放行；`defer ReleaseConcurrent` 在 handler 完成后归还。指标：`Metrics` 加 `RateLimitedEndpoint`/`RateLimitedConcurrent` 计数（`labeledCounters[endpointKey]` 复用，/metrics 输出 `sproxy_rate_limit_rejected_total{scope="ip|endpoint|global|concurrent",endpoint=...}`）。

## 错误处理
- 非法配置（limit<=0 / window<=0）→ 沿用现有默认化（归 5/1s）或显式 400——设计选**沿用默认化**（与现状 UpdateConfig 一致，文档标注）；path 规则冲突（精确 vs 前缀）→ 精确优先（文档标注）；sem 容量变化热更新 → 重建 channel 需在 mu 下且**在途持有者归还旧 sem**（设计选「只增不减 + 旧 sem 排空后切换」复杂化 → **简化为热更新仅生效于新请求，旧请求继续归还旧 sem**，靠 `defer` 保证不泄漏）。

## 测试+变异点
- table-driven：每维独立（per-endpoint 配额、并发上限 N 时第 N+1 个 429、无规则透传）；组合链（并发满 + IP 桶满 + endpoint 满 + 全局满各维度独立拒绝）；`-race` 并发放行/归还；热更新后新规则生效。
- 变异验证：① 并发闸不放行计数（Acquire 不占用）→ 超限用例红；② per-endpoint 规则匹配写反 → 该端点不限制用例红；③ `ReleaseConcurrent` 缺失（defer 删掉）→ 并发泄逐渐拒绝所有请求用例红（泄漏可观测）。

## 片划分
- 片1：ratelimit 配置扩展 + `AllowEndpoint`/`AcquireConcurrent`/`ReleaseConcurrent` + Middleware 放行链 + 单测（纯 pkg/server）。
- 片2：config.go 接线 + PUT /api/config 热更新 + Metrics 拒绝计数 + docs/config.md。
- 片3（可选）：coordinator 扩展 per-endpoint key（多实例共享 endpoint 配额）——先不做，标注后续。

## 风险与零回归
- 新维度默认关闭（endpoints 空 + max_concurrent=0）→ 放行链与现状逐字一致（golden 测试：无配置时 Middleware 行为不变）；429 响应体/状态码沿用现状（`{"error":"rate limit exceeded"}`）；sem 归还在 `defer` 中保证不泄漏（并发测试断言泄漏为 0）；不动 coordinator 既有单实例语义。
