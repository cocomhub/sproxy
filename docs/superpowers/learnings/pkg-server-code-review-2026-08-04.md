# pkg/server 代码审查修复经验总结

> 审查日期：2026-08-04 ~ 2026-08-05
> 审查范围：pkg/server/ 全部 41 个非测试文件
> 目标：功能正确性、可用性、可靠性、完整性、可维护性

## 修复统计

| 指标 | 数据 |
|------|------|
| 原始问题 | 151 个（12 Critical, 49 Important, 91 Suggestion） |
| 同类搜索新增 | 10 个（来自 H1 硬关卡搜索） |
| 复检新增 | 4 个（Phase 4D 复检发现） |
| 技术债处理 | 3 个（parseDuration 死代码、storage_config 冗余、rmdir force） |
| **总修复** | **168 个** |
| Fix commit | 24 个 |
| 变更文件 | 46 个 |
| 变更行数 | +797 / -443 |
| 测试 | ✅ `go test -count=1 -race` 全部通过 |

## 通用问题模式

### 1. 并发安全（出现频率最高，8 个 Critical 中 5 个是并发问题）

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| **channel 关闭顺序错误** | `UploadStore.Stop()` 先关 persistCh 后关 stopCh → send on closed panic | 先关 stopCh 阻断新请求，再关 persistCh 排空 |
| **goroutine 泄漏** | `StorageManager.periodicScan` panic recover 后重启，Stop() 无等待机制 | 加 `sync.WaitGroup` 追踪 + panic 恢复时检查 stopCh |
| **wg.Add(1) 在 go 之后** | `cloud_download.go` 断线重连时 wg.Add 在 goroutine 内部 | wg.Add(1) 统一在 go 之前调用 |
| **后台 goroutine 未跟踪 wg** | `cleanupExpired`/`flushLoop` 启动时未调用 wg.Add | 启动时 wg.Add(1) + defer wg.Done() |
| **锁内 I/O 阻塞** | `cleanupExpiredOnce` 持有锁期间执行 `os.Remove` | 锁内收集 ID 列表，释放锁后执行 I/O |
| **re-entrant lock 死锁** | `updateConfigHandler` 调用 `rebuildLogger`，后者重复获取同一 Mutex | 移除内部锁，注释说明调用方必须持有锁 |

### 2. 安全漏洞

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| **认证绕过** | 空 APIKeys 时 `Enabled=true, Keys=[]` 放行所有请求 | 双重防护：Validate 层拒绝 + handler 只看 Enabled |
| **SSRF 防护** | DNS 解析无超时、scheme 检查用字符串前缀 | 加 10s timeout context、改用 `req.URL.Scheme` |
| **认证审计盲区** | 认证失败时不记录任何日志 | 5 个拒绝点加 slog.Warn 记录 remote/path/method |

### 3. 资源泄漏 / OOM

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| **map 条目永不清除** | `ChunkFileLocker.fileLocks` 为每个 uploadID 创建 RWMutex 但永不删除 | DeleteSession 时清理对应条目 |
| **sync.Map 无限增长** | `uploadingFiles` 条目无过期清理机制 | 10 分钟定时清理循环 + 配合 uploadingStop channel |
| **并发上传无防护** | 同一文件路径的并发上传可能竞态覆盖 | `uploadingFiles.LoadOrStore` 防护 |

### 4. 死代码 / 无效内容

| 模式 | 案例 | 处理方式 |
|------|------|---------|
| **遗漏实现** | `muxMetrics` 字段声明但未赋值 | 加 TODO 注释标注未来接入路径 |
| **无效内容** | `MaxChunkSize` 服务端 Config 含客户端字段 | 删除 |
| **无效内容** | `gzipResponseWriter.statusCode` 赋值永不读取 | 删除字段 |
| **无效内容** | `parseDuration` TTL 统一为 time.Duration 后不再需要 | 删除 |
| **设计冗余** | `storageConfigHandler` 与 `updateConfigHandler` 功能重叠 | 删除路由和文件 |

### 5. API 设计缺陷

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| **副作用违反命名约定** | `Validate()` 修改接收者状态 | 拆分为 `SetDefaults()` + `Validate()` |
| **语义不匹配** | `LoadConfig` 文件不存在时自动写入 | 新增 CLI flag `--write-default-config`，默认 false |
| **响应格式不统一** | 限流器 429 返回 text/plain 而非 JSON | 改为 `sendJSONResponse` |
| **响应格式不统一** | hub_handler 未启用时返回 text/plain | 改为 JSON |

## 流程改进建议

### 本次流程中有效的方法

1. **按文件隔离分组批次** — 确保每个文件只出现在一个批次中，批次间可并行修复，无文件冲突
2. **H1 同类问题搜索** — 多发现约 10 个额外问题（错误忽略、wg 跟踪遗漏等），值得保留
3. **Phase 4D 复检** — 发现 4 个新问题（re-entrant lock、双重 close panic、测试死测试、防御深度削弱），避免了修复引入新问题
4. **独立子代理** — 每个阶段从零开始审查，避免了上下文污染

### 本次流程中遇到的问题

1. **并行修复代理的提交冲突** — 多个代理修改同一文件时，后完成的覆盖前一个的修改。解决方案：严格按文件隔离分组
2. **pre-commit hook 失败** — 部分代理提交后 pre-commit 格式化失败，工作区残留未提交变更。解决方案：提交前检查 `git status --short`
3. **stash 操作丢失变更** — 自动 stash 在并行修复场景下极易导致变更丢失。解决方案：禁止自动 stash，询问用户处理

## 中间产物清理

以下目录已清理：
- `.superpowers/sdd/pkg-server-review/` — 7 组审查报告
- `.superpowers/sdd/similar-search/` — 7 个同类搜索报告
- `.superpowers/sdd/batch*-analysis.md` — 13 个分析报告
- `.superpowers/sdd/batch*-review.md` — 6 个复检报告
- `.superpowers/sdd/consolidated-summary.md` — 汇总报告
- `.superpowers/sdd/final-review.md` — 最终审查报告