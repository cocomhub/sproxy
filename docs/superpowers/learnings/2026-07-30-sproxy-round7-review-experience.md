# 第七轮审查经验总结：pkg/client 包

> 日期：2026-07-30
> 项目：sproxy
> 分支：feat/external-library-enhancement
> 本轮审查范围：pkg/client/ 全部 34 个文件（6 个独立子代理）

---

## 本轮修复统计

| 指标 | 数值 |
|------|------|
| 审查问题总数 | 154 |
| Critical 修复 | 16（100% 修复） |
| Important 修复 | 44（主要修复） |
| Suggestion 处理 | 94（部分处理） |
| 涉及文件 | 21 个 |
| 代码变更 | +399 / -147 行 |
| 新增提交 | 1 个（70114eb） |

### 累计修复总览（7 轮审查）

| 轮次 | 审查范围 | 发现问题 | 修复文件 | 代码变更 |
|------|---------|---------|---------|---------|
| 1-2 | pkg/client 全包 | 70+ | 30+ | +500/-200 |
| 3-6 | pkg/client 深度迭代 | 60+ | 28 | +753/-293 |
| **7** | **pkg/client 全包（全新审查）** | **154** | **21** | **+399/-147** |
| **累计** | | **~280+** | **40+** | **+1652/-640** |

---

## 通用问题模式（新增）

### GP#7.1 测试中的 `time.Sleep` 等待服务就绪（flaky test 根源）
**发现**：`client_options_test.go` 中 4 处 xfer tunnel 测试使用 `time.Sleep(50ms)` 等待 `tunB.Serve` goroutine 就绪。在 CI 高负载下可能不足。
**修复**：提取 `waitForTunnel` 轮询函数（10×10ms=100ms），用 `tun.Do` 重试确认服务就绪。
**防御**：所有测试中避免硬编码 sleep 等待 goroutine 就绪，改为轮询重试模式。

### GP#7.2 测试中 goroutine 直接关闭 `resp.Body` 而不消耗
**发现**：`TestXferTunnelConcurrentStreams` 中 goroutine 直接 `resp.Body.Close()` 而不读取 body，在 mux 隧道中可能导致底层流未被完全消耗。
**修复**：先 `io.Copy(io.Discard, resp.Body)` 再关闭。
**防御**：`http.Response.Body` 应始终先读取再关闭，模拟标准 `http.Client` 的使用模式。

### GP#7.3 测试中 pipe 的 b 端被丢弃导致 goroutine 泄漏
**发现**：`TestGetTunnelMux` 的 Dial 函数中 `_ = b` 丢弃 b 端，mux 的 3 个 goroutine（readLoop/writeLoop/pingLoop）永远阻塞。
**修复**：`t.Cleanup` 关闭缓存的 mux 触发共享 `closeOnce` 完成清理。
**防御**：`xfertest.Pipe()` 返回的两端连接必须同时关闭，不能丢弃一端。

### GP#7.4 修复引入的 `CreateShare` TTL 格式不兼容
**发现**：`int64(cfg.ttl.Seconds())` 发送纯整数（如 `86400`），但服务端用 `time.ParseDuration` 解析，要求字符串包含单位后缀（如 `"86400s"`）。
**修复**：改为 `fmt.Sprintf("%ds", int64(cfg.ttl.Seconds()))`。
**防御**：客户端与服务端之间的协议变更必须验证两端兼容性，不能仅改一端。

### GP#7.5 `StructCodec.ToMap` JSON 中间格式的数字精度丢失
**发现**：`json.Marshal(v)` → `json.Unmarshal` 到 `map[string]any`，Go 默认将数字解码为 `float64`。`int64` 超过 2^53 时精度丢失。
**修复**：使用 `json.NewDecoder` + `UseNumber()` 保留数字精度，`FromMap` 中通过 `convertNumbers` 递归将 `json.Number` 转为 `int64`/`float64`。
**防御**：通过 JSON 中间格式做 struct ↔ map 转换时，必须使用 `json.Number` 保留数字精度。

### GP#7.6 测试中 `TotalFiles` 类型变更未同步到 `output_test.go`
**发现**：`StatsResponse.DiskUsage.TotalFiles` 从 `int` 改为 `int64` 后，`output_test.go` 中的匿名结构体字段类型未同步更新，导致编译错误。
**修复**：将 `output_test.go` 中对应的 `TotalFiles int` 改为 `int64`。
**防御**：修改导出类型字段时，检查所有调用方（包括测试文件）中的匿名结构体、类型断言等是否同步更新。

---

## 代码审查流程改进

### 1. 细粒度分工效果显著
本轮审查 34 个文件拆分为 6 个子代理（G1-G6），每个审查 3-8 个文件。相比前几轮的 15+ 文件/代理，本轮发现了更多深层问题（如 `StructCodec.ToMap` 精度丢失、`calcChunkSize` 整数溢出等），验证了"每个代理 ≤10 文件"的规则。

### 2. 独立分析 + 复检闭环有效
批次 1-5 的修复全部经过独立复检代理验证，发现并修复了 1 个回归问题（`CreateShare` TTL 格式不兼容）。复检环节 catch 了只有端到端运行时才会暴露的协议兼容性问题。

### 3. 测试文件独立分配代理
本轮将测试文件拆分为 G4/G5/G6 三个独立代理，测试审查更加聚焦（死测试、无效断言、并发安全等维度），发现了 27 个测试相关问题。

---

## 测试最佳实践

### 新增实践

| 实践 | 说明 |
|------|------|
| 测试中的数据完整性验证 | 并发上传测试不仅验证"不 panic"，还验证 `chunkCalls` 和 `completeCalls` 的具体数量 |
| 请求体验证 | mock handler 应解析请求体并验证字段值，不能仅检查状态码 |
| Load 隔离性验证 | `MemoryKVStore.Load` 应验证返回的 map 是深拷贝而非内部存储引用 |
| 负 TTL 替代 time.Sleep | 测试缓存过期时使用负 TTL 实现立即过期，而非固定等待 |
| 测试文件名变更同步 | 修改导出类型字段时，检查所有测试文件中的匿名结构体是否同步更新 |

---

## API 设计经验

| 经验 | 说明 |
|------|------|
| `InitError()` 模式 | Option 函数初始化失败时，不应仅记录日志，应设置 `initError` 字段并通过 `InitError()` 方法暴露给调用方 |
| 哨兵错误链 | 所有方法在 HTTP 404 时应使用 `ErrNotFound` 包装，方便调用方使用 `errors.Is` 判断 |
| 路径穿越防御 | 客户端层应尽早拒绝 `..` 路径，不依赖服务端校验 |
| 协议格式选择 | 服务端-客户端之间的 duration 等字段应使用通用格式（秒数、ISO 8601），避免 Go 特有格式 |
| `ChainResult` 接口抽象 | 通用结果类型不应硬编码具体实现的类型断言，应使用 `Extra map` 等通用字段 |

---

## Changelog（本轮修复）

| 类型 | 数量 | 代表问题 |
|------|------|----------|
| **并发安全** | 4 | calcChunkSize 整数溢出、pollAllTasks 取消传播、flaky sleep 替换、pipe b 端泄漏 |
| **功能性 Bug** | 10 | JSON 精度丢失、ErrNotFound 语义、路径穿越、FormatETA 边界值、TotalFiles 类型 |
| **API 设计** | 11 | 字段重命名、ChainResult 抽象、ResumeChain 选项恢复、SaveConfig 权限 |
| **测试修复** | 13 | 死测试修复、断言补充、请求体验证、中文消息移除 |
| **合计** | **38** | 21 个文件，+399/-147 行 |