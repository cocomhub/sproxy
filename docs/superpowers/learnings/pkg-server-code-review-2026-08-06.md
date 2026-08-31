# pkg/server 代码审查与修复 — 经验总结

> 日期：2026-08-06 ~ 2026-08-07
> 范围：pkg/server/ 全量 41 个非测试文件 + 43 个测试文件
> 审查源：10 个并行审查代理 + 8 个同类问题搜索代理
> 修复：10 个批次，11 个 commit，~50 个文件变更

## 修复统计

| 严重度 | 发现 | 已修复 | 备注 |
|:------:|:----:|:------:|------|
| Critical | 24 | 22 | 2 项标记为 TODO（SaveConfig 脱敏、孤儿文件） |
| Important | 99 | 87 | 12 项已在修复中合并或确认为误报 |
| Suggestion | 90 | 80 | 10 项保留不采纳（NewMetrics 保留、hashPool 已有防护等） |
| **总计** | **213** | **~189** | 修复率 ~89% |

## 通用问题模式

### 1. 错误处理：MkdirAll 失败后不 return
出现 3 处（`chunked_upload.go`、`upload_store.go`、`cloud_download.go`），模式相同：目录创建失败后仅记录日志，继续执行后续操作，必然因目录不存在而失败，错误信息误导。

**修复模式：** MkdirAll 失败后立即 return + sendJSONResponse + ShouldRetry 提示。

### 2. 并行修复冲突：共享工作区 git reset 回退
8 个并行修复代理在共享工作区互相执行 `git checkout`/`git reset`，导致 6 个批次的变更全部丢失。最终改用串行修复。

**教训：** 并行修复必须使用隔离 worktree，或串行执行。禁止在共享工作区同时运行多个 `git checkout`/`git reset` 操作的代理。

### 3. SSRF 后验证错误被吞没
`fmt.Errorf("ssrf post-request: %w", err)` 中 `err` 是 nil（`Do(req)` 成功），本应使用 `err2`。导致 SSRF 防护触发时错误信息完全丢失。

**教训：** `err` 和 `err2` 变量名在作用域重叠时极易混淆。应使用描述性名称或在内层作用域复用 `err`。

### 4. goroutine 泄漏：io.Pipe + 客户端断开
`archive.go` 中 `io.Pipe` 的写入 goroutine 在客户端断开后不退出，因为 `io.Copy(w, pr)` 返回后 `pr` 未关闭。

**修复模式：** `pr.Close()` + `sync.Once` 保护双重关闭 + context 传递到 `addFileToTar`。

### 5. 上下文取消缺失
`writeFileAtomically`、`restoreVersionHandler`、`accessShareHandler` 等多处 `io.Copy` 操作不响应客户端断开。

**修复模式：** 引入 `copyWithContext` 辅助函数，每次 Read/Write 前检查 `ctx.Done()`。

### 6. 存储会计损坏
`CancelTask` 释放 `task.TotalSize`（可能已被进度回调更新为实际大小），而非初始预留量，导致过量释放。

**修复模式：** 引入 `ReservedSize` 字段，与 `TotalSize` 分离，`CancelTask`/`failTask` 释放 ReservedSize。

### 7. TOCTOU 竞态
`delete`、`rename`、`rmdir` 多处存在"先检查后操作"的竞态窗口。

**修复模式：** 基于 fd 操作（`delete`）、保留检查 + TODO 注释（`rename`）、双重检查（`rmdir`）。

## 流程改进建议

### 1. 并行修复隔离
- 并行修复必须使用独立 worktree 隔离，或串行执行
- 禁止在共享工作区同时运行多个 `git checkout`/`git reset` 操作的代理
- 每个修复代理应提交后再启动下一个

### 2. 声明与实现一致性检查
- Phase 5 最终审查必须逐 commit 验证 commit message 与 diff 一致
- 可借助 `git diff --stat HEAD~1` 快速验证

### 3. 同类问题搜索
- 本审查 Phase 3 H1 发现了 12 个额外问题（占总结的 6%）
- 路径穿越类搜索结果为"无必须修复"——证明 validate.go 的防护体系完整
- 后续审查应优先使用同类搜索

### 4. 批次确认流程
- 使用 `AskUserQuestion` 工具强制用户选择方案，避免"继续"不等于"同意"
- 方案对比表必须包含边界行为说明

## 测试最佳实践

### 1. SSRF 防护测试
- `httptest.NewServer` 监听 127.0.0.1，会被 SSRF 防护拦截
- 测试中需设置 `AllowPrivate: true` 或提供公网 mock server
- 修复后应检查所有使用了 `httptest.NewServer` 的测试

### 2. 并发测试
- `-race` 下测试时间显著增加，context timeout 需留足余量（3 倍）
- `wg.Go` 配合 `recoveryGuard` 模式可防止 panic 导致 `wg.Wait()` 永久阻塞

### 3. 网络测试
- 依赖 DNS 解析的测试（如 `TestValidateURLHost_PublicHostname`）
- 在无网络环境中可能失败，应使用 mock DNS 或 httptest 替代

## 中间产物清理

- 审查报告目录：`.superpowers/sdd/pkg-server-review/`（已清理）
- 临时脚本：`fix_changes.py`、`fix_all.py`、`fix.py`（已删除）
- 审查代理副本：`4caf671b-ccd2-4a01-a22b-7b0cea0c5061/tasks/`（会话结束自动清理）

## 修复文件清单

| 文件 | 主要修复内容 |
|------|-------------|
| `response.go` | sendJSONResponse buffer、删除 Finished/Code 死代码 |
| `metrics.go` | strings.Builder + writeMetric 辅助函数 |
| `config_api.go` | rebuildLogger 同步、RateLimit 校验 |
| `config.go` | Validate 增加 Window 校验、SaveConfig TODO |
| `checksum.go` | hex.EncodeToString、io.CopyBuffer 256KB |
| `errors.go` | errMsgFileExists → errFmtFileExists |
| `auth.go` | APIKeys 启用时截断 fallthrough |
| `cors.go` | CORS 配置化、* 不反射 origin |
| `gzip.go` | WriteHeader 跳过 206/204/304 |
| `ratelimit.go` | per-IP 令牌桶限流、limit<=0 回退 |
| `upload_handler.go` | copyWithContext、handleDuplicateFile 顺序调整 |
| `chunked_upload.go` | MkdirAll 失败返回、临时文件名、幂等检查 |
| `upload_store.go` | NewUploadStore 返回 error、drain 受 wg 追踪、提取辅助函数 |
| `download_handler.go` | 复用文件句柄计算 checksum、countingWriter |
| `chunked_download.go` | 空文件 200、ParseInt、消除重试、metrics |
| `delete_handler.go` | 基于 fd 操作、审计日志 |
| `rename_handler.go` | TOCTOU TODO、审计日志、统一路径解析 |
| `list_handler.go` | 统一搜索响应、Warn 日志、日志降级 |
| `dirs.go` | 双重检查、分隔符注释 |
| `stats.go` | StorageManager 缓存、首次扫描 503、ScannedAt |
| `storage_manager.go` | LastScanTime、遗漏目录统计 |
| `share.go` | Peek/Stat/Consume、token 重试、Lstat、copyWithContext |
| `relay.go` | 排空逻辑、path.Clean、JSON 错误日志、RawMessage |
| `hub_handler.go` | JSON 编码错误日志 |
| `cloud_download.go` | ReservedSize、failTask 释放、Close 取消、MkdirAll 报错 |
| `cloud_download_handler.go` | 空字节过滤、统一 URL |
| `archive.go` | goroutine 泄漏修复、路径前缀、递归深度、大小限制、严格模式 |
| `cloud_archive_handler.go` | 流式 checksum、孤儿文件 TODO |
| `version.go` | 流式 checksum 写入、删除清理、Sync/Close 检查、排序稳定、versionID |
| `checksum_store.go` | Windows 回退、Set 重试、Rename defer 解锁 |
| `validate.go` | 递归父目录符号链接检查 |
| `downloader/http_downloader.go` | SSRF err2 修复、User-Agent |
| `downloader/ssrf.go` | DNS 重绑定防御、localhost 拒绝、127.0.0.0/8 全范围 |