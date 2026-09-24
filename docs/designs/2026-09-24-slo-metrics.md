# SLO 指标设计：p99 延迟直方图 + Apdex

## 1. 背景与目标
- 现状：Metrics 已有 40+ 指标（requests_total/2xx/4xx/5xx、volume_io 延迟累计、mux/cloud/hub 族），
  但**无延迟分位数**——无法回答「p99 延迟多少」「SLO 是否达标」。
- 目标：
  1. 请求处理时长直方图（Prometheus histogram），支撑 PromQL `histogram_quantile(0.99, ...)` 求 p99；
  2. Apdex 评分（阈值可配，默认 500ms），输出 0-1 gauge；
  3. 全部标准库实现（无 prometheus client 依赖），延续 /metrics 手写文本格式。

## 2. 现状（源码事实，pkg/server/metrics.go）
- 请求记录点实为 **`metricsMiddleware`**：`ActiveConnections` 增减 + `metricsResponseWriter` 捕获
  状态码 → `RecordRequest(statusCode)`。**当前不测时长**。
- **简报所称 `requestLogMiddleware` 未出现在 metrics.go**（grep 无此函数）；`metricsResponseWriter.reqActor`
  由 authMiddleware 写入「供请求日志」——日志/时长记录点在本文件之外（本任务读取范围外，不猜文件名）。
- 渲染基建：`writeMetric`（单值）、`writeLabeledCounter`/`writeGaugeSamples`（带标签族）、`escapeLabel`；
  counter 族约定「无样本也输出 HELP/TYPE」（可发现性）。
- 计数基建：高频用 atomic.Int64；低频带标签用 `labeledCounters`（互斥+map）。
  请求延迟是**每请求高频**事件 → 直方图必须无锁（bucket 数组 + atomic，不用互斥）。

## 3. 组件与接口（全部落在 pkg/server/metrics.go；metrics_slo.go 不存在、不新建）
```go
// durationHistogram：无锁桶直方图（阈值升序，末桶恒为 +Inf）
type durationHistogram struct {
    thresholds []time.Duration // 升序；不含 +Inf
    buckets    []atomic.Int64  // len = len(thresholds)+1，末位 = +Inf
    sum        atomic.Int64    // 累计纳秒
    count      atomic.Int64
}
func newDurationHistogram(thresholds []time.Duration) *durationHistogram
func (h *durationHistogram) observe(d time.Duration) // nil 安全；sort.Search 定位桶，d<=0 落首桶
func (h *durationHistogram) snapshot() (buckets []int64, sumNanos, count int64) // 渲染读

// Metrics 新增字段（NewMetrics 初始化）
requestDuration *durationHistogram // 默认桶：5/10/25/50/100/250/500ms/1/2.5/5/10s
apdexSatisfied, apdexTolerated, apdexFrustrated atomic.Int64
apdexT atomic.Int64 // 纳秒；0 → 默认 500ms

// 新方法（RecordRequest 签名不变 → 零回归）
func (m *Metrics) RecordRequestLatency(d time.Duration) // 直方图 observe + apdex 分档
func (m *Metrics) SetApdexThreshold(d time.Duration)    // 装配层注入；<=0 忽略
func (m *Metrics) apdexScore() float64                  // (sat + tol/2) / total；total=0 → 0

// 渲染助手
func writeHistogram(b *strings.Builder, name, help string,
    th []time.Duration, buckets []int64, sumNanos, count int64)
```
- Apdex 分档（T = apdexT，默认 500ms）：`d <= T` → satisfied；`T < d <= 4T` → tolerated；`d > 4T` → frustrated。
- 配置：`apdex_threshold`（新配置字段，默认 500ms）在装配层经 `SetApdexThreshold` 注入。

## 4. 数据流
请求 → `metricsMiddleware` 包 `start := time.Now()` → `next.ServeHTTP(mw, r)` →
`defer m.RecordRequestLatency(time.Since(start))` → 直方图 observe（无锁累加 3 处 atomic）
+ apdex 三档计数（apdexT 走 atomic.Load，无锁）。
抓取 /metrics → `MetricsHandler` 尾部追加：
- `writeHistogram("sproxy_request_duration_seconds", ...)`（桶 + `_sum` + `_count`）；
- `sproxy_apdex` gauge（浮点，保留 3 位小数）+ 三档 counter（satisfied/tolerated/frustrated）。
PromQL：`histogram_quantile(0.99, sproxy_request_duration_seconds_bucket)` → p99。

## 5. 输出格式（手写 Prometheus 文本，单位秒）
```
# HELP sproxy_request_duration_seconds HTTP request duration histogram
# TYPE sproxy_request_duration_seconds histogram
sproxy_request_duration_seconds_bucket{le="0.005"} 1
sproxy_request_duration_seconds_bucket{le="0.01"} 1
...
sproxy_request_duration_seconds_bucket{le="+Inf"} 10
sproxy_request_duration_seconds_sum 0.042
sproxy_request_duration_seconds_count 10
```
- bucket 为**累计**计数（非增量），le 升序，+Inf 恒在末位；命名 `*_duration_seconds` 遵循约定。
- 无样本也输出 HELP/TYPE（与 writeLabeledCounter 同约定）。
- apdex：`# TYPE sproxy_apdex gauge` + `sproxy_apdex 0.95`；三档 counter 走 writeMetric。

## 6. 错误处理
- 负/零时长：防御性落首桶（time.Since 实际不可能为负）。
- 超长时长：落 +Inf 桶，不丢计数；sum/count 恒准确。
- apdexT <= 0 / 未配置：NewMetrics 置默认 500ms；SetApdexThreshold 非法值忽略并 slog.Warn。
- 渲染并发漂移：多次 atomic.Load 间计数可轻微不一致（Prometheus client 同语义），
  且 bucket 单调不减性质不受影响，可接受。
- /metrics 自身请求也计入（简单一致）；如需排除留后续片（r.URL.Path 过滤，默认不排除）。

## 7. 测试与变异点（TDD、纯标准库、仅 127.0.0.1）
1. **TestDurationHistogram_ObserveBucketBoundary**：时长恰在桶边界归入正确桶（升序判定 <=）；
   变异：sort.Search 比较方向 < vs <= 反 → 红。
2. **TestDurationHistogram_CumulativeInvariant**：随机序列后每桶计数 ≥ 前一桶（累计不变量）、
   sum == Σd、count == N；变异：漏加 sum/count、越界索引 → 红。
3. **TestDurationHistogram_PlusInfCatchesAll**：超最大桶时长 → +Inf +1 且 count == N；
   变异：桶数组长度不 +1（无 +Inf 桶）→ 红。
4. **TestApdex_Boundary**：d==T → satisfied；d==4T → tolerated；d==4T+1ns → frustrated；
   变异：<= 改 <、4T 常量错 → 红。
5. **TestApdex_ScoreAndZeroTotal**：score=(sat+tol/2)/total 数值断言；total=0 → 0（不 NaN）；
   变异：漏 tol/2、除零 → 红。
6. **TestWriteHistogram_TextFormat**：golden 字符串比对（le 升序、+Inf 行、_sum/_count、HELP/TYPE）；
   变异：桶乱序、sum 误用纳秒单位 → 红。
7. **TestMetricsMiddleware_RecordsLatency**：httptest 打一次请求 → count==1、apdex 恰一档 +1；
   变异：middleware 不测时长（还原旧实现）→ 红。
8. **零回归**：既有指标断言文本不变（新增仅追加尾部）；现有 metrics 测试全绿。

## 8. 片划分
- S2c-1：durationHistogram + writeHistogram + 测试 1/2/3/6（纯数据结构，无装配依赖）。
- S2c-2：apdex 三档 + apdexScore + SetApdexThreshold + 测试 4/5 + /metrics 渲染接线。
- S2c-3：metricsMiddleware 时长捕获 + RecordRequestLatency + 测试 7/8 + docs/metrics.md 文档。

## 9. 风险与零回归保证
- 风险①：直方图每请求 3 处 atomic 写——与 RequestsTotal 同级开销，可忽略。
- 风险②：apdexT 生效时机——SIGHUP 只重载软配置，apdexT 走启动装配（重启生效），文档注明。
- 风险③：新指标命名冲突——`sproxy_request_duration_seconds_*` / `sproxy_apdex*` 全仓唯一（可 grep 验证）。
- 零回归：只增字段与方法；`RecordRequest` 签名不变；middleware 仅加 time.Now()/defer；
  无新依赖（sort/atomic/time/strings/fmt 均已在用）；/metrics 输出追加在尾部，既有行不变。
- 验证：`go test ./pkg/server/...` + `go test ./internal/archcheck/...`；无新门禁需求（R10 不涉前端）。

## 10. 残余与后续
- requestLogMiddleware（含 actor/时长，位于 metrics.go 之外、本任务读取范围外）：如需统一时长口径，
  可复用 `RecordRequestLatency`（接口不变）——本设计不依赖它，时长以 metricsMiddleware 为准。
- 可选后续：排除 /metrics 自身、按 status 族拆分直方图（2xx/4xx/5xx 标签）。
