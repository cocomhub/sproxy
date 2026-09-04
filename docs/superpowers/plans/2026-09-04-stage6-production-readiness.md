# 阶段6 生产可用性：AK 轮换 + rateLimiter 热更新 + OTel 装配 + 审计 UI + 单位工具包

## Context

sproxy 完全组网路线图（阶段 1-5）已全部完成并合入 master。后续"阶段 6 生产可用性"各项中，
同步重试（#144）、操作审计（#145）、多租户存储布局（#146）、配额磁盘封顶（#154）、TURN REST（#141）、
联邦/kad 持久化（#142）已合入。**剩余 5 项生产可用性能力未实现/待增强**（用户 2026-09-04 `/plan` + 追加确认）：

1. **AK 轮换**：sclient 仅有 `access-key create`（生成一对 AK/SK 打印），无 rotate/expire/delete；
   服务端 `access_keys` 为静态配置（SIGHUP 不重载 access_keys），无法平滑轮换凭据。
2. **rateLimiter.UpdateConfig 热更新**：`pkg/server/config_api.go:217` 明确 TODO，
   `PUT /api/config` 改 rate_limit 只改 cfgPtr，已启动的限流实例不生效。
3. **OTel 装配（含 exporter）**：`pkg/telemetry/ext/otel` 适配器存在且独立 module（已入 go.work），
   但 main（sproxy/sclient）未装配 OTel tracer provider，无配置开关。**用户要求支持 OTLP exporter**
   （配置 `otlp_endpoint` 时实际上报），不限于进程内 SDK。
4. **Web UI 审计日志查看**：审计只写 stdout JSON 行（`pkg/server/audit.go` RecordAudit），
   无查询 API、无内存缓冲、无 UI 面板。
5. **字节单位工具包（新增）**：类型解析/配置工具，避免直接配字节，改用 MB/MiB 等人类单位
   配置，行为类似 `time.Duration`——提供 `Parse` 函数与 `String` 等方法，便于 flag 命令行解析、
   配置文件解析、日志以人类可视化单位输出；工具包合理命名便于后续扩展其他辅助类型。

**目标**：全部 TDD 实现 + 测试覆盖 + lint 0 + 独立对抗式审查，分别开独立 feature 分支 + PR
（遵循项目惯例：每功能一 PR，CI 通过后人工 squash 合并）。

## 用户决策（AskUserQuestion 已确认 + 追加）

- **AK 轮换形态 = SIGHUP 重载 + API**：服务端新增凭据管理端点（受 SproxySig 保护），
  sclient 加 rotate/expire/delete 子命令；同时让 SIGHUP 重载 access_keys（当前 CLAUDE.md 明确不重载，
  需更新文档与校验逻辑）。
- **OTel 范围 = SDK 接入 + OTLP exporter**（用户 2026-09-04 追加修正，原"不引 OTLP"作废）：
  cmd/sproxy 装配 oteltracing，配置 `tracing.otel.otlp_endpoint` 时创建 OTLP 管道实际上报；
  重依赖（otlptrace 等）留在 ext/otel 独立 module，核心 go.mod 零新增三方依赖。
- **审查修复口径（追加）**：所有审查发现（含 Minor/建议级）必须当场修复，不延后。

## 全局约束（所有任务必须遵守）

1. **核心 go.mod 零新增三方依赖**（仅 stdlib + golang.org/x/* + gopkg.in/yaml.v3）；
   OTel 等重依赖只能存在于独立子 module（`pkg/telemetry/ext/otel`）。
2. **纯标准库测试**（无 testify/gomega）；127.0.0.1 loopback 绑定（禁 0.0.0.0/localhost）；
   Windows 兼容；`-race` 全绿。
3. **日志统一 log/slog**（新代码不混入 zap/logrus）；错误 `fmt.Errorf("...: %w")` 包装。
4. **cmd 薄逻辑**：非命令行逻辑下沉 `pkg/`；满足分层。
5. **TDD**：先写失败测试，看到失败再实现；边界场景全覆盖。
6. **lint 0 容忍**：主 go.mod + 每个子 go.mod 分别 `golangci-lint run` 0 issues。
7. 路径分隔符用 `filepath.Join`/`filepath.ToSlash`（Windows 兼容）。
8. 每个新文件带 SPDX 头：`Copyright 2026 The Cocomhub Authors. All rights reserved.`
   / `SPDX-License-Identifier: Apache-2.0`。

## 已核实关键现状（Plan agent + 自核）

| 项 | 现状文件 | 关键点 |
|----|---------|--------|
| AK 轮换 | `cmd/sclient/access_key.go`（仅 create）、`pkg/sproxysig/sproxysig.go`、`pkg/server/auth.go:200-224`（verifySproxySig 线性遍历 `cfg.AccessKeys`） | authMiddleware 每次从 `h.cfgPtr.Load()` 遍历 AccessKeys 做 constant-time 匹配 → 支持 **copy-on-write 更新运行时凭据** |
| AK 轮换 hub 面 | `pkg/tunnel/hub/auth.go:74-150` | `Authenticator` 持 `accessKeys []AccessKey` 无锁，`Authenticate` 线性匹配；**需加 RWMutex + SetAccessKeys 支持运行时更新**（否则"新 key 可 HTTP 面验签、hub 注册被拒"不一致） |
| AK 轮换 SIGHUP | `cmd/sproxy/root.go:729-775 handleSighup` | 只 Store 软配置；`access_keys` 无比较日志（不重载）。需新增：SIGHUP 时对比 access_keys、更新 cfgPtr + hub Authenticator + keyring |
| rateLimiter | `pkg/server/config_api.go:217`、`pkg/server/ratelimit.go`、`pkg/server/handlers.go:603-606` | Handlers 无 rateLimiter 字段；rl 是局部变量；ratelimit 无 UpdateConfig；`signalPostRL`（handlers.go:725-727）另有独立 rl |
| OTel | `pkg/telemetry/tracer.go`（Tracer 接口，原 `pkg/tunnel/tracing`）、`ext/otel/tracer.go`（适配器 New(t oteltrace.Tracer)）、`go.work` 已含、`cmd/sproxy/go.mod`/`cmd/sclient/go.mod` 未 require | 核心 `Tracer` = StartSpan/Inject；slog 日志层已带 trace_id/span_id（telemetry.WithContextHandler）；ext/otel 目前无 exporter 依赖 |
| 审计 UI | `pkg/server/audit.go`（RecordAudit→JSON stdout）、`handlers.go:458-461`（auditLogger 创建，默认 JSON stdout）、`web/static/index.html`（stats-modal 3 tab：统计/配置/Hub） | 录入点单点（RecordAudit）；Web 已有 tab 模式可复用；api/index.js 6 命名空间工厂 |
| 单位工具包 | `internal/size/size.go`（仅常量 KiB/MiB/GiB + 硬限制，**无解析/格式化函数**）、配置字节字段 `MaxStorageBytes`/`CloudSyncThreshold`/`CloudArchiveMaxBytes`/`ChunkSize` 均为裸 int64 | 无 `Parse`/`String`；config.example.yaml 字节值用裸数字（如 20971520）；viper mapstructure 解码 |

## 任务

### 任务 1：rateLimiter.UpdateConfig 热更新（PR-A，最小改动）✅ 已完成/已合并

**文件**：
- `pkg/server/ratelimit.go` — 加 `UpdateConfig(enabled bool, limit int, window time.Duration)`（复用 mu，重算 ipQuota）；加 `enabled` 字段，`Middleware` 内 `if !enabled { next(); return }` 短路（不重建 handler 链，规避 xfer LocalHandler 引用断开）。
- `pkg/server/handlers.go` — Handlers 加 `rateLimiter *RateLimiter` 字段；`RegisterRoutes` 把 `rl := NewRateLimiter(...)` 存入 `h.rateLimiter`；`signalPostRL` 的独立 rl 也存入（若存在，统一入口）。
- `pkg/server/config_api.go` — TODO 处接线：`h.rateLimiter.UpdateConfig(cfg.RateLimit.Enabled, cfg.RateLimit.Requests, cfg.RateLimit.Window)`。

**注意**：不要改动 `NewRateLimiter` 的签名（现有调用点不变）；UpdateConfig 只更新字段与 ipQuota，不清空 timestamps（旧窗口按新 window 自然过期）。

**验证**：`go test -race -count=1 ./pkg/server/...` + `make lint` + `make build` + `make test-packages`。

### 任务 1：rateLimiter.UpdateConfig 热更新（PR-A）✅ 已 squash 合并 #155

**文件**：
- `pkg/server/ratelimit.go` — 加 `UpdateConfig(enabled bool, limit int, window time.Duration)`（复用 mu，重算 ipQuota）；加 `enabled` 字段，`Middleware` 内 `if !enabled { next(); return }` 短路（不重建 handler 链，规避 xfer LocalHandler 引用断开）。
- `pkg/server/handlers.go` — Handlers 加 `rateLimiter *RateLimiter` 字段；`RegisterRoutes` 把 `rl := NewRateLimiter(...)` 存入 `h.rateLimiter`；`signalPostRL` 的独立 rl 也存入（若存在，统一入口）。
- `pkg/server/config_api.go` — TODO 处接线：`h.rateLimiter.UpdateConfig(cfg.RateLimit.Enabled, cfg.RateLimit.Requests, cfg.RateLimit.Window)`。

**注意**：不要改动 `NewRateLimiter` 的签名（现有调用点不变）；UpdateConfig 只更新字段与 ipQuota，不清空 timestamps（旧窗口按新 window 自然过期）。

**验证**：`go test -race -count=1 ./pkg/server/...` + `make lint` + `make build` + `make test-packages`。

**审查发现（3 条 Minor，用户要求全部修复）**：
- ratelimit.go `Allow()` 无锁读 limit：`Allow()` 首行 `if rl.limit <= 0`（:96）无锁——被 `UpdateConfig` 并发写。改为锁内读：把判断移入持锁区间，或先用 `rl.limit` 原子读。
- config_api.go `time.ParseDuration` 无单位数值被拒绝（`{"rate_limit_window":"1"}` → 400）：评估是否补一个友好提示（`must include unit like 5s`）。
- 黑盒测试 `do` helper 重复内联三处：抽公共 helper。
> 注：这三条在 squash 合并前已识别但未修。**后续单独补一个小 commit（基于 origin/master）修掉**——任务2 完成后处理。

### 任务 2：telemetry 装配（PR-B，→telemetry 重命名 + autoexport + OTLP exporter + 为 metric 扩展预埋）

**核心调整（用户 2026-09-04 追加）**：
1. **tracing → telemetry 命名 + 上移 `pkg/telemetry`**：核心包 `pkg/tunnel/tracing` 重命名为 **`pkg/telemetry`**
   （用户确认放 `pkg/telemetry` 而非 `pkg/tunnel/telemetry`——telemetry 不止 tunnel 可用，server/client 也消费；
   go.work 引用的子 module 目录同理 `pkg/telemetry/ext/otel`），包名/import 全部更新；
   命名空间为未来扩展 metric/log 辅助类型预留（telemetry 是比 tracing 更广的 umbrella）。
2. **exporter 用 `exporters/autoexport` 简化**：不再手写 `otlptracehttp.New(...)` 分支，
   改用 `go.opentelemetry.io/otel/exporters/autoexport`（环境变量驱动 OTLP 端点、
   `OTEL_EXPORTER_OTLP_ENDPOINT` 等标准约定），失败自动回落仅进程内；纯代码零硬编码端点。
3. **OTLP exporter 默认关**：`telemetry.otel.enabled=false` 时纯 slog；`enabled=true` 时 SDK 装配；
   endpoint 经 `autoexport` 从环境变量解析；配置里可显式 `otlp_endpoint` 覆写环境变量（可选）。

**文件**：
- `pkg/tunnel/tracing/` → **`pkg/telemetry/`**（目录重命名 + 上移；carrier.go / context.go / core.go / random.go / slog.go / tracelog.go / tracer.go / tracing.go + 测试）。
- `pkg/telemetry/ext/otel/provider.go`（新）— `NewProvider(opts...) (*Provider, error)`，`Tracer(name) core.Tracer`，`Shutdown(ctx)` 幂等；sampler = ParentBased(TraceIDRatioBased)；`WithAutoExport()` 用 `autoexport.NewSpanExporter` 创建 exporter + BatchSpanProcessor 接 TracerProvider；`autoexport` 失败 → 仅进程内 + Warn（不回退静默）。
- `pkg/telemetry/ext/otel/tracer.go` — `Tracer.StartSpan` 额外写 `core.SpanContextKey`（打通 OTel ↔ slog 日志 id）；Inject 用 OTel `TraceContext`。
- `pkg/server/config.go` — `Telemetry OTELConfig{Enabled bool, SampleRatio float64, OTLPEndpoint string}` 配置段（原 `Tracing` 段改名 `Telemetry`）+ SetDefaults + Validate。
- `go.work` — use 目录更新（pkg/telemetry/ext/otel）。
- `cmd/sproxy/go.mod`、`cmd/sclient/go.mod` — require + replace 到 `telemetry/ext/otel`。
- `cmd/sproxy/root.go` — 装配 `if cfg.Telemetry.OTEL.Enabled { p := oteltracing.NewProvider(...); ...; 注册 Shutdown }`；提供 `--otel` flag 可选。
- `cmd/sclient/root.go` — 同态（可选延后）。

**TDD 测试**：
- `pkg/telemetry/ext/otel/provider_test.go`：NewProvider 正常/越界/Shutdown 幂等/StartSpan 双 ctx 打通/autoexport 装配（用假环境变量或 mock exporter 断言 span 到达）。
- `pkg/server/config_test.go`：Telemetry 默认关、ratio 非法、endpoint 非法。
- 装配级：sproxy 开 OTel 后 `/api/files` 响应 Traceparent 头仍正确（不破坏 requestlog）。
- 重命名回归：全仓 import 指向新包无残留；`go build ./...` + `make test-all` 绿。

**验证**：`go test -race ./pkg/telemetry/... ./pkg/telemetry/ext/otel/... ./cmd/sproxy/... ./cmd/sclient/...` + `make build-all` + `make test-all` + `make lint`（含子 module）+ `make check-loopback`。

### 任务 3：Web UI 审计日志查看面板（PR-C）

**设计**：有界内存环形缓冲（`AuditRing`，默认 2048 条，配置 `audit.buffer_size`，0=关闭），
不落盘（与分享/云任务内存存储模式一致；审计留档交给日志 collector）。`RecordAudit` 同时 `Add` 到 ring。
`GET /api/audit` **只注册主 mux + authMiddleware**（不注册 localMux/tunnel 内层，避免经隧道无额外认证面；
与 /api/hub/nodes 同模式）。Web UI 在 direct 模式（配 AK/SK 走 SproxySig）可访问；tunnel 模式下为 404（前端 catch）。

**文件**：
- `pkg/server/audit.go` + `_test.go` — `AuditRing`（环形，thread-safe，Recent(limit, filter) 倒序）+ `AuditFilter{Action, Actor, Mesh, Since}`。
- `pkg/server/audit_handler.go`（新）+ `_test.go` — `GET /api/audit?limit=&action=&actor=&mesh=&since=`；响应 `{events:[...], total}`；limit 上限 ≤500；since 非法 400。
- `pkg/server/handlers.go` — Handlers 加 `auditRing *AuditRing`；`RegisterRoutes` 按 cfg 装配 + 注册路由（主 mux only）；`RecordAudit` 挂钩 Add。
- `pkg/server/config.go` — `Audit{BufferSize int}` 配置段（默认 2048）。
- `web/static/sclient/api/audit.js`（新）+ `api/index.js` 注册第 7 命名空间。
- `web/static/index.html` — stats-modal 加第 4 tab「审计」；`app.js` `showAudit()` + 渲染（纯函数 `app-render.auditTableHtml` 可测）；tab 切换接线。

**TDD 测试**：AuditRing 环形语义（N 条全保留、N+1 丢弃最旧、N=0 禁用）、过滤/倒序/limit 上界、并发 `-race`；
handler 黑盒（审计动作后查得到、无认证 401、**non-auth 直连 localMux 路径 404** 回归）；前端 auditTableHtml 转义 actor/mesh/detail 防 XSS。

**验证**：`go test -race ./pkg/server/...`、`make web-test`（node --check 全部 JS + upload 回归）、浏览器手测（审计 tab 拉取）。

### 任务 4：AK 轮换（PR-D，SIGHUP 重载 + API + KeyRing + hub 准入同步）

**设计**（最大，拆分 2 个 submit-commit 顺序合并，但 1 个 PR 保证"新 key 可 HTTP 面 + hub 注册"一致性）：
- **KeyRing**（`pkg/server/keyring.go` 新）：内存 `map[ak]→KeyEntry{Secret, Mesh, AddedAt, ExpireAt, state}` + RWMutex；
  每 mesh 上限（默认 8）、ttl 上限（默认 30d）；Add/Lookup/Expire/Delete/Snapshot/Len。
- **verifySproxySig 合并查询**（`pkg/server/auth.go`）：先线性遍历静态 `cfg.AccessKeys`，未命中查 `h.keyring.Lookup`；
  keyring 命中用其 Secret/mesh；旧 key 保留过渡期间仍验签。
- **管理端点**（主 mux + authMiddleware 保护；调用方须是 active key 且 mesh 一致）：
  - `POST /api/access_keys/rotate {mesh, ttl}` → 服务端 crypto/rand 生成 AK=`sk[-<mesh>]-<16hex>`/SK=32B hex（可参考 cmd/sclient generateAccessKeyPair 逻辑），返回一次明文 SK（唯一一次）。
  - `GET /api/access_keys` → 列表。
  - `POST /api/access_keys/{ak}/expire {until}` / `DELETE /api/access_keys/{ak}?force=`（仅 expiring 可删，`--force` 强制）。
  - 全部 `RecordAudit`（action=access_key_rotate/expire/delete）。
- **hub.Authenticator 动态化**（`pkg/tunnel/hub/auth.go`）：加 `mu sync.RWMutex` + `SetAccessKeys([]AccessKey)`；`Authenticate` 读锁。
- **SIGHUP 重载**（`cmd/sproxy/root.go`）：`handleSighup` 增加 access_keys 对比——变更时 Store cfgPtr + 更新 keyring + `authenticator.SetAccessKeys(...)`；删除/修改"access_keys 修改需重启"的 Warn（改为 Info 生效）。同时更新 `CLAUDE.md` 的 SIGHUP 重载范围说明。
- **sclient**：`cmd/sclient/access_key.go` 加 `rotate/expire/delete/list` 子命令；`pkg/client/accesskey.go`（新）领域 API `RotateAccessKey/ListAccessKeys/ExpireAccessKey/DeleteAccessKey`（走 coreRequest 签名）。
- **cmd/sproxy/root.go 装配**：创建 keyring → 注入 `h.SetKeyRing`（如需要）→ keyring Snapshot 更新回调挂到 Authenticator.SetAccessKeys。

**TDD 测试**：KeyRing 表驱动（非法 SK/重复/上限/Expire 不存在/Delete 未过期拒绝/Snapshot 排序）；并发 `-race`；
auth 合并查询（动态 key 验签成功、旧 key 过渡访问、双 key 共存）；rotate 端点黑盒（active key 签名成功→新 key 立即访问；过期 key 401）；e2e 全链路（真实二进制 rotate→新 key 访问→hub 注册）。

**验证**：`go test -race ./pkg/server/... ./pkg/tunnel/hub/... ./cmd/sproxy/... ./cmd/sclient/...` + `make lint`（含子 module）+ `make build-all` + `make test-all` + `make check-loopback`。

### 任务 5：字节单位工具包（PR-E，类型化大小配置）

**动机**（用户追加需求）：避免直接配字节，改用 MB/MiB 等人类单位配置，行为类似 `time.Duration`。
工具包需提供 `Parse` 函数与 `String` 等方法，便于：① flag 命令行解析（`flag.Value`）；② 配置文件解析
（yaml/mapstructure/viper 解码）；③ 日志以人类可视化单位输出。工具包命名需合理以便后续扩展其他辅助类型。

**已有铺垫**：`internal/size/` 已有常量（KiB/MiB/GiB）与硬限制。**新工具包**（建议路径 `pkg/unit` 或 `pkg/bytefmt`，
命名待定稿，需与现有 `internal/size` 关系协调——可新增 `internal/size/bytesize.go` 扩展而非新包，但用户要求"合理取名便于扩展其他辅助类型"，倾向独立 `pkg/unit` 聚合字节/百分比/速率等）。**本计划倾向 `pkg/unit`**：`unit.ByteSize` 类型 + 未来可扩展 `unit.Rate`、`unit.Percent` 等。

**核心接口（仿 time.Duration）**：
```go
type ByteSize int64

func ParseByteSize(s string) (ByteSize, error) // "10MiB" "1.5GB" "1024" "500 KB"（大小写/单位同义）
func (b ByteSize) String() string              // 人类可读：自动选 KiB/MiB/GiB/TiB 或 B
func (b ByteSize) Int64() int64
func (b ByteSize) MarshalText() ([]byte, error)  // encoding.TextMarshaler → "10MiB"
func (b *ByteSize) UnmarshalText(text []byte) error // 供 yaml/mapstructure/viper 解码
func (b ByteSize) Set(s string) error           // flag.Value 接口，供 pflag/pflag
```
- 十进制（KB/MB/GB/TB，×1000）与二进制（KiB/MiB/GiB/TiB，×1024）都支持；裸数字按字节；
  支持小数（如 1.5GB）；大小写不敏感；单位同义词（B/byte/bytes）。
- 解析失败返回哨兵错误（`ErrInvalidSize` 或类似），供调用方映射状态码。
- 负数允许（配额可负？限制由调用方决定），空串报错。

**接入现有配置**（把字节 int64 字段逐步换用 `unit.ByteSize`，或至少提供通用解码 hook）：
- 评估范围：`MaxStorageBytes`、`CloudSyncThreshold`、`CloudArchiveMaxBytes`、`ChunkSize`（若可配）。
- **最小可行**：工具包 + 单测 + pflag 集成测试（`--max-storage-bytes 10GiB`）；config 解码用
  mapstructure `DecodeHookFunc`（把 string→ByteSize）或给字段改类型。
- `config.example.yaml` 注释更新为人类单位示例。
- 日志输出：凡打印字节处用 `ByteSize.String()`（人类可视化）。

**TDD 测试**：
- `pkg/unit/bytesize_test.go`：表驱动 Parse（各单位/大小写/小数/空格/非法/空串/裸数字/负数）、String 往返（Parse→String→Parse 幂等）、MarshalText/UnmarshalText 往返、flag.Value Set 集成。
- config 解码：viper/mapstructure 把 `"10GiB"` 解码为字节值；`config.example.yaml` 示例。
- pflag：`--max-storage-bytes 10GiB` 等 flag 解析。

**验证**：`go test -race ./pkg/unit/... ./pkg/server/... ./cmd/sproxy/...` + `make lint` + `make build-all` + `make test-all` + `make check-loopback`。

## 实施顺序

按依赖与风险递增：**任务 1（限流热更新）→ 任务 2（OTel 装配）→ 任务 3（审计面板）→ 任务 4（AK 轮换，最大最险）→ 任务 5（单位工具包，独立新增）**。
任务相互独立可并行分支；每个任务完成时：

1. TDD 先写失败测试（核心 + 全部边界场景）
2. `make lint`（主 + 每子 go.mod，0 issues）+ `make build-all` + `make test-all` + `make check-loopback`
3. 派独立对抗式审查 agent → 修复**全部**发现（含 Minor/参考，用户明确要求不延后），必要时补回归测试再复审
4. CI 全绿 → 用户人工 squash 合并
5. 每任务写 learnings（已入库）+ 最后写阶段 6 复盘文档

## 验证（总）

- 每个任务：`go test -race -count=1 <受影响包>/...` + `make lint` + `make build-all` + `make test-all`
- 涉前端：`make web-test` + 浏览器手测（审计 tab、无 console error）
- 手测样例：
  - 任务1：PUT /api/config 把 rate_limit_requests 从 100 改 1 → 下个请求 429；enabled=false 后放行
  - 任务2：sproxy 配 `tracing.otel.enabled: true` + `otlp_endpoint: http://collector:4318` → span 上报；
    未配 endpoint 仅进程内；关闭默认 slog
  - 任务3：sclient 执行 delete/rename 后 `GET /api/audit` 返回对应事件；隧道模式 404
  - 任务4：rotate 出旧/新 key → 旧 key 请求仍 200 → expire → delete；SIGHUP 改 yaml access_keys 即时生效；hub 节点用新 key 注册成功
  - 任务5：`--max-storage-bytes 10GiB`、yaml `max_storage_bytes: 20MiB` 解析生效；日志打印 `Used: 1.2 GiB`

## 涉改文件汇总

- `pkg/server/ratelimit.go`、`config_api.go`、`handlers.go`、`config.go`、`auth.go`、`audit.go`、`audit_handler.go`(新)、`keyring.go`(新)
- `pkg/tunnel/hub/auth.go`
- `pkg/telemetry/ext/otel/provider.go`(新)、`tracer.go`、`go.mod`、`go.sum`
- `cmd/sproxy/root.go`、`cmd/sproxy/go.mod`、`cmd/sproxy/go.sum`
- `cmd/sclient/access_key.go`、`access_key_rotate.go`(新)、`root.go`、`go.mod`、`go.sum`
- `pkg/client/accesskey.go`(新)
- `pkg/unit/bytesize.go`(新)、`pkg/unit/bytesize_test.go`(新)
- `config.example.yaml`、`web/static/index.html`、`app.js`、`app-render.js`、`api/index.js`、`api/audit.js`(新) + 对应 `*.test.js`
- `docs/architecture.md`/`docs/cli.md`（OTel 配置、单位配置说明）、`CLAUDE.md`（SIGHUP 重载范围更新 + 技术债务清单移除 rateLimiter TODO）
