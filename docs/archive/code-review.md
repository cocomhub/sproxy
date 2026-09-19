<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 代码审查经验总结（2026-07 ~ 2026-08）

> 来源：sproxy `pkg/client` / `pkg/server` 多轮（10+ 轮）独立子代理代码审查与修复的沉淀。
> 这些是**跨时间仍然有效的实践教训**，覆盖问题模式识别、审查流程方法论与测试最佳实践。
> 历史审查的逐轮统计与修复清单见 git 历史（已删除的 `docs/superpowers/learnings/*review-experience*.md`）。

## 1. 高频问题模式（写代码 / 审查时逐条对照）

### 并发安全（Critical 里最常见）

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| channel 关闭顺序错误 | 先关 persistCh 后关 stopCh → send on closed panic | 先关 stopCh 阻断新请求，再关 persistCh 排空 |
| goroutine 泄漏 | io.Pipe 写入 goroutine 在客户端断开后不退出 | defer pr.Close() + sync.Once 保护双重关闭 |
| wg.Add 在 go 之后 | 断线重连时 wg.Add 在 goroutine 内部 | wg.Add(1) 统一在 go 之前调用 |
| 锁内 I/O / 锁内回调 | 持锁执行 os.Remove / 锁内调 progressFn | 锁内收集列表，锁外执行 I/O；回调移出锁 |
| re-entrant lock 死锁 | updateConfigHandler → rebuildLogger 重复获取同一 Mutex | 移除内部锁，注释说明调用方必须持锁 |
| 共享池多次归还 | sync.Pool hash 对象 3 个错误路径分别 Put | defer 统一归还 |
| 锁顺序不一致 | executeDownload: mu→dirtyMu，flushDirty: dirtyMu→mu | 统一锁获取顺序 |

### 安全与完整性

| 模式 | 案例 | 修复方式 |
|------|------|---------|
| 路径穿越「部分分支防护」 | outputPath=="" 时检查 `..`，非空时透传 | 统一入口辅助函数（`validateOutputPath` / `containsPathTraversal`），所有写路径调用 |
| 认证 fail-open | cfg==nil 时直接 next(w,r)；空 Bearer token 通过 ConstantTimeCompare | fail-closed（500/401） |
| 空凭据放行 | Enabled=true, Keys=[] 放行所有请求 | Validate 拒绝 + handler 只看 Enabled |
| 符号链接穿越 | joinSafePath 只查字符串前缀 | EvalSymlinks 逐级解析 + os.Lstat |
| Content-Disposition 注入 | 文件名直接拼 HTTP 头 | mime.FormatMediaType 封装 |
| 存储会计损坏 | CancelTask 释放已被进度回调更新过的 TotalSize | ReservedSize 字段与 TotalSize 分离 |
| 子串匹配误报 | strings.Contains(lower, "507") 匹配状态码 | 用语义短语，避免纯数字 |

### 协议 / 类型变更连锁

- 手动 JSON API 必然遗漏公共处理（LimitReader / Success 检查 / ErrNotFound 包装）→ 统一 `doJSON` 入口
- 服务端-客户端 duration 字段用通用格式（`"86400s"`），避免 Go 特有格式
- 导出类型变更（int↔int64、struct 重命名）必须 grep 全仓（含测试匿名结构体）同步
- 修复引入的回归约占严重问题的 1/3 ——「修复后新问题检测」是必要环节

### 死测试与假覆盖

- 「不 panic 就通过」、`t.Logf` 替代 `t.Errorf`、`_ = err` 消音、测试名与实际行为颠倒 —— 比缺测试更危险
- 判断测试是否真覆盖：阅读测试名 → 读 mock 与断言 → 确认通过原因与名称声称一致
- Fuzz 不变量要覆盖上界和下界两个方向

## 2. 审查流程方法论（验证有效的做法）

1. **独立子代理 + 文件即接口**：每个审查代理从零开始、中间产物写文件，避免上下文污染与确认偏误
2. **按文件隔离批次**：每个文件只出现在一个批次，批次间可并发修复（按功能/严重度分组必然文件冲突）
3. **H1 同类问题搜索**：按已发现类型全仓 grep，能额外发现 30–60% 的同类问题（路径穿越 6 处、手动 doJSON 15 个方法）
4. **跨代理交叉验证（⨯N）优先级上调**：多个独立代理发现同一问题时可信度显著提高（信令身份伪造被 3 组发现）
5. **4D 复检查调用链接线点**：不只是「确认修好」，要查调用链上未同步的接线点（B2 只给 relay.go 接入、B3 漏掉 mesh 客户端）
6. **独立复检发现回归**：每批修复后由全新上下文代理复检，能 catch 端到端才暴露的协议兼容问题
7. **最终审查代理运行全量测试**：`go test ./pkg/... ./cmd/...`（目标包全绿 ≠ 关联包全绿）
8. **并行修复纪律**：共享工作区禁止 `git reset/checkout/stash`（会吞掉他人变更）；修复代理提交后必须 `git diff --stat HEAD~1` 核对文件列表与 commit message 一致（曾发生「声称修 9 个文件实际只提交 5 个」）
9. **分析阶段先验证问题真实性**：误报排除（已有防护措施、函数名体现设计意图、Go 1.26 已修复的 os.Rename 语义）——「先分析后执行」
10. **方案对比表格化 + 追溯调用方**：每个问题 2–3 种方案表格对比；判断「重复」是否该消除前，先回答职责是否相同、被不同层级使用吗、调用方是否更简洁（chainOptions 是桥接层不是冗余）

## 3. 测试最佳实践

- **t.Context() 替代 context.Background()**（Go 1.24+，测试结束自动取消）
- **t.Cleanup 替代 defer**（panic 安全）；纯逻辑测试加 `t.Parallel()`
- **异步等待用轮询，不用固定 time.Sleep**（CI 高负载下 flaky）
- **goroutine 中禁止直接 t.Errorf**：errCh + wg.Wait() + close + for range 收集
- **并发测试验证最终状态，不假定 goroutine 顺序**
- **负 TTL 实现立即过期**，替代等待
- **响应体读取统一 io.LimitReader 限大小**（OOM 防护）
- **mock handler 也要处理 I/O 错误**（os.Create 失败后 nil panic 误导排查）
- **io.Writer 注入替代 CaptureStdout 全局替换**（t.Parallel 安全）
- **Windows 跨平台**：mtime 精度有限（需要时加等待）、`core.autocrlf input`、make fmt 统一格式
- **测试网络客户端禁止共享 http.DefaultClient/DefaultTransport**（并行用例 CloseIdleConnections 会打断在途连接）

## 4. API 设计经验

- **Option 模式优于平铺参数**；导出字段不如 Option 函数（保护封装）
- **`Validate()` 只读校验，`SetDefaults()`/`Normalize()` 负责设默认值**（Validate 修改接收者是反模式）
- **错误吞没不如记录**：`fmt.Errorf("...: %w", err)` 包装；哨兵错误 + errors.Is 优于子串匹配
- **默认严格模式优于静默回退**：WithTransportFallback() 让调用方显式选择降级
- **内部字段 + 自定义 JSON 序列化保护 API 契约**（ChainResult.Extra 内部化模式）
- **结果类型替代多返回值**（tryResumeResult 替代 (result, error, bool)）
- **零值忙等待防御**：PollInterval=0 → time.NewTicker(0) CPU 100%；Option 层 + 恢复层 + select 兜底三重防御
- **原子写入模式**：`.tmp` + os.Rename；失败后清理预分配文件（先 Close 再 Remove，Windows 兼容）
- **ctx 级联取消陷阱**：WithCancel(ctx) 会随父取消；saveState 等关键持久化用 context.Background() 分离
- **json:"-" 字段序列化陷阱**：状态文件持久化字段需显式 json 标记，运行时句柄由 Resume 手动 setClient
- **close 幂等性**：closeOnce sync.Once 防多次 close panic

## 5. 服务端特有教训（pkg/server 审查）

- **MkdirAll 失败后不 return**（继续执行必然失败、错误误导）→ 立即返回 + 明确响应
- **SSRF**：DNS 解析 10s 超时；scheme 检查用 `req.URL.Scheme` 而非字符串前缀；`err`/`err2` 变量名作用域重叠极易用错
- **map 条目永不清除**（ChunkFileLocker、uploadingFiles）→ DeleteSession 清理 / 定时清理循环
- **同一文件并发上传竞态覆盖** → LoadOrStore 防护
- **TOCTOU「先检查后操作」** → 基于 fd 操作 / 双重检查 / 保留检查
- **限流器边界**：limit=0 全拒绝（`len(timestamps) >= 0` 恒真）、window=0 无限放行 → 显式边界校正
- **审计盲区**：认证失败不记日志 → 拒绝点加 slog.Warn（remote/path/method）
- **破坏性变更需显式标注**：批量删除 checksum 拒绝、无效 subdir 400、TTL 无效 400、CORS 拒绝 403、SaveConfig 0600

## 6. 跨模块依赖约束（本项目特有）

- **pkg/client 的 e2e_test import pkg/server** ⇒ pkg/server 生产代码**不能** import pkg/client（测试编译环）
  - 解法：接口 + 独立实现包（如 `Executor` 接口由 `pkg/syncexec` 实现、cmd/sproxy 装配注入）
- **xfer/internal/tcp 的 internal 可见性**：外部包无法 blank import，需 `pkg/tunnel/xfer/builtin` 桥
- **核心 go.mod 零三方新增**（仅 yaml.v3 + x/sys + x/crypto + x/net）；子 module 依赖经 require + replace 接线，`go mod tidy` 会因跨 module 测试依赖失败 → 手动加依赖
