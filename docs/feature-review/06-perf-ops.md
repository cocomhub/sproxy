# 审查：性能与运维（传输管线 + mux 热路径 + 限流 + 基准 + telemetry）

- **批次**：6
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### 传输管线 + mux 热路径
- **分块传输**：并发（默认 4）+ 断点续传 + 64 KiB 密文块恒定内存 + gzip 响应中间件。
- **流控窗口**：mux `FrameWindowUpdate` + **单流 buffered 上限 fail-closed**（`stream.go:16-28`：`pendingOverflowLimit = DefaultWindowSize + MaxFramePayload`——对端超窗口灌数据 → `abortWindowViolation` 只 Abort 该流）；`pendingEntryLimit`（空帧/重复 CloseWrite 推高条目数也被拦）。
- **热路径优化**：快路径 2 次原子 load + 1 次 Add + 无竞争锁配对、零分配（`stream.go` 注释 + 基准验证）。
- **缓冲水位**：mux buffer_watermark 阈值自适应 + 防抖 + BufferAdjustments 指标。

### 多实例协调限流
- **coord_backend=file**（`bandwidth_coord.go`）：字节配额落共享后端（storage 根 bandwidth/ 子目录）——多实例共享同一 per-owner 配额（N×bps 总量正确）；`coordinated` 开关；等待不拒绝语义。
- **单实例 token 桶**：`NewRateLimiter`（per-IP token bucket）+ `UpdateConfig` 热更新 + SetCoordinator。

### 隔离与连接池
- `netutil.DefaultTransport` 共享工厂（生产装配层）+ `IsolatedTransport`（SDK/测试隔离，TLSClientConfig=nil 防意外复用）；R19/R20 门禁（禁 &http.Transport{} 裸建）；`pkg/testutil.IsolatedClient` 收敛。

### 内存观测 + 安全开关
- **pprof 认证保护**：`/debug/pprof/` 挂 `debug_pprof_enabled` 显式开关 + **authMiddleware 包装**（未认证 401——`pprof_handlers.go:49`）。
- **metrics_token 认证**：`subtle.ConstantTimeCompare`（`metrics.go:652`）——query/Bearer 双通道常量时间比较，仅门 /metrics 零回归。

### 基准套件 + telemetry
- **基准**：`benchmark_test.go`（Upload/Download/ConcurrentUploads/ChunkedUpload 1 MiB）+ CI Benchmark job（10 分钟超时取消重试 + I/O 塌陷守卫 + benchwatch 进程外看门狗）。
- **telemetry**：span + slog + traceparent 传播 + OTLP 导出骨架（telemetry.enabled + otlp_endpoint）。
- **自识别**：/version、/livez、/readyz、/healthz、/metrics、/api/hub/nodes|services|stats、/api/mesh/status、SIGHUP 软配置热重载。

### 测试
- mux 流控/热路径测试 + ratelimit/bandwidth_coord 测试 + metrics 认证测试（#497）；`go test` 全绿。

## 验证方式

- 源码逐路径审查（mux 流控判据 + 限流协调 + pprof/metrics 认证 + 基准守卫）
- `go test -count=1 -timeout 180s -run 'TestMux|TestRateLimit|TestMetrics|TestPprof' ./pkg/tunnel/mux/... ./pkg/server/...` → **ok**
