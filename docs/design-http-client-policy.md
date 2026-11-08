# 设计：HTTP 客户端的策略化与收敛管理

> 状态：**实施中**（2026-11-08）。上游：`docs/design-http-orchestration.md`（pkg/pipe + pkg/httpc）。
> 本文回答四个问题：① 现有客户端使用场景；② 可替换的退避策略；③ 客户端行为收敛管理；
> ④ **生产是否默认共享客户端、测试才用 IsolatedTransport**。

## 1. 现有使用场景（实测盘点，2026-11-08）

对全部 46 个迁移点（PR #1058）按**调用形态**归类：

| 场景类 | 典型模块 | 特征 | 需要的策略 |
|---|---|---|---|
| **S1 外部 API 单发调用** | `llmgate`、`aiembed`、`dnspod`、`accesskey(Vault)`、`selfupdate` | 短请求、幂等、同 host 反复调用、对 429/5xx 敏感 | 重试（快退避）+ 短单次超时 |
| **S2 服务内部/控制面** | `notify`、`sync/httptransport`、`tunnel/hub/*`、`mesh/*`、`relay` | 同进程内网、低延迟、失败即降级、可 long-poll | 少量重试 / 不重试（由上层状态机处理） |
| **S3 长传输（下载/流式）** | `downloader`、`selfupdate`（大文件）、`webdav`、`pikpak`（直链） | 大 body、按 Range 续传、**不能有整体超时** | 不重试（自带续传）/ Transport 层细粒度超时 |
| **S4 库/第三方持有的 client** | `baidupcs`（库 API）、`pikpak`（hybrid/resolver） | client 要交给第三方或库 | 只需"规范构造"，不需要 httpc 的 Do 编排 |
| **S5 测试替身** | `testutil.IsolatedClient`、各 `_test.go` | httptest、需与其它用例隔离 | **必须独立连接池** |

**结论**：策略需求收敛为 **4 种**（none / fast / standard / slow），不需要每个调用点自定义字面量。

## 2. 可替换的退避策略（`httpc.PolicyFor` / `RegisterPolicy`）

现状：调用点内联 `pipe.Policy{...}` 字面量 ⇒ 同类调用的退避不一致，且改动要逐点改。

设计：**命名策略注册表**（策略字符串可配置/可替换，注册表进程级）：

```go
type PolicyName string
const (
    PolicyNone     PolicyName = "none"      // 不重试（长传输/非幂等）
    PolicyFast     PolicyName = "fast"      // 200ms×2ⁿ 封顶 2s，3 次（外部 API/LLM）
    PolicyStandard PolicyName = "standard"  // 1s×2ⁿ 封顶 8s，4 次（默认）
    PolicySlow     PolicyName = "slow"      // 5s×2ⁿ 封顶 60s，5 次（云盘/跨网长任务）
)
func PolicyFor(name PolicyName) pipe.Policy      // 未注册 ⇒ PolicyStandard
func RegisterPolicy(name PolicyName, p pipe.Policy) // 可覆盖/新增（进程级，供装配层定制）
```

调用点写法：`Retry: httpc.PolicyFor(httpc.PolicyFast)`（集中、可替换、可审计）。
**默认（零值）= PolicyStandard**：让"忘了声明"也有安全行为。

## 3. 客户端行为收敛管理（`httpc.Shared`）

设计：**按用途（Purpose）收敛**——同一用途全进程复用同一 `*Client`（含策略 + 连接池 + 指标）：

```go
type Purpose string
const (
    PurposeLLM        Purpose = "llm"        // llmgate
    PurposeEmbed      Purpose = "embed"      // aiembed
    PurposeSelfUpdate Purpose = "selfupdate"
    PurposeNotify     Purpose = "notify"
    PurposeCloudAPI   Purpose = "cloud-api"  // pikpak/baidupcs 控制面
)
func Shared(p Purpose, opt Options) *Client // 进程级幂等：同 purpose 首次调用生效
```

收益：① 同用途连接池复用（TLS/连接数）；② 策略/超时/指标**一处配置**；③ 可在装配层
一次性替换（例如把所有外部 API 的重试策略从 standard 换成 fast）。

## 4. 【关键决策】生产默认共享 vs 测试隔离

### 4.1 事实
- `netutil` 已有两档：`DefaultTransport()`（**进程级共享**，`sync.Once`）与 `IsolatedTransport()`
  （`DefaultTransport.Clone()`，独立池）。其注释已写明："**生产**长期运行进程……连接池复用是性能
  正确选择；**需要隔离**（测试、不可信端点、防跨实例串连）用 IsolatedTransport"。
- **但**生产代码里有 **5 处 `CloseIdleConnections()`**（`oidcldap`、`turnrest`、`hub/federation`、
  `sync/httptransport.Close`、`testutil`）；若它们共享同一池，就会**打断全局在途请求**
  （"transport connection broken: http: CloseIdleConnections called"，硬规则 17 的原始事故）。
- PR #1058 迁移时 `httpc.NewHTTPClient` 默认取 `IsolatedTransport()` ⇒ 与 `netutil` 的
  "生产共享"意图**相反**，并且会出现"每个调用点一个连接池"的资源浪费。

### 4.2 决策：**分层，不用"生产共享 / 测试隔离"这条粗规则**

| 维度 | 决策 | 理由 |
|---|---|---|
| **httpc 默认 Transport** | **共享池** `netutil.DefaultTransport()` | 对齐 netutil 既定设计 + Go 官方语义；避免"每调用点一池" |
| **是否允许对共享池 `CloseIdleConnections`** | **禁止**（门禁 + 文档） | 共享池的生命周期属于**进程**；任一处关闭都会伤及全局（历史事故） |
| **需要"用完即关"的场景**（短命进程/一次性 client/库持有） | 显式 `Options{Isolated: true}` 或 `Transport: netutil.IsolatedTransport()` | 让隔离成为**显式选择**，而不是默认（默认隔离的代价是资源浪费） |
| **测试** | **必须自建 client**（`testutil.IsolatedClient(t)`，硬规则 17） | 测试独立性的**正确保证**是"测试不碰生产池"，而不是"生产默认隔离" |

**为什么不是"生产默认共享、测试用 Isolated"**：这句话把两件事混在一起 ——
- "测试用 Isolated" 若指"测试去调用 `IsolatedTransport()`"，那**仍然共享**（同一函数返回新池 ✓ 其实是独立的）；
- 正确表述是：**测试必须自建 client 且不与任何生产/其它用例共享池**（现有 `testutil.IsolatedClient`
  已实现：每用例新池 + `t.Cleanup(CloseIdleConnections)`）；
- 生产是否共享与测试独立性**无关**——只要测试不引用生产池。

**收益/风险评估**：
- 共享池收益：FD 数（每池默认 MaxIdleConns=100）、TLS 握手复用、DNS 缓存命中 —— 对本仓
  "同 host 高频"的 S1/S2 场景明显（如 notify 告警风暴、mesh 心跳）。
- 共享池风险：① 一处 Close 伤全局（**用门禁禁止**）；② 恶意/慢 host 占用池（可用
  `MaxIdleConnsPerHost`/分层 Purpose 缓解）；③ 故障隔离弱化（不同 purpose 各有池 ⇒ 部分隔离）。
- ⇒ 以 **Purpose 分层**在两者间取平衡：**同 purpose 共享、跨 purpose 隔离**。

### 4.3 落地顺序（避免大爆炸）
1. `httpc` 增加 `PolicyFor/RegisterPolicy` + `Shared(purpose)` + `Options.Isolated`（默认共享）；
2. **默认值变更风险控制**：`NewHTTPClient` 默认由 `IsolatedTransport()` 改为 `DefaultTransport()`，
   但**要求调用点不得对共享池调 `CloseIdleConnections`** ⇒ 逐个审查 5 处 Close 调用点：
   - `oidcldap`（每次 discovery 新建 client + defer Close）⇒ **改用 `Isolated: true`**（短命 client 语义）；
   - `turnrest`（同上）⇒ `Isolated: true`；
   - `hub/federation`（`federationPeerClient` 返回的 client 有 CA/TLS 定制）⇒ 已是独立 Transport ✓ 不受影响；
   - `sync/httptransport.Close`（自有 Transport）⇒ 不受影响；
   - `testutil`（测试）⇒ 本就是隔离 ✓。
3. 门禁：新增"**禁止对共享池（DefaultTransport）调 CloseIdleConnections**"的棘轮/白名单检查
   （比文本匹配更稳的做法：检查 `CloseIdleConnections` 调用点是否都持有 `Isolated: true` 或自有 Transport）。
4. Purpose 适配：`llmgate`/`aiembed`/`selfupdate`/`notify` 改 `Shared(...)`；S3/S4 保持既有形态。
