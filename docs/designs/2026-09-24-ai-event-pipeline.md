# 事件流 AI 流水线（11.9-③）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：事件流消费端（AI Agent 感知文件变更）+ SSE 端点
> 引用：[2026-09-24-cluster-write-coordination.md](./2026-09-24-cluster-write-coordination.md)（EventBus + FileChangeEvent 已设计，本设计在其上做 AI 消费端）
> 只读源码：pkg/server/handlers_endpoints.go（事件流现状核实）、notify.go（挂点先例）、audit_store.go

## 1. 背景与目标

- **现状**：`handlers_endpoints.go` 经核实只有基础端点与自愈循环（livez/readyz/healthz/version/清理 GC 循环），**无 /api/events 事件流**——事件通道需新建（cluster-write-coordination §2.3 通道 B 已设计 `GET /api/events` SSE + 进程内 EventBus）。
- **目标**：AI Agent（向量索引、摘要、标签等异步流水线）经事件流感知文件变更——`file.change` 事件触发「该文件需重新向量化/重新摘要」的入队信号；SSE 供**外部** AI 编排器订阅；进程内 EventBus 供**内部**流水线订阅。
- **非目标**：不做事件溯源（事件是失效/入队信号，事实源是文件本体+StateStore）；不保证至少一次投递（幂等消费 + 全量重建兜底）；不实现 AI 编排器本身（本期只给事件通道）。

## 2. 组件与接口

### 2.1 复用 cluster-write-coordination 的事件模型

`FileChangeEvent{Op, Owner, Rel, To, Rev, Node, TS}` + `EventBus.Subscribe(prefix, fn)` / `Publish(evt)`（`pkg/server/events.go`，W1 片交付）。本设计在其上新增：

```go
// AIEventConsumer 订阅 file.change 并路由到 AI 流水线任务队列。
// prefix 过滤：只订阅本机 owner 相关事件（单节点 = 全量）。
type AIEventConsumer struct {
    bus    *EventBus           // nil = 未装配（零回归）
    enqueue func(owner, rel string, op string) // 幂等入队回调（向量化/摘要/打标任务）
    logger *slog.Logger
}
func NewAIEventConsumer(bus *EventBus, enqueue func(string, string, string), logger *slog.Logger) *AIEventConsumer
func (c *AIEventConsumer) Start()   // 注册订阅 file.change 前缀
func (c *AIEventConsumer) Stop()    // cancel 订阅
```

- **内部消费路径**：事件 → `enqueue(owner, rel, op)` → AI 任务队列（`pkg/aiindex` 的 workqueue，见 ai-vector-search 设计）。去重：同 (owner, rel) 窗口内只入队一次（map + 时间戳，防事件风暴）。
- **外部消费路径（SSE）**：`GET /api/events?type=file.change`——cluster-write-coordination W2 片交付（认证 + type 过滤 + 断连清理）。**本设计不再重复实现**，只在其上补：AI 编排器可订阅同一流，消费侧要求**幂等**（处理前校验 rev/文件 mtime，处理结果可覆盖）。

### 2.2 流水线消费语义

```
file.change 事件到达
  → AIEventConsumer 去重入队 (owner, rel, op)
  → 向量索引 worker：跳过未变文件（对比 rev 缓存）→ 重embedding → upsert
  → 摘要/标签 worker：摘要缓存失效 → 重新生成（llmgate）
  → 失败事件 → 重试队列（指数退避）→ 终败落 audit（可观测）
```

- **rev 幂等**：消费端缓存 per-(owner,rel) 已处理 rev，`evt.Rev <= cached` 直接跳过（与 cluster-write-coordination 的 rev 幂等同一语义）。
- **删除事件**：`op=delete/rmdir` → 向量/摘要条目同步删除（不重新生成）。
- **rename**：向量/摘要条目 key 迁移（from→to），不重新生成。

### 2.3 配置（config.go 新增 `ai.events` 段）

```go
type AIEventsConfig struct {
    Enabled bool `yaml:"enabled"`   // 默认 false 零回归
    QueueSize int `yaml:"queue_size"` // 默认 256
    DedupWindow time.Duration `yaml:"dedup_window"` // 默认 5m
}
```
装配：`ai.events.enabled=true` 且 EventBus 已装配 → NewAIEventConsumer 接入；单节点未装配 EventBus → 事件源缺位 → Warn + 流水线只跑手动触发/周期扫描（可观测降级）。

## 3. 数据流

```
主节点 POST /upload 成功 → EventBus.Publish(file.change{op:upload, owner, rel, rev})
  ├─ 内部：AIEventConsumer 订阅回调 → 去重入队 → worker 处理（向量/摘要）
  └─ 外部：SSE 广播（events_sse 开启时）→ 外部 AI 编排器订阅 → 拉取文件处理
  → 处理完成 → audit 事件（ai.embed / ai.summarize，见 ai-quota-audit）
```

**不变式**：事件丢失 → 流水线周期全量扫描兜底（如 15m 一次 walk 索引比对 mtime——复用 search_index 全量构建路径）；事件只是「及时触发」，不承担正确性。

## 4. 错误处理

| 场景 | 处理 |
|---|---|
| EventBus 未装配（单节点默认） | AIEventConsumer 不启动；周期扫描兜底（fail-safe） |
| 事件先于文件落盘完成 | 入队延迟重试（如 500ms 后文件仍不可读 → 退避重试 3 次 → 终败 audit） |
| worker 处理失败 | 指数退避重试（3 次）→ 终败 audit 事件 + Warn（可观测，不静默） |
| 去重 map 溢出 | 定期清理过期条目（dedup_window） |
| 消费端 panic | EventBus.Publish 内 recover（cluster 设计已含） |

## 5. 测试 + 变异点

1. `TestAIEventConsumer_SubscribesAndEnqueues`：Publish file.change → enqueue 恰好一次（**变异：不订阅 → 红**）。
2. `TestAIEventConsumer_DedupWindow`：同 (owner,rel) 窗口内两次事件 → enqueue 一次（**变异：去掉重 → 红**）。
3. `TestAIEventConsumer_DeleteEvent_NoEnqueue`（或 enqueue op=delete）：delete 事件路由到删除处理（**变异：delete 当 upload 处理 → 红**）。
4. `TestAIEventConsumer_RevIdempotent`：旧 rev 事件 → 跳过（**变异：去掉 rev 校验 → 红**）。
5. `TestAIEventConsumer_NilBus_Noop`：bus=nil → Start 不 panic、无订阅（零回归断言）。
6. `TestAIEventConsumer_PanicInHandler_Recovered`：enqueue 回调 panic → 不扩散（**变异：去掉 recover → 红**）。
7. 集成：真实 EventBus + 假 enqueue → Publish 后消费链全链路（条件轮询 WaitFor，不 time.Sleep——R14）。
8. SSE 端点测试复用 cluster W2（认证/过滤/断连清理），本设计只补「外部编排器消费幂等」说明不新增测试。

## 6. 片划分

| 片 | 内容 | 验收 | 依赖 |
|---|---|---|---|
| **E1** | AIEventConsumer（订阅/去重/rev 幂等/路由）+ 单测 1–6 | 单测绿 + 变异命中 | cluster W1（EventBus） |
| **E2** | 周期扫描兜底（mtime 比对 + 入队）+ 集成测试 7 | 集成绿 | E1、search_index |
| **E3** | 配置段 `ai.events` + 装配 + docs/config.md（R15） | 文档门禁绿 | E1 |

E1 与 cluster W1 可并行（接口先约定）；E2 依赖 E1；E3 ∥ E2。

## 7. 风险与零回归保证

- **零回归**：`ai.events.enabled` 默认 false → consumer 不装配 → 写路径无额外调用；SSE 端点归属 cluster W2（默认关）。
- **事件风暴**：去重窗口 + 有界队列（queue_size，满则丢弃 + Warn——消费兜底靠周期扫描）。
- **消费滞后**：队列 backlog → 周期扫描兜底收敛；监控：queue 深度 metrics（/metrics 扩展）。
- **与 cluster 事件共用通道**：file.change 语义对两者一致（cluster 用来失效索引、AI 用来失效向量/摘要），前缀订阅互不干扰。
- **外部编排器可靠性**：SSE 断连重连 + 消费幂等（文档明示：处理后覆盖式写入，重复消费无害）。
