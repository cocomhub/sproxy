# 第 10 轮审查经验总结：pkg/client 包代码质量修复（含测试与完整性审计）

> 审查日期：2026-07-31 ~ 2026-08-01
> 项目：sproxy
> 目标包：`pkg/client/` + `cmd/sclient/` 关联 CLI + `pkg/server/` 协议核对
> 目标：三阶段轮次（代码→测试→完整性）

## 修复统计

| 指标 | 轮次 1（代码） | 轮次 2（测试） | 轮次 3（完整性） | 合计 |
|------|:-------------:|:-------------:|:---------------:|:----:|
| Commits | 13（含 3 fixup） | 5 | 2 | **20** |
| 修改文件 | 22 | 20 测试 + 2 生产 | 3 + 1 CLI 测试 | **40+** |
| 修复问题 | ~58（5C+29I+24S） | ~65（4C+28I+33S） | 3 minor + 1 破坏性 | **~125** |
| 新增行 | +682 | - | - | **+1067** |
| 删除行 | -475 | - | - | **-444** |

## 三阶段轮次设计评估

### 轮次 1（代码质量）：收敛代码缺陷

**范围**：`pkg/client/` 14 个非测试文件

**关键修复**：
| 类型 | 问题 | 方案 |
|------|------|------|
| Critical | 硬编码状态字符串 vs 常量 | 替换为 `TaskStatus*` |
| Critical | cancelled 任务被当作已完成 | 改为直接返回错误终止流程 |
| Critical | `tryResumeSession` nil pointer | 专用结果类型 `tryResumeResult` |
| 并发安全 | `downloadOneChunk` TOCTOU | `context.WithCancel` 替代共享指针 |
| 资源泄漏 | io.Pipe goroutine 不感知 ctx | 增加 `select` 监听 `ctx.Done()` |
| 死代码 | KVStoreRegistry 25+ 行 | 直接移除 |
| API 设计 | `NewCloudDownloadChain` 返回 `(chain, error)` | 增加参数校验 |
| API 设计 | `Extra` 内部化 + `MarshalJSON` | 保护内部状态保持 JSON 兼容 |
| API 设计 | `NewChainManager` 变参 Option | `(store KVStore, opts ...ChainOption)` |

**回归**：修复中引入 3 个 bug（变量遮蔽 x2、`archive.go` 遗漏），重复审查捕获确认"约 1/3 严重问题来自修复自身"。

### 轮次 2（测试优化）：细化测试边界与质量

**范围**：`pkg/client/` 20 个测试文件

**关键修复**：
| 类型 | 问题 | 方案 |
|------|------|------|
| 死测试 | `TestClientBatchRename_MissingChecksum` 因 9999 端口无服务通过 | 改为 httptest 真实校验 |
| 名实不符 | `TestFileClient_Download_EmptyOutputPath` 传入非空路径 | 改名+新增真正空路径测试 |
| 断言不足 | `TestGetConfig` 17 字段仅验 3 个 | 扩展到全部字段 |
| 存储字段 | `TestGetStats` 遗漏 4 个存储字段 | mock+断言补充 |
| Fuzz 不变量 | `calcChunkSize` 缺 2 的幂倍不变性 | 添加不变量断言 |
| 哨兵错误 | 硬编码中文错误消息检查 | 替换为 `errors.Is` + 哨兵错误 |
| t.Parallel() | ~20 个测试缺失 | 全部补充 |
| t.Cleanup | `defer ts.Close()` 不统一 | 统一为 `t.Cleanup` |

### 轮次 3（完整性检查）：跨包一致性验证

**范围**：`pkg/client/` ↔ `pkg/server/` ↔ `cmd/sclient/`

**发现**：
- 36 个 HTTP 端点全对齐，方法/路径/JSON 字段一致
- 数据结构差异：`TotalFiles int` vs `int64`、`ArchiveResult` 缺少 `omitempty` — 均已修复
- CLI 绕过 FileClient 的 5 个命令有合理原因（relay、diag 不需要完整客户端）

### 最终审查发现的 5 个回归

| 问题 | 原因 | 是否已修 |
|------|------|:--------:|
| CLI `archiveName` 默认空导致 4 测试失败 | `NewCloudDownloadChain` 签名变更未同步 CLI 默认值 | ✅ |
| `apply_edits.py` 被提交 | 代理临时文件误提交 | ✅ |
| `chunked.go:830` 遗漏 `containsPathTraversal` | 替换不完全 | ✅ |
| `store_json.go:53` `os.Remove` 错误忽略 | `Save` 未同步 `Delete` 的修复 | ✅ |
| `output_test.go` TotalFiles `int64` vs `int` | 类型变更未同步 CLI 测试 | ✅ |

## 通用问题模式

### 1. 签名变更的连锁影响（3 次出现）

`NewCloudDownloadChain` 返回 `(chain, error)` 影响 9 处调用方+CLI 默认值。`HandleConfigShow` 加 `io.Writer` 影响 4 处调用方+2 测试适配。`TotalFiles:int64→int` 导致 CLI 测试编译失败。

**教训**：签名变更后，grep 搜索 `cmd/sclient/` 全目录确认所有调用方。编译器只能检查类型不匹配，不能检查语义变化（默认值、行为变更）。

### 2. 并发代理间文件冲突导致修复遗漏

F6 代理承诺修复 9 个文件但实际只提交了 5 个。`archive.go` 的 3 处修复完全丢失，直到重复审查才被发现。

**教训**：每个修复代理提交后，主进程必须执行 `git diff --stat HEAD~1` 确认提交的文件列表符合预期。

### 3. 类型变更的连锁影响

`int64` → `int` 的类型变更绕过 `go vet` 和 `go build` 在 `pkg/client` 的检查，仅当编译 `cmd/sclient` 时才暴露。原因是 `output_test.go` 使用匿名结构体字面量。

**教训**：修改导出类型字段时，grep `cmd/sclient/` 全目录搜索对应字段。

### 4. 变量遮蔽在修复代码中高发

2 个 fixup 中出现了 `err` 变量遮蔽问题。修复代码中新增语句使用 `:=` 短声明时，必须检查外部作用域是否有同名变量。

**教训**：新增变量赋值时使用新名称，避免与函数参数或返回值同名。

## 流程改进建议

### 增加"最终审查前测试红线"

当前流程：修复→最终审查→发现问题→修复→重新审查。应在最终审查启动前，执行全量测试 + 跨包编译：

```bash
go build ./pkg/... ./cmd/...
go test -race ./pkg/... ./cmd/...
```

这能拦截 Final Review 阶段的大部分回归（CLI 测试失败、跨包类型不匹配）。

### "类型变更自动同步检查"机制

修改导出类型的字段时，扫描所有跨包引用：

```bash
grep -rn "FieldName" cmd/sclient/ --include="*.go"
grep -rn "FieldName" test/ --include="*.go"
```

每个被命中的文件必须逐个确认是否需要同步更新。

## 测试最佳实践

### 使用 `io.Writer` 注入替代 `CaptureStdout`

`HandleConfigShow(cfg, w io.Writer)` 的设计让测试可以直接注入 `strings.Builder` 验证输出，不再需要替换全局 `os.Stdout`。这使得 `t.Parallel()` 和安全简单。

### 哨兵错误优先于文本匹配

```go
// 差
if strings.Contains(err.Error(), "存储空间不足") { ... }

// 好
var ErrStorageFull = errors.New("storage full")
if errors.Is(err, ErrStorageFull) { ... }
```

### 测试名称与行为一致

`TestFileClient_Download_EmptyOutputPath` 声称测试空输出路径但实际传入非空路径。测试名称比没有测试更危险——它制造了"已覆盖"的假象。

## API 设计经验

1. **默认严格模式** — `WithTransportFallback()` 的设计让调用方显式选择回退
2. **Option 变参优于配置结构体** — `NewChainManager(store, opts...)` 向后兼容、可扩展
3. **内部字段+自定义 JSON 序列化** — `ChainResult.Extra` 内部化模式保护 API 契约
4. **结果类型替代多返回值** — `tryResumeResult` 替代 `(result, error, bool)` 消除误用

## changelog

### 修复清单

| 类别 | 数量 | 典型问题 |
|------|:----:|---------|
| 并发安全 | 3 | TOCTOU context.WithCancel、io.Pipe ctx 取消、lock-in-callback |
| 参数校验 | 7 | archiveName/LocalDir/taskID/versionID/maxStorageBytes |
| 路径穿越 | 7 | `containsPathTraversal` 替换 7 处 `strings.Contains("..")` |
| 死代码清理 | 25+ 行 | 移除 KVStoreRegistry、jsonKVStoreFactory |
| 资源泄漏 | 3 | JSONKVStore tmp、out.Close、io.Pipe goroutine |
| 协议不一致 | 3 | TotalFiles int64→int、ArchiveResult omitempty |
| 测试死测试 | 2 | `MissingChecksum` 因端口不可达通过、名称不符 |
| 测试断言不足 | 3 | GetConfig 17→3、GetStats 缺 4 字段、HandleConfigShow 43% |
| Fuzz 不变量 | 1 | calcChunkSize 2 的幂倍不变性 |
| 哨兵错误 | 3 | 替换硬编码中文错误消息检查 |
| 回归修复 | 6 | 变量遮蔽 x2、archive.go 遗漏、CLI 测试 x2、类型同步 |
| 跨包类型同步 | 1 | TotalFiles int64→int 波及 CLI 测试 |