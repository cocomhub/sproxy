# PR #102 经验总结

## 一、通用问题模式

### 1. goroutine 中 t.Errorf 并发不安全

**问题**：多个测试在 goroutine 中直接调用 `t.Errorf`/`t.Fatal`，在 `t.Parallel()` 模式下可能导致 data race 或 panic。

**涉及文件**：
- `pkg/client/concurrent_test.go` — 5 个 goroutine 并发上传
- `pkg/server/store_test.go` — 100 个 goroutine 并发标记 chunk
- `pkg/tunnel/hub/dht_test.go` — 50 个 goroutine 并发查询

**修复模式**：
```go
// Before (有问题的模式)
go func() {
    if err != nil {
        t.Errorf("...: %v", err)
    }
}()

// After (安全模式)
errCh := make(chan error, N)
go func() {
    if err != nil {
        errCh <- fmt.Errorf("...: %w", err)
    }
}()
wg.Wait()
close(errCh)
for err := range errCh {
    t.Error(err)
}
```

### 2. 异步 goroutine 调度顺序不确定

**问题**：测试假设 goroutine 按特定顺序启动和运行，但 Go 调度器不保证顺序。

**案例**：`TestCloudDownloadManager_ConcurrentSemaphoreLimit` 假设 task1/task2 先进入 downloading，但 task3 可能先获取信号量。

**经验**：并发测试不应假定特定 goroutine 的执行顺序，而应验证**最终状态**的正确性。

### 3. Windows 文件系统 mtime 精度问题

**问题**：Windows 文件系统 mtime 精度为 100ns，但实际刷新粒度可能更高（~10-15ms）。连续两次写入同一文件可能在同一 mtime 窗口内，导致缓存失效判断失败。

**修复**：使用 `time.Sleep(10ms)` 或写入不同大小内容确保 mtime+size 双重变化。

### 4. context.WithCancel(ctx) 的级联取消

**问题**：`dlCtx = context.WithCancel(ctx)` 创建的 context 在 `ctx` 取消时也会被取消，导致异步重试条件 `ctx.Err() != nil && dlCtx.Err() == nil` 永远无法满足。

**修复**：改为 `context.WithCancel(context.Background())`，使 `dlCtx` 独立于调用方 context。

### 5. 子串匹配的误报风险

**问题**：`isStorageFullError` 使用 `strings.Contains(lower, "507")` 匹配 HTTP 状态码，但 "507" 可能出现在时间戳、版本号、文件名中。

**经验**：子串匹配错误消息时，避免匹配纯数字。优先使用语义短语（如 "storage full"、"insufficient storage"）。

### 6. json:"-" 字段的序列化陷阱

**问题**：`CloudDownloadChain.client` 和 `opts` 标记为 `json:"-"`，不会被序列化到 JSON 状态文件。`Restore` 时为零值，导致 `Run()` 时 `c.client` 为 nil。

**修复**：将 `pollInterval`/`timeout`/`keepFiles` 作为直接字段 + `json:"include"`，`client` 保持 `json:"-"` 由 `ResumeChain` 手动 `setClient`。

### 7. 文件下载失败后残留不完整文件

**问题**：`Archive`/`ArchiveDir` 在 `io.Copy` 失败后不清理已创建的文件，残留不完整文件污染文件系统。

**修复**：在 `io.Copy` 失败后调用 `os.Remove(outputPath)` 清理。

### 8. sync.Once 的 close channel 幂等性

**问题**：`Close()` 和 `StopFlush()` 都直接 `close(channel)`，如果被多次调用会 panic。

**修复**：使用 `closeOnce sync.Once` 确保 channel 只关闭一次。

---

## 二、代码审查流程经验

### 1. 独立子代理 + 文件即接口

**模式**：每个审查子代理从零开始，不依赖会话历史。中间产物写入文件。

**效果**：避免了大模型上下文膨胀导致的"确认偏误"，子代理更容易发现新问题。

**成本**：3 个独立子代理 × 各 40-80 次工具调用，约 300K tokens。

### 2. 先分析后执行

**模式**：每个修复前先分析多角度方案，确认后再执行。

**效果**：避免了"修了但没修对"的情况。例如 D3（os.Rename 不覆盖）在分析阶段确认是 Go 1.26 已修复的误报，节省了无效修复。

### 3. 分类分批处理

**模式**：问题按严重度分组，分批处理。Critical → Important → Suggestion。

**效果**：避免了"修了 50 个小问题但留下了 1 个 data race"的情况。

### 4. 验证闭环

**模式**：每个修复后运行 `go build` + `go test -race -run <affected>` + `golangci-lint`。

**效果**：批次 1-5 的所有修复均通过验证，无回归。

---

## 三、Go 测试最佳实践

### 1. 并发测试

| 模式 | 推荐 | 不推荐 |
|------|------|--------|
| 错误收集 | `errCh <- err` + `wg.Wait()` + `close(errCh)` + `for range errCh` | goroutine 中直接 `t.Errorf` |
| 顺序验证 | 检查最终状态，不假定执行顺序 | 假定特定 goroutine 先启动 |
| 资源隔离 | 每个 goroutine 各自创建资源 | 共享 `t.TempDir()` 等 |

### 2. 异步测试

- 使用 `time.After(deadline)` 作为超时保护
- 使用轮询 + `time.Sleep(10ms)` 等待异步状态，而非 `time.Sleep` 固定等待
- 测试完成后释放阻塞资源（`close(blockCh)`）避免 TempDir 清理失败

### 3. 文件系统测试

- Windows mtime 精度有限，需要时加 `time.Sleep(10ms)`
- 文件修改后 mtime+size 双重保障缓存失效
- TempDir 清理失败时手动 `RemoveAll` 子目录

---

## 四、API 设计经验

### 1. Option 模式优于平铺参数

```go
// 不推荐
func CreateShare(ctx, filename, ttl, maxDownloads, oneTime)

// 推荐
func CreateShare(ctx, filename, opts ...ShareOption)
```

### 2. 导出字段不如 Option 函数

```go
// 不推荐 — 可直接修改，破坏封装
type FileClient struct {
    ChunkSize int64
}

// 推荐 — 通过 Option 设置，不可直接修改
type FileClient struct {
    chunkSize int64
}
func WithChunkSize(n int64) Option { ... }
```

### 3. 错误吞没不如记录

```go
// 不推荐
_ = m.store.Save(ctx, key, state)

// 推荐
if err := m.store.Save(ctx, key, state); err != nil {
    return fmt.Errorf("保存状态失败: %w", err)
}
```

### 4. 结构体参数优于多参数回调

```go
// 不推荐
reportFn func(ctx, phase, msg string, current, total int)

// 推荐
type ProgressInfo struct { Phase, Message string; Current, Total int }
type ProgressFunc func(ctx, info ProgressInfo)
```

---

## 五、changelog

| 类型 | 数量 | 说明 |
|------|------|------|
| Critical 修复 | 4 | goroutine 泄漏、data race、dlCtx 级联取消、StopFlush double-close |
| Important 修复 | 18 | 路径穿越、错误吞没、URL 校验、缓存失控、串行轮询等 |
| 测试修复 | 4 | 死测试、无断言测试、goroutine 中 t.Errorf |
| 测试追加 | 50+ | Option 函数、边界测试、并发测试 |
| 代码删除 | 3 | StopFlush 死代码、重复测试 |
| 覆盖率提升 | 78.6% → 84.7% | pkg/client 包 |

---

## 六、第二轮审查新增发现（2026-07-30）

### 新增通用问题模式

#### 9. goroutine 泄漏 — pollAllTasks 超时后 goroutine 仍在运行

**问题**：`CloudDownloadChain.pollAllTasks` 在 `timeoutCtx` 超时后从 `select` 返回，但已启动的 HTTP goroutine 仍在后台运行，尝试向 `resultCh` 发送数据。

**修复**：使用 `sync.WaitGroup` 等待所有 goroutine 完成，`select` 非阻塞发送，在错误路径消费剩余结果避免 goroutine 阻塞。

#### 10. 非事务性状态机 — 状态变更与远程 API 非原子

**问题**：`CloudDownloadChain.Run` 的 switch 流中，`submitTasks` 成功后、`CurrentPhase` 更新前进程崩溃，恢复后重新提交任务，导致服务端生成重复任务。

**修复**：在每个阶段切换点立即调用 `saveState` 持久化新状态，缩短崩溃窗口。

#### 11. 全局可变状态 — 测试隔离

**问题**：`runnerRegistry` 是包级全局 `map`，`chain_test.go` 的 `init()` 注册的 `test_chain` runner 影响包内所有测试。并行测试共享全局注册表存在竞态。

**修复**：`ChainManager` 持有实例注册表，`NewChainManager` 从全局拷贝默认值，测试通过创建独立实例隔离。

#### 12. Context 级联取消影响状态持久化

**问题**：`reportFn` 中 `saveState` 使用主流程的 `ctx`，当主流程 `ctx` 被取消后，状态持久化也失败，链式操作无法恢复。

**修复**：`saveState` 使用 `context.Background()` 分离主流程取消和状态持久化。

#### 13. 锁内调用回调 — 阻塞风险

**问题**：`chunked.go` 在持有 `u.mu` 时调用 `u.client.progressFn` 回调，如果回调耗时较长（如终端写入、网络上报），阻塞其他 goroutine 的进度更新。

**修复**：先更新进度数据，释放锁，再在锁外调用回调。

#### 14. 接口抽象泄漏 — 类型断言依赖

**问题**：`ResumeChain` 必须做 `runner.(*CloudDownloadChain)` 类型断言才能设置非持久化字段，新增 runner 类型时 `ResumeChain` 需要同步修改。

**修复**：`ChainRunner` 接口增加 `SetClient`/`SetOptions` 方法，`ResumeChain` 统一接口调用消除类型断言。

#### 15. ChainResult 暴露内部实现

**问题**：`ChainResult.Raw` 是公开字段，外部代码通过 `result.Raw.(*CloudDownloadChain)` 类型断言访问具体字段，破坏接口隔离。

**修复**：`ChainResult` 增加 `LocalPath()`/`KeepFiles()` 访问方法，外部改用方法调用，后续可将 `Raw` 改为私有。

### 新增流程最佳实践

- **独立分析代理**：每个批次的分析独立启动子代理，从零开始，不依赖会话历史，避免上下文污染
- **方案对比表格化**：分析阶段必须用表格对比各方案的优缺点、改动量、风险，让用户一目了然
- **误报排除**：分析阶段必须验证每个问题是否真实存在（如 `MoveFileEx` 同卷重命名是原子的、Go duration 格式与 `time.ParseDuration` 兼容）
- **initError 模式**：Option 函数初始化失败时，不应只默默记录日志，应设置 `initError` 字段并通过 `InitError()` 方法暴露给调用方
- **`TestMain` 替代 `init()`**：测试中的全局注册应在 `TestMain` 中显式注册+注销，而非 `init()` 隐式注册

### 新增测试最佳实践

#### goroutine 超时保护

```go
cancel()
select {
case <-srvErr:
case <-time.After(2 * time.Second):
    t.Error("goroutine did not exit after cancel")
}
```

#### 测试命名必须与实际行为一致

`TestWithTunnel_ValidKey` 和 `TestWithTunnel_InvalidKey` 的测试体完全颠倒，会严重误导维护者。

#### 死测试的识别

"不 panic 就通过"的占位测试：`_ = chain` 消音操作、无 `t.Error`/`t.Fatal` 断言、无被测方法调用，应删除或补全。

### 新增 changelog（第二轮）

| 类型 | 数量 | 说明 |
|------|------|------|
| 并发安全修复 | 4 | goroutine 变量捕获、pollAllTasks 泄漏、锁内回调、锁竞争 |
| 错误处理修复 | 6 | saveState 吞错、List 静默跳过、json.Marshal 吞错、WithTunnel 静默吞错、LoadConfig 副作用、报告Fn ctx 级联 |
| API 设计修复 | 5 | ChainResult 访问方法、ChainRunner 接口扩展、runnerRegistry 实例化、非事务性状态机、文档注释 |
| 测试修复 | 9 | 死测试、命名颠倒、弱断言、冗余测试、命名误导、时间脆性、错误消息硬编码、init 注册、goroutine 超时 |
| 误报排除 | 3 | Windows Rename 原子性、Go duration 格式、ListHubNodes 响应格式 |

---

## 七、第三至六轮审查新增发现（2026-07-30）

### 新增通用问题模式

#### 16. 并发 0 死锁 — 无缓冲通道的阻塞风险

**问题**：`WithChunkedConcurrency(0)` 覆盖默认值 `defaultConcurrency=4` 后，`make(chan struct{}, 0)` 创建无缓冲通道，`sem <- struct{}{}` 在 goroutine 创建前就阻塞，导致永久死锁。

**防御**：
- Option 层校验：`if n <= 0 { n = 1 }`
- 运行时防御：`run()` 方法开头 `if concurrency <= 0 { concurrency = 1 }`

#### 17. 空任务列表静默成功

**问题**：`pollAllTasks` 在 `TaskIDs` 为空时，`resultCh` 缓冲区为 0，无 goroutine 写入，`for range resultCh` 立即完成，空任务列表被误认为"全部完成"。

**修复**：在 `pollAllTasks` 开头校验 `len(c.TaskIDs) == 0` 并返回错误。

#### 18. 测试名与实际行为不符

**问题**：
- `BenchmarkChunkedUpload` 实际调用 `Upload()` 而非 `ChunkedUpload()`
- `TestFileClient_Download_EmptyOutputPath` 实际传入非空路径
- `TestFileClient_CloudDownloadChain_NoManager` 实际测试空 URL 列表
- `TestClientBatchDelete_ContinueOnError` 实际测试 nil 文件列表

**经验**：测试名是维护者理解测试意图的第一线索，名不符实比无测试更危险。

#### 19. 响应体读取缺少大小限制

**问题**：`version.go` 中 `ListVersions`/`RestoreVersion`/`DeleteVersion` 用 `io.ReadAll(resp.Body)` 无限制读取，其他文件已统一使用 `io.LimitReader(resp.Body, 4<<10)`。

**防御**：所有 `io.ReadAll(resp.Body)` 调用都应使用 `io.LimitReader` 限制最大读取大小。

#### 20. 预分配文件后失败残留

**问题**：`ChunkedDownload` 先 `Truncate(fileSize)` 预分配大文件空间，如果后续下载失败，预分配的大文件未被删除。

**修复**：使用 `defer` 模式确保文件在失败时被清理，先 `Close()` 再 `Remove()`（Windows 兼容）。

### 新增流程最佳实践

- **全量测试验证**：修复后必须运行全量 `go test -count=1 -race`，仅运行指定测试可能遗漏回归
- **lint 零容忍**：`golangci-lint run` 必须在修复后通过，0 issues
- **误报分析机制**：对于多次报告的审查发现，应启动独立分析代理深入确认，区分：
  - 误报（如 `mapstructure` tag 解析错误、`t.Errorf` 多 goroutine 安全）
  - 真实但无害（如双重关闭，`os.File.Close()` 幂等）
  - 真实且有风险（如 `concurrency=0` 死锁，需修复）
- **测试名修正优先级**：名不符实的测试应优先于边界测试修正，因为误导性比缺失更糟糕

### 新增 changelog（第三至六轮）

| 类型 | 数量 | 说明 |
|------|------|------|
| 并发安全修复 | 3 | concurrency 死锁、ChainManager 锁、errCh select 保护 |
| 功能性 Bug 修复 | 10 | FormatByte GB/TB、created_at、空 TaskIDs、空参数校验、死代码、maxBatchURLs、文件清理等 |
| 错误处理修复 | 6 | URL 校验、HTTP 状态码检查、Sscanf 改用 Atoi、nil panic、Download 清理、Upload 空路径 |
| API 设计修复 | 7 | ChainResult 接口、doJSON 迁移、响应结构体统一、错误消息中文化、输入参数校验、LimitReader、context.WithoutCancel |
| 测试修复 | 16 | 重复测试删除、测试名修正、t.Context() 替换、t.Log→t.Fatal、Benchmark 重命名、handler t.Errorf→channel 等 |
| 误报排除 | 5 | mapstructure tag、concurrency 死锁（确认需修复）、双重关闭、handler t.Errorf、ChainManager 锁（确认需修复） |
| 误报分析 | 5 | 独立代理深入分析确认，区分误报/无害/真实 |
| **累计修复** | **130+** | **40+ 文件，所有测试通过，lint 0 问题** |