# 批次 6 审查：性能与运维

> 本批 3 路并发对抗审查：R6.1 传输管线+mux / R6.2 限流+内存观测 / R6.3 基准+telemetry。
> 基线：master `e428acbe`。产出文件 `06-perf-ops-*.md`。

## 审查目标功能（roadmap 6.1）

- **R6.1 传输管线 + mux 热路径**：分块上传/下载并发（默认 4）、断点续传、流式窗口（mux
  FrameWindowUpdate 流控 + 单流 buffered 上限 fail-closed）、64 KiB 密文块恒定内存、gzip 响应
  中间件；mux 热路径优化（快路径 2 次原子 load + 1 次 Add + 无竞争锁配对、零分配）。
- **R6.2 隔离与连接池 + 多实例协调限流 + 内存观测**：netutil.DefaultTransport 共享工厂 +
  IsolatedTransport；R19/R20 门禁；rate_limit.bandwidth.coord_backend（local 进程内 token 桶 /
  file 文件原子计数共享配额）+ coordinated 开关；/debug/pprof 受认证保护 + /metrics 分配指标 +
  mux 缓冲水位自动调整。
- **R6.3 基准套件 + telemetry + 自识别**：pkg/server/benchmark_test.go（Upload/Download/
  ConcurrentUploads/ChunkedUpload 1 MiB 基准）；CI Benchmark job（10 分钟超时取消重试 + I/O
  塌陷守卫）；telemetry 追踪骨架（span + slog + traceparent + OTLP 导出骨架）；/version、/livez、
  /readyz、/healthz、/metrics、/api/hub/nodes|services|stats、/api/mesh/status、SIGHUP 软配置热重载。

## 关键文件

- R6.1：pkg/tunnel/mux/*.go（stream.go 热路径）、pkg/files/chunked_*.go、pkg/server/gzip.go
- R6.2：pkg/netutil/*.go、pkg/server/bandwidth_coord.go、pkg/server/ratelimit.go、
  pkg/server/pprof_handlers.go、pkg/tunnel/mux/buffer*.go（水位）
- R6.3：pkg/server/benchmark_test.go、pkg/tunnel/tracing/*.go、pkg/server/handlers_lifecycle.go、
  docs/archive/benchmark-ci.md

## 审查维度与关注点

### 正确性
- mux 流控（窗口更新语义、buffered 上限 fail-closed 是否可被耗尽）；热路径原子操作配对
- 协调限流（file 后端原子计数、跨进程一致性、等待不拒绝语义）；水位调整防抖
- 基准测试统计有效性（同环境可比性、I/O 塌陷守卫逻辑）

### 安全性
- pprof 认证保护（能否绕过）；协调限流 file 后端的文件权限/竞争
- gzip 中间件（压缩炸弹/内容类型过滤）

### 可用性 / 可维护性
- SIGHUP 热重载范围（哪些配置真正生效）；自识别端点信息泄漏（版本/节点列表）
- 测试覆盖；基准基线管理

## 输出格式

每路审查产出 `06-perf-ops-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
