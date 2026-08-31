# 2026-07-31-sproxy-pkgclient-review-experience-round9.md

## 第九轮审查修复统计

| 批次 | 内容 | 问题数 | 提交 |
|------|------|--------|------|
| B1 | client.go — 路径穿越/goroutine泄漏/nil防护/doJSON缺陷 | 10 | 自动合并 |
| B2 | chunked.go — OOM/路径穿越/goroutine泄漏/锁内回调 | 5 | `776ace9` |
| B3 | chain+cloud — 状态不一致/错误包装/nil防护/Success检查 | 12 | `cfc84e8` |
| B4 | version/share/stats/config/archive — doJSON迁移+5文件 | 13 | `3a0ed3f` |
| B5 | 测试修复 — 并发干扰/pipe泄漏/ParseKey/断言增强 | 11 | `c5f3bb9` |
| B6 | 测试修复 — 请求体验证/矛盾断言/mock I/O | 6 | `3165931` |
| B7 | 测试修复 — 边界测试/mock I/O/错误处理/Fuzz不变量 | 12 | `82022a5` |
| B8 | 测试修复 — t.Cleanup统一/字段验证/t.Parallel() | 6 | `95ad627` |
| **合计** | | **约 75** | **8 个提交** |

### 严重度分布

| 等级 | 数量 | 占比 |
|------|------|------|
| Critical | 8 | 11% |
| Important | 25 | 33% |
| Medium | 22 | 29% |
| Suggestion | 20 | 27% |

## 通用问题模式

### 1. 路径穿越防护不完整 — 同一模式重复出现 6 处

**现象**：`Download`、`ChunkedDownload`、`downloadToFile`（archive.go）、`downloadToLocal`（chain_cloud_download.go）、`SaveConfig`（config.go）共 6 处文件写入操作存在路径穿越风险。其中 5 处是"部分分支有防护，但非所有分支"——典型模式是 `outputPath == ""` 时检查 `..`，但非空 `outputPath` 时直接透传。

**教训**：路径穿越防护必须是"统一入口校验"，不能按分支条件判断。条件判断必然遗漏某个分支。

**防御**：提取 `validateOutputPath` 辅助函数，在所有文件写入操作入口处统一调用。

### 2. doJSON 手动实现 — 同一缺陷重复 15 个方法

**现象**：`version.go`/`share.go`/`stats.go`/`config.go` 共 9 个 API 手动构造 HTTP 请求 + 手动 JSON 解析，与 `doJSON` 功能重复。每个手动实现都有"缺少 `ErrNotFound` 包装"（14/15 方法）、"JSON 解码无大小限制"（9 方法）、"错误格式不一致"等问题。

**教训**：手动 JSON 解析的 API 方法必然遗漏 `doJSON` 已经处理的公共逻辑。不应信任"这次我会注意"。

**防御**：所有 JSON API 应统一使用 `doJSON` 封装。`doJSON` 本身需完备：LimitReader + Success 自动检查 + 错误格式统一 + ErrNotFound 包装。

### 3. 修复引入的回归 — version_test.go 中 `"v1"` 未同步

**现象**：`3a0ed3f` 将 `VersionID` 从 `string` 改为 `int64`，`version.go` 中增加了 `strconv.ParseInt` 转换，但 `version_test.go` 中的测试参数仍使用 `"v1"`，导致 `ParseInt("v1")` 失败。

**教训**：导出类型变更（字段类型、签名变更）时，必须同步更新所有测试文件中的匿名结构体、测试参数、mock 数据。`grep -rn "字段名" *_test.go` 是必须执行的步骤。

**防御**：在 Phase 4D 复检模板中已增加"向后兼容性检查"和"导出类型变更同步检查"。

### 4. 修复文件精确性 — 格式化变更污染

**现象**：pre-commit hook 的 `gofmt` 格式化导致 8 个文件产生 `+1523/-1526` 的空白符变更，与修复内容混在一起。如果直接提交，后续 `git blame` 无法区分格式化和真实修复。

**教训**：pre-commit hook 格式化是预期行为，但应通过 `fixup!` + `autosquash` 合并到对应 commit，而非作为独立 commit 提交。

**防御**：修复子代理提交后，如果发现格式化变更，使用 `git commit --fixup=<commit>` 创建 fixup commit，然后 `git rebase --autosquash` 合并。

### 5. 锁内调用回调 — 上传/下载路径不一致

**现象**：`chunked.go` 中 `uploadChunk` 正确地在解锁后调用 `progressFn`，但 `downloadOneChunk` 在锁内调用。同一文件内的两种路径不一致。

**教训**：fix 同类问题时，必须搜索全包确认是否所有同类模式都已修复。`grep -rn "progressFn\|回调"` 在同一包下搜索所有调用点。

**防御**：修复子代理的步骤 3 中已增加"语义等价搜索"要求。

## 代码审查流程改进

### 1. 按文件隔离批次 + 并发修复

**改进**：与之前按功能/严重度分批次不同，本轮改为按文件隔离批次（每个文件只出现在一个批次中），8 个批次可全部并发修复。实际效果：
- 7 个批次同时修复，B4 依赖 B1 的 doJSON 修复
- 无文件冲突，无需担心并发修改同一文件
- 修复时间从串行 ~8 轮缩短到并行 ~2 轮

**结论**：按文件隔离并发修复比按功能批次串行修复效率更高，风险更低。

### 2. 同类问题搜索先行

**改进**：在分析阶段之前，先启动 9 个并行同类问题搜索代理，覆盖所有问题类型。这带来了：
- 路径穿越：发现 6 处（比最初 G1/G2 报告的 2 处多 4 处）
- 手动 doJSON：发现 15 个方法有缺陷（比最初报告的 3 个文件多 12 个）
- 资源泄漏：发现 2 处生产路径泄漏（比最初多 1 处）

**结论**：同类问题搜索是审查流程中性价比最高的步骤——它发现了约 60% 的额外问题。

### 3. 格式化变更必须 fixup 到对应 commit

**改进**：pre-commit hook 的格式化变更不应以独立 commit 提交，而应通过 `git commit --fixup=<commit>` + `git rebase --autosquash` 合并到原始 commit。

**结论**：这个流程应在修复子代理的提交步骤中明确说明。

## 测试最佳实践

### 1. Mock handler I/O 错误处理

本轮发现约 30 处 mock handler 中的 I/O 错误被静默忽略（`os.Create`、`os.ReadFile`、`os.WriteFile`、`json.Decode` 等）。虽然测试环境 I/O 失败概率极低，但 `os.Create` 失败后 `out.Write` 在 nil 上 panic，错误信息是"nil pointer dereference"而非"file create failed"，增加调试难度。

**防御**：mock handler 中 I/O 操作至少应输出错误信息，或使用 `http.Error` 返回 500。

### 2. 测试名称与行为一致

本轮修复了 `TestClient_ChunkedUploadAutoThreshold` → `TestClient_SmallFileUploadWithoutChunking`（名称暗示"自动分块"但实际只测试了"小文件上传"）。名称误导比没有测试更严重——它制造了"已覆盖"的假象。

**防御**：审查测试文件时，强制验证测试名称与断言内容的对应关系。阅读测试名称 → 阅读 mock handler 和断言 → 判断测试通过的真实原因是否与名称声称的行为一致。

### 3. Fuzz 不变量不只检查上界

`FuzzCalcChunkSize` 只检查了 `cs > 0` 和 `cs <= maxChunk`，缺少 `cs >= min(preferred, maxChunk)` 下界不变量。上界不变量只能捕获"太大"的 bug，下界不变量才能捕获"太小"的 bug。

**防御**：Fuzz 不变量应覆盖上界和下界两个方向。

## API 设计经验

### 1. doJSON 统一入口模式

将 `doJSON` 设计为所有 JSON API 的统一入口，并在其中添加：
- `io.LimitReader` 10MB 上限（OOM 防护）
- `successChecker` 接口自动检查 `Success` 字段
- 统一错误格式 `"请求失败 (HTTP %d): %s"`
- `ErrNotFound` 自动包装

这样一劳永逸地消除了所有手动 JSON 实现的缺陷。

### 2. 路径穿越防护统一辅助函数

`validateOutputPath` 辅助函数封装了 `filepath.Clean` + `..` 检查 + 拒绝绝对路径，所有文件写入操作入口处统一调用。避免了"这个分支有检查，那个分支没有"的问题。

### 3. 零值忙等待防御

`CloudDownloadChain` 的 `PollInterval=0` 导致 `time.NewTicker(0)` 忙等待，CPU 100%。防御策略：
- Option 层：`n <= 0` 时设为默认值（5s）
- 恢复层：`fixPollInterval` 辅助函数兜底
- pollAllTasks 层：select 中 ctx.Done() 作为兜底退出

三重防御确保了即使遗漏了 Option 层校验，运行时也不会忙等待。

## Changelog

### 第九轮审查修复统计

| 类型 | 数量 | 示例 |
|------|------|------|
| 路径穿越 | 6 | Download/ChunkedDownload/downloadToFile/downloadToLocal/SaveConfig/List |
| OOM 防护 | 5 | doJSON 入口 + 4 处 json.NewDecoder |
| goroutine 泄漏 | 3 | Upload/buildChunkRequest/sendChunkRequest |
| doJSON 缺陷修复 | 3 | LimitReader + Success 检查 + 错误格式统一 |
| doJSON 迁移 | 9 个 API | version(3) + share(3) + stats(1) + config(2) |
| 协议兼容性 | 2 | StatsResponse 4 字段 + CloudTask Method 字段 |
| 锁内回调 | 2 | downloadOneChunk + uploadChunk |
| nil 防护 | 4 | WithHTTPClient/WithKVStore/NewChainManager/HandleConfigShow |
| 测试质量 | 30+ | mock I/O/Fuzz 不变量/边界测试/断言增强 |
| **合计** | **约 75** | |

### 全九轮审查累计修复统计

| 轮次 | 问题数 | 主要修复内容 |
|------|--------|-------------|
| 第 1-8 轮 | 180+ | 累计 8 轮审查修复 |
| 第 9 轮 | 75 | 8 个批次并发修复，按文件隔离 |
| **累计** | **255+** | **pkg/client 全面质量提升** |

### 文件变更统计

| 文件 | 说明 |
|------|------|
| `pkg/client/client.go` | 路径穿越/goroutine泄漏/nil防护/doJSON缺陷/注释修正 |
| `pkg/client/chunked.go` | OOM/路径穿越/goroutine泄漏/锁内回调/JSON解码限制 |
| `pkg/client/chain.go` | 错误包装/nil panic/WithoutCancel/日志级别/注释 |
| `pkg/client/chain_cloud_download.go` | 状态不一致/路径穿越/零值忙等待 |
| `pkg/client/cloud.go` | Success检查/Method字段 |
| `pkg/client/version.go` | 迁移到doJSON |
| `pkg/client/share.go` | 迁移到doJSON |
| `pkg/client/stats.go` | 迁移到doJSON + 补充4个存储字段 |
| `pkg/client/config.go` | 迁移到doJSON + 路径穿越/nil防护 |
| `pkg/client/archive.go` | 路径穿越/OOM |
| `pkg/plugin/registry.go` | 新增Delete方法 |
| 测试文件 × 12 | mock I/O/断言增强/并发安全/边界测试/t.Cleanup统一 |
| `cmd/sclient/version_test.go` | 回归修复 `"v1"` → `"1"` |