# 2026-07-31-sproxy-pkgclient-review-experience.md

## 第八轮审查修复统计

| 批次 | 内容 | 问题数 | 提交 |
|------|------|--------|------|
| 批次 1 | 并发安全 + 阻塞性缺陷 | 3 | `9f25a2c` |
| 批次 2 | 功能性 Bug + 边界条件 + API 设计 | 11 | `2cda051` |
| 批次 3 | 测试质量全面提升 | 16 | `24198f4` |
| 批次 4 | API 设计改进 | 5 | `729f58b` |
| 批次 5 | Suggestion 级别 | 17 | `c63a035` |
| **合计** | | **56** | **5 个提交** |

### 严重度分布

| 等级 | 数量 | 占比 |
|------|------|------|
| Critical | 12 | 21% |
| Important | 29 | 52% |
| Suggestion | 15 | 27% |

## 通用问题模式

### 1. 修复子代理文件变更遗漏风险
**现象**：批次 4 修复代理提交的 `b853751` 仅包含 `chunked.go` 的空白符变更（`gofmt -w`），实际 API 修复未提交到分支。
**教训**：修复代理提交前必须通过 `git diff --stat` 确认所有目标文件已变更，而不是仅验证编译通过。
**防御**：修复子代理的验证步骤中增加 `git diff --stat <文件列表>` 检查。

### 2. `context.Background()` 在测试中系统性滥用
**现象**：17 处 `context.Background()` 替代 `t.Context()`，涉及 6 个测试文件。`t.Context()` 在 Go 1.24+ 中可用，测试结束时自动取消，避免 goroutine 泄漏。
**教训**：编写新测试时养成使用 `t.Context()` 的习惯，review 时检查 `context.Background()` 的使用。
**防御**：在 Phase 2 审查范畴中增加"测试中使用 `t.Context()` 替代 `context.Background()`"的检查项。

### 3. 死测试 — 测试名称与实际行为不一致
**现象**：本轮发现 4 个名不副实的测试（`BenchmarkUpload_4MB`、`TestSubmitError_StorageFull`、`TestClientChunkedUpload_Resume`、`TestOpen_DefaultDir`），以及 2 个死测试（路径穿越测试无有效断言）。
**教训**：测试名称与行为不一致是比缺少测试更严重的问题——它制造了"已覆盖"的假象。
**防御**：在 Phase 2 审查测试时，强制验证测试名称与断言内容的对应关系。

### 4. mock handler 中的错误处理不完整
**现象**：`mockUploadHandler` 中 `os.Create` 失败忽略、`out.Write` 失败忽略、`mockListFilesHandler` 中 `os.ReadDir` 失败忽略、`cloudTestServer` 中 `json.Decode` 失败忽略。
**教训**：mock handler 也应遵循生产代码的错误处理规范，否则测试行为不可预测。
**防御**：在 Phase 2 审查测试文件时，检查 mock handler 的 I/O 错误处理。

### 5. 公开字段泄漏实现细节
**现象**：`ChainResult.Raw` 是公开字段，外部代码通过类型断言（`result.Raw.(*CloudDownloadChain)`）直接访问内部实现，破坏了接口隔离。
**教训**：公开字段是 API 契约的一部分，一旦导出就难以收回。接口设计优先使用方法而非公开字段。
**防御**：在 Phase 2 审查中增加"检查公开字段是否泄漏实现细节"的检查项。

## 代码审查流程改进

### 1. 修复验证增加 diff 检查
当前修复子代理的验证步骤包括：
- `go build`
- `go test -race`
- `golangci-lint run`

应增加：
- `git diff --stat <预期变更文件列表>` — 确认所有目标文件已变更

### 2. 复检阶段增加向后兼容性检查
当前 Phase 4D 复检要求包括：
- 确认修复是否解决了原始问题
- 检查修复代码是否引入了新问题
- 确认测试覆盖

应增加：
- 确认向后兼容性（API 签名、行为变更、返回值类型变更）

### 3. 子代理提交前的工作区检查
批次 4 修复代理提交了 `b853751`，但该提交只包含了 `chunked.go` 的空白符变更。原因是修复代理的 `git add -A` 和 `git commit` 在工作区混入了批次 3 的变更。

**改进**：修复子代理启动前应执行 `git stash` 或 `git checkout` 确保工作区干净，提交前确认 `git diff --cached --stat` 只包含目标变更。

## 测试最佳实践

### 1. 测试名称规范
- 测试名称必须精确描述测试内容
- Benchmark 名称必须与被测函数一致（`BenchmarkChunkedUpload` 应调用 `ChunkedUpload`）
- 避免使用暗示未覆盖场景的名称（如"Resume"但未测试续传）

### 2. 断言强度分级
- `bw > 0` 是弱断言，应设合理下限
- `task.Filename != ""` 是伪断言（mock 已保证非空），应验证具体值
- 路径穿越测试不应只验证 `filepath.Base` 的副作用，应验证防护是否真正生效

### 3. 测试生命周期
- 优先使用 `t.Context()` 而非 `context.Background()`
- 优先使用 `t.Cleanup` 而非 `defer`（panic 安全）
- 纯逻辑测试应添加 `t.Parallel()`

## API 设计经验

### 1. 接口隔离原则
- 公开字段会泄漏实现细节，优先使用方法
- 返回类型统一（值类型 vs 指针类型）——同类 API 应保持一致

### 2. Validate() 模式
- `Validate()` 方法名暗示"只读校验"，不应修改接收者
- 如需设置默认值，使用 `SetDefaults()` 或 `Normalize()` 方法

### 3. 原子写入模式
- 文件写入使用 `.tmp` + `os.Rename` 模式，避免写入中断留下不完整文件
- 标准模式：`os.Create(tmp)` → `io.Copy(out, src)` → `out.Close()` → `os.Rename(tmp, target)`

## Changelog

### 第八轮审查修复统计

| 类型 | 数量 | 示例 |
|------|------|------|
| 并发安全 | 3 | `filepath.Abs` 错误处理、类型断言 ok 检查、`pollAllTasks` 空列表防护 |
| 功能性 Bug | 11 | Validate 拆分、Windows 删除文件、原子写入、ChainResult 私有化 |
| 测试质量 | 16 | 死测试修复、t.Context() 替换、断言增强、冗余删除 |
| API 设计 | 5 | ListShares 类型统一、nil slice、常量提取 |
| Suggestion | 17 | 错误信息统一、边界值补充、Fuzz 不变量 |
| Lint 修复 | 1 | if-else → switch |
| **合计** | **56** | |

### 全七轮审查累计修复统计

| 轮次 | 问题数 | 主要修复内容 |
|------|--------|-------------|
| 第 1 轮 | 20+ | 初始审查、基础问题修复 |
| 第 2 轮 | 45+ | 并发安全、API 设计 |
| 第 3 轮 | 12+ | 功能性 Bug、测试修复 |
| 第 4 轮 | 8+ | 测试质量 |
| 第 5 轮 | 6+ | 并发死锁、双重关闭 |
| 第 6 轮 | 8+ | 功能性 Bug、API 一致性 |
| 第 7 轮 | 25+ | 并发安全、API 设计、协议兼容性 |
| 第 8 轮 | 56 | 全部 5 个批次全面修复 |
| **累计** | **180+** | **pkg/client 全面质量提升** |

### 文件变更统计

| 文件 | 行数变更 | 说明 |
|------|---------|------|
| `pkg/client/chunked.go` | +1019/−1019 | 空白符格式化（gofmt） |
| `pkg/client/client_test.go` | +30/−20 | mock handler 错误处理、测试增强 |
| `pkg/client/chain.go` | +20/−5 | Raw 私有化、logger 字段、ResetRunners |
| `pkg/client/chain_cloud_download.go` | +15/−8 | 状态机持久化、常量提取、空列表校验 |
| `pkg/client/config.go` | +20/−10 | SetDefaults + Validate 拆分 |
| `pkg/client/archive.go` | +20/−5 | 原子写入、Windows 删除文件修复 |
| `pkg/client/cloud.go` | +15/−3 | nil slice、空参数校验 |
| `pkg/client/share.go` | +5/−5 | ListShares 类型统一 |
| `pkg/client/format.go` | +5/−2 | KB/MB/GB/TB 常量 |
| `pkg/client/store.go` | +5/−1 | Close 清空数据 |
| 其他测试文件 | +90/−55 | t.Context()、t.Parallel()、断言增强 |
| `cmd/sclient/output.go` | +2/−2 | ListShares 签名同步 |
| `test/e2e_test.go` | +3/−3 | AsCloudDownloadChain() |