# 设计：通用 HTTP 编排基座（`pkg/pipe` + `pkg/httpc`）

> 状态：**待评审**（2026-11-08）。动机来自 `docs/archive/baidupcs-upload-core.md`：
> 百度上传「不可取消 + ~90s」的根因是**把超时/重试策略藏在第三方库里**；同样的病在
> 本仓库**自己**的 89 处 HTTP 调用点上以「各写一遍」的形式存在。

## 1. 问题（量化，2026-11-08 实测本仓库）

| 现象 | 计数 | 命令 |
|---|---|---|
| 自建 HTTP 客户端构造点（裸 `&http.Client{}` / `http.Transport{}` / `DefaultTransport`） | **89** | `grep -rn "&http.Client{\|http.Transport{\|DefaultTransport" --include=*.go pkg cmd` |
| 各写一遍的超时字段（`ResponseHeaderTimeout`/`TLSHandshakeTimeout`/`SetTimeout(`/`Timeout:`） | **90** | 同上类推 |
| 各写一遍的指数退避 | **≥5**（`pkg/backup` 100ms×2ⁿ｜`pkg/leader/local.go` 1s×2 封顶 10s｜`pkg/tunnel/mesh/node.go` 1s→30s｜`pkg/syncmgr/manager.go` 10s×2｜`pkg/server/remotefs/accept_retry.go` 5ms→1s） | `grep -rn "backoff\|Backoff\|MaxRetr"` |
| 裸受限并发 worker 池（`sem <- struct{}{}`） | **10 文件**（backup / client / sync / tunnel.mesh×2 / relay / baidupcs×2 / pikpak×2） | `grep -rln "sem <- struct{}{}"` |

**结论**：本仓库违反自身收敛原则第 4 条（「同一语义只留一份实现」）——**超时、重试退避、受限并发、
取消传播**这四件事被重复实现了数十次，且**没有一个地方能统一回答"这个请求会不会卡、能不能取消"**。
这正是 baidupcs 90s 事故的同构病根（只是那次病在第三方库，这次病在我们自己）。

## 2. 目标 / 非目标

### 目标
1. **机制统一，策略显式**：超时、重试、退避、并发、取消**只有一份实现**；每个调用点用**声明的策略**
   表达自己的语义（而不是散落的字面量）。
2. **取消优先**：任何等待（重试退避、限流、连接）都必须可被 `ctx` 打断——**禁止不可中断的 `time.Sleep`**。
3. **可观测**：每次调用的阶段/重试/耗时/错误分类可打点，可接 `pkg/audit`。
4. **可测试**：`RoundTripper`、时钟、睡眠全部可注入 ⇒ `httptest` 能在 CI 锁定行为。
5. **渐进**：新代码直接使用；旧代码**按子系统分批迁移**，每批行为等价 + 独立验证。

### 非目标（**防过度抽象**，边界必须写死）
- ❌ 不做「通用云盘客户端」：各后端协议/签名/会话差异大，只统一 **HTTP 交互 + 编排**；
- ❌ 不做「业务重试语义」统一：策略由调用点声明，基座只提供机制；
- ❌ **不自动重试非幂等请求**（防重复上传/扣费）——默认仅对幂等方法生效；
- ❌ 不接管认证：刷新钩子由调用方提供（基座最多调用一次）；
- ❌ 不一次性替换 89 处（大爆炸重构风险不可控）。

## 3. 分层架构

```
L2 领域适配（消费方，逐步迁移）
   uploadcore / baidupcs / pikpak / downloader / syncmgr / backup / selfupdate / llmgate / ...
        │  只声明策略：Timeout / RetryPolicy / Concurrency / RetryOn
        ▼
L1 pkg/httpc —— HTTP 交互基座（★ 本设计的核心交付）
   Client.Do(ctx, req) (*http.Response, error)
   Client.DoJSON / DoStream / DoUpload        ← 流式上传/下载/SSE
   Options{Transport, Timeout, ResponseHeaderTimeout, TLSHandshakeTimeout,
           IdleConnTimeout, Retry, Logger, Metrics, Clock, Sleeper, Recorder}
   · 默认 Transport = netutil.IsolatedTransport()（**唯一构造点**，满足 R19）
   · 重试：L0 驱动；分类（幂等+5xx/429/网络 ⇒ 可重试；4xx ⇒ 致命）
   · 打点：每次尝试 stage/attempt/elapsed_ms/err_class
        │
        ▼
L0 pkg/pipe —— 纯编排原语（零 HTTP 知识）
   pipe.Retry(ctx, Policy, op)        Policy{Attempts, Base, Cap, Jitter, Retryable}
   pipe.RunParts(ctx, items, conc, fn) 受限并发 + 首错取消 + 顺序无关结果
   pipe.Clock / pipe.Sleeper           可注入（测试零等待）
   pipe.Recorder                       阶段/尝试事件（可接 audit.Scope）
```

**为什么分两层**：L0 可以被**非 HTTP** 场景复用（本地 FS 重试、DB 写重试、迁移逐项处理）；
L1 承担 HTTP 特有语义（幂等判定、Connection 复用、body 流式）。这样避免"为了上传下载而造一个
带 HTTP 知识的万能包"。

## 4. 关键设计决策

### 4.1 幂等与重试（安全第一）
- 默认 `RetryPolicy.Retryable`：`GET/HEAD/OPTIONS` 恒可重试；`PUT/DELETE` 仅当调用方声明
  `Idempotent: true`；`POST/PATCH` **默认不重试**（除非显式声明）。
- 状态码分类：`429/5xx/网络错误` ⇒ 可重试（并受 `Retry-After` 影响）；`4xx(非429)` ⇒ 致命，
  **不再重试**（与 baidupcs 实测的 400/401/403/413 立即终止同构）。
- **绝不重放已消费的 body**：需要重试的请求体必须实现 `io.ReadSeeker` 或提供 `BodyFactory`，
  否则基座拒绝重试（fail-fast，而不是静默发送空 body）。

### 4.2 超时（三层，全部显式）
| 层 | 默认 | 说明 |
|---|---|---|
| 连接/握手 | 10s | `TLSHandshakeTimeout` / `DialContext` timeout |
| 响应头 | 30s | `ResponseHeaderTimeout`（防"连接成功但服务端不响应"） |
| 单次尝试 | 60s | 由 `ctx` per-attempt 施加（**可取消**） |
| 整体预算 | 调用方声明 | `ctx` 上层 deadline；基座不叠加"不可见的总超时" |

> 这正是 baidupcs 的反面：那里是**库内 200s 不可注入**，这里是**每个调用点显式可见**。

### 4.3 取消传播
- 退避用 `pipe.Sleeper`（默认 `select { <-ctx.Done(); <-timer.C }`）——**不用 `time.Sleep`**；
- 流式 body 读取一律 `req = req.WithContext(ctx)`；
- `RunParts` 在首个致命错误时取消兄弟任务（快速失败），但**已成功的结果保留**（幂等重入时可跳过）。

### 4.4 可观测性
- 结构化日志：`http_method/host/path/attempt/status/elapsed_ms/err_class`（默认 `Debug`，
  失败升级 `Warn`），脱敏（不记 query 里的 token；只记 path 模板）；
- 指标（`pkg/httpc` 侧，可选注入）：
  `httpc_requests_total{method,status_class}`、`httpc_retries_total{reason}`、
  `httpc_attempt_seconds{method}`；
- 可选 `pipe.Recorder`：把"尝试/重试/阶段"写入 `pkg/audit` 作用域（长任务可恢复场景）。

### 4.5 与既有能力的复用关系（**不新造**）
| 既有能力 | 关系 |
|---|---|
| `pkg/netutil.IsolatedTransport()` | **唯一 Transport 构造点**（L1 默认值）；R19 门禁白名单收敛到 `pkg/httpc` + `pkg/netutil` |
| `pkg/waitq`（排队/租约/配额） | **互补**：`RunParts` 控并发数，`waitq` 控资源配额；`RunParts` 可注入 `waitq.Queue` 做准入 |
| `pkg/audit`（WAL） | L0 `Recorder` 的适配目标（长任务恢复） |
| `pkg/recovery` | 消费 L0 `Recorder` 的作用域做启动恢复（与 uploadcore 同路径） |
| `pkg/httpproxy` | 代理协议实现（客户端侧），与 L1 正交；L1 只"使用"代理配置 |
| `pkg/units/sizex` | 字节阈值类配置（硬规则 19） |

## 5. 迁移策略（分批、可回退、每批独立验证）

| 阶段 | 内容 | 验收 |
|---|---|---|
| **H1** | `pkg/pipe`：Retry/RunParts/Clock/Sleeper/Recorder + 表驱动单测 + 变异 | 单测全绿；变异命中（退避序列/取消/首错取消） |
| **H2** | `pkg/httpc`：Client/Options/Do/DoJSON/DoStream/DoUpload + `httptest` 契约 | 重试次数、退避序列、取消时延、幂等拒绝、body 不可重放拒绝 均被锁定 |
| **H3** | **首个消费方 = `uploadcore`**（替换其内部 `sleepCtx/backoff/runParts`） | uploadcore 既有 8 用例保持全绿（行为等价） |
| **H4** | 按子系统迁移：`pkg/downloader` → `pkg/selfupdate` → `pkg/llmgate` → `pkg/syncmgr` → `pkg/backup` → `pkg/client` … | 每项独立 PR：行为等价 + 该子系统测试全绿 + 真实链路 e2e |
| **H5** | 门禁与文档：archcheck 棘轮（`pkg/**` 新增裸 `&http.Client{}` 只减不增）+ `docs/architecture.md` 收敛 | 棘轮基线登记；文档写明"新代码必须用 httpc" |

**回退**：H1/H2 是纯新增包（零风险）；H3/H4 每批可单独 revert。

## 6. 风险与对策

| 风险 | 对策 |
|---|---|
| 抽象过度，调用点被迫走弯路 | 机制/策略分离；`Retry: nil` / `Transport: 自定义` 可逃逸；**不强制**迁移 |
| 重试语义误判导致重复副作用 | 默认只重试幂等；非幂等需显式声明；body 不可重放即拒绝重试 |
| 迁移面大（89 处） | 分批 + 棘轮只减不增 + 每批独立 PR |
| 与 R19 门禁冲突 | 门禁白名单收敛到 `pkg/httpc`/`pkg/netutil`，并在门禁文件写明理由（例外必须写明，不能只加白名单） |
| 新增包与 `pkg/client` 职责重叠 | `pkg/client` 是 **sproxy 协议 SDK**（业务），`pkg/httpc` 是**传输基座**（机制）；`pkg/client` 未来可内部改用 httpc |
| 指标/打点开销 | 默认 `Debug` + `NullMetrics`；仅显式开启时记录 |

## 7. 待评审决策

1. **范围**：只做 L0（`pkg/pipe`）+ L1（`pkg/httpc`）基座（推荐）
   vs 连 L2 一起做（为每个后端提供薄封装，工作量大且易过度抽象）。
2. **迁移节奏**：H1–H3（基座 + uploadcore 首个消费方）先行，其余按子系统分批（推荐）
   vs 同步迁移候选清单（风险高）。
3. **默认重试语义**：仅幂等方法自动重试，非幂等需显式声明（推荐，安全）
   vs 默认全部可重试（不推荐：有重复副作用风险）。
4. **门禁**：H5 才加 archcheck 棘轮（先统计不阻塞）（推荐）
   vs 立即加硬门禁（可能因历史存量误报阻塞其它 PR）。
