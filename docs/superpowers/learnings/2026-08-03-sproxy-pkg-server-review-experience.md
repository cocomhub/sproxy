# 2026-08-03 sproxy-pkg-server-review-experience.md

## 审查统计

| 维度 | 数值 |
|:----|:----:|
| 审查范围 | `pkg/server/` 全部 37 个非测试文件 + 41 个测试文件（~18k + ~11k LOC） |
| 审查方式 | 8 组并行代码审查 + 5 组并行测试审查 + 1 组完整性审查 |
| 修复 commit | 22 个 |
| 修改文件 | 52 个 |
| 净增行 | +769 / -373 |

## 修复统计

| 严重度 | 发现 | 已修复 | 误报降级 |
|:------|:---:|:-----:|:--------:|
| Critical | 14 | 14 | 0 |
| Important | 28 | 25 | 3 |
| Suggestion | 18 | 18 | 0 |
| 测试问题 | 84 | 84 | 0 |
| **总计** | **144** | **141** | **3** |

## 通用问题模式

### 1. 安全类问题
- **符号链接路径穿越** — `archive.go`、`share.go`、`dirs.go` 中的文件操作均未检查符号链接。`joinSafePath` 只检查路径字符串前缀，不解析符号链接目标。修复方案：在 `joinSafePath` 中增加 `EvalSymlinks` 检查，在文件操作点使用 `os.Lstat` 替代 `os.Stat`。
- **Content-Disposition 注入** — 4 处直接将文件名嵌入 HTTP 头，未使用 `mime.FormatMediaType` 转义。修复方案：封装 `formatContentDisposition` 辅助函数。

### 2. 并发安全类问题
- **hashPool 多次归还** — `sync.Pool` 的 hash 对象在 3 个错误路径上分别 `Put`，正常路径 `Put` 一次。分散三处的模式是 data race 隐患。修复方案：`defer hashPool.Put(hash)` 统一归还。
- **锁顺序不一致** — `executeDownload` 中 `mu → dirtyMu`，`flushDirty` 中 `dirtyMu → mu`，存在死锁风险。修复方案：统一锁获取顺序。
- **goroutine 泄漏** — `executeDownload` 重试分支通过 `go executeDownload()` 新建 goroutine，不受 `Close()` 控制。修复方案：`sync.WaitGroup` 追踪所有 goroutine。

### 3. 认证绕过类问题
- **cfg==nil 时 fail-open** — `authMiddleware` 在 `cfgPtr.Load()` 返回 nil 时直接 `next(w, r)`。修复方案：改为返回 500（fail-close）。
- **空 Bearer token 通过认证** — `strings.TrimPrefix("Bearer ", "Bearer ")` 返回空字符串，`ConstantTimeCompare("", "")` 返回 1。修复方案：token 为空时返回 401。

### 4. 限流器设计缺陷
- **limit=0 全拒绝** — `len(timestamps) >= 0` 永远为 true，所有请求被 429 拒绝。修复方案：`Allow()` 中 `limit <= 0` 直接返回 true。
- **window=0 无限放行** — `now.Add(0) = now`，`After(now)` 永远 false，所有时间戳被清除。修复方案：`NewRateLimiter` 中 `window <= 0` 校正为 `time.Second`。

### 5. 资源泄漏类问题
- **io.Pipe goroutine 泄漏** — `archiveHandler` 的 `io.Pipe` goroutine 在客户端断开后仍在运行。修复方案：`defer pw.Close()` 确保 goroutine 在任何情况下都会关闭 pipe。
- **响应体泄漏** — 测试代码中 `resp.Body` 未关闭。修复方案：`t.Cleanup` 统一关闭。

### 6. 测试质量类问题
- **死测试（无有效断言）** — 8 个测试函数"不 panic 就通过"。修复方案：替换为有效断言或删除。
- **裸指针 data race** — 10 个测试直接修改 `CreateTask()` 返回的裸指针，未持锁。修复方案：用 `SnapshotTask` 获取副本，或 `mgr.mu.Lock()` 包裹。
- **`t.Logf` 替代 `t.Errorf`** — 预期错误路径使用了日志而非断言。修复方案：替换为 `t.Errorf`。
- **`json.Decode` 错误忽略** — 22 处 `Decode` 返回值被忽略。修复方案：添加 `t.Fatalf` 检查。

## 代码审查流程改进

### 1. 文件隔离批次分组
按文件隔离分组，确保每个文件只出现在一个批次中，批次间可并发修复。从过去按功能/严重度分组（导致文件冲突和串行执行）改为按文件分组，修复时间从推测串行 ~8 轮缩短到并行 ~2 轮。

### 2. 跨代理交叉验证
多个独立代理从不同角度发现同一问题时，可信度显著提高。本次 `parseDuration` 静默吞错误被 B/G/H 三组独立发现，Content-Disposition 未转义被 B/C 两组独立发现。交叉发现的问题优先级上调一级。

### 3. 同类问题搜索
在 Phase 3 汇总后启动并行搜索代理，搜索同类模式。本次搜索了 7 种模式共 7 组，确认 Content-Disposition 4 处全部迁移，downloadsDirName 4 处全部迁移，io.Pipe 2 处全部修复，无遗漏。

### 4. worktree 管理
本次使用 21 个独立 worktree 进行并行修复，每个 worktree 在独立分支上提交，然后 cherry-pick 到主分支。优点是：完全隔离，互不干扰。缺点是：worktree 管理开销大（21 个 worktree 需要手动 cherry-pick 和清理），且可能导致合并冲突。

### 5. 分析阶段问题降级
分析阶段对 3 个问题进行了降级处理：
- `Release` 语义不一致 → 误报，`-sizeDelta` 在 `sizeDelta < 0` 时实际为正数
- 缺少 checksum 时仍删除 → Low，已有 `f.Checksum == ""` 检查
- 锁顺序不一致 → 误报，`flushDirty` 释放 `dirtyMu` 后才能获取 `mu`

## 测试最佳实践

### 1. 死测试检测
死测试的核心特征：调用方法后只 log 不 assert、传入 nil 验证"不 panic"、`t.Logf` 替代 `t.Errorf`。本次共发现 8 个死测试。

### 2. t.Context() 使用
Go 1.24+ 的 `t.Context()` 在测试结束时自动取消，避免 goroutine 泄漏。本次审查发现，所有 18 个小型测试文件已正确使用 `t.Context()`，无 `context.Background()`。

### 3. time.Sleep 替代
异步等待测试应使用轮询模式替代 `time.Sleep`。本次修复了 `TestUploadStore_CleanupSessionAfter`、`TestRelayHandlerRoundTrip`、`TestRateLimiter_RecoversAfterWindow` 中的 `time.Sleep`。

### 4. 断言三要素
每个测试断言应包含：期望值、实际值、清晰的错误消息。`if total < 5` 与 `expected total >= 6` 矛盾时排查价值归零。

## API 设计经验

### 破坏性变更标注
以下变更属于 API 契约级别的破坏性变更，需要通知所有外部调用方：
1. 批量删除 checksum 拒绝语义（旧：仍删除 → 新：拒绝删除）
2. 无效 subdir 返回 400（旧：200 + 空数组 → 新：400）
3. TTL 格式无效返回 400（旧：静默使用 24h → 新：400）
4. CORS 拒绝返回 403（旧：继续处理 → 新：403）
5. SaveConfig 权限 0600（旧：0644 → 新：0600）

### 包内签名变更
以下函数签名变更仅影响包内调用方，所有调用方已同步更新：
- `parseDuration` 返回 `(time.Duration, error)` 替代 `time.Duration`
- `handleNoBearerToken` 去掉返回值
- `mergeChunksWithHash`/`mergeOneChunk` 新增 `ctx context.Context`

## 后续建议

1. **补充 `formatContentDisposition` 和 `resolveAndValidateFile` 单元测试** — 已为 `formatContentDisposition` 添加了 4 个测试用例，`resolveAndValidateFile` 因逻辑简单（委托给已有测试覆盖的函数），暂未添加。
2. **合并分支** — `review/pkg-server-code-review` 分支包含 22 个 commit，确认所有修复正确后合并回 master。
3. **CHANGELOG 更新** — 标注所有破坏性行为变更。