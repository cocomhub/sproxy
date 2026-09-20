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
- **cmd/sproxy 与 cmd/sclient 各有一份 ViperProvider**（各 65 行，差异仅 env prefix）：多 module 工作区中 `internal/` 对同级 module 不可见是必然代价；若未来移除 viper 依赖可同时删除（`LRN-20260618-GC10`）

## 7. 工具链 / linter / CI 配置经验（源自早期学习记录）

> 以下条目来自 2026-06~2026-07 的工具链踩坑沉淀（原 `.learnings/LEARNINGS.md` 与 `build/` 记录），
> 多数已被门禁固化，保留作为排查参考。

### golangci-lint 配置

- **v2 presets 只支持有限值**（`comments`/`common-false-positives`/`legacy`/`std-error-handling`），传 `stutter`/`var-naming` 直接报错退出；豁免请用 `exclusions.rules` 按 path 匹配（`LRN-20260618-GC1`）
- **errcheck 对类型断言**（如 `chunkPool.Get().(*[]byte)`）用 `_ =` 无法修复（不是函数调用），必须行尾 `//nolint:errcheck`（`LRN-20260618-GC2`）
- **gosec 确认安全的规则**（G101/G115/G117/G306/G404/G703/G705）优先在 `.golangci.yml` 的 `gosec.excludes` 统一豁免，而非逐处 `//nolint`（`LRN-20260618-GC3`）
- **thelper 要求 testing.TB 参数名为 `tb`**（`b` 会被拦）——benchServer 等辅助函数需全局替换（`LRN-20260618-GC4`）
- **govet shadow 三种修复模式**：简单 `if err :=` → `= `；有新同名变量 → 先 `var` 声明再赋值；goroutine 闭包捕获外层 err → 改用独立变量名（`part, wErr :=`）（`LRN-20260618-GC6`）
- **独立 go.mod 的子 module lint 需显式指定路径**：`golangci-lint run ./...` 只覆盖主 module；ext/ws、ext/quic 需单独跑（`LRN-20260619-BP77`）

### Makefile / CI

- **`| tee` 吞掉 go test 退出码**：管道退出码取自 tee 恒 0 ⇒ 改「写临时文件 → 读回 → exit $rc」纯 POSIX 写法（dash 无 pipefail）（`LRN-20260618-GC18`，已固化进 `make bench`）
- **`go test -bench` 必须加 `-run=^$`**：否则通配符 `.` 同时匹配 TestXxx，flake 测试会拖垮 benchmark job（`LRN-20260618-GC20`）
- **CI benchmark 配置应低于本地**：2 核 runner 比本地慢约 10 倍，CI 用 `count=3, benchtime=500ms`（`LRN-20260619-GC25`）
- **Linux SO_REUSEADDR 使「先占端口再 ListenAndServe」测试失效**：Linux 允许多 listener 绑定同地址 ⇒ 改为 goroutine + signal + 超时模式（`LRN-20260618-GC8`）

### 测试经验补充

- **测试 TTL 立即过期用负值**（`-time.Nanosecond`）：正纳秒 TTL 在 Windows 时钟粒度下不可靠（`LRN-20260619-GC24`）
- **插件注册表测试必须独立实例**：全局 `xfer.TransportRegistry` 被污染会 panic；用 `plugin.New[*xfer.Transport]("test", builtin)` + 必要时 `Clear()` + `t.Cleanup`（`LRN-20260618-GC17`/`LRN-20260619-BP56`）
- **网络测试优先用产品代码标准流程**：`Listen → Dial → Accept` 三件套，而非手动 goroutine 模拟（`LRN-20260618-GC21`）
- **captureXxx 辅助函数必须 save/restore 全部包级全局变量**（含 `cfgFile`/`cfgProvider`），新增全局变量时同步更新（`LRN-20260618-GC14`）
- **Windows 并发 Rename**：固定 `.tmp` 路径多 goroutine 冲突 ⇒ `os.CreateTemp` 唯一名 + 指数退避 `atomicRename` 重试（`LRN-20260623-BP95`）
- **Windows 编码**：解析外部工具 JSON 显式 `encoding='utf-8'`（默认 gbk 会 UnicodeDecodeError）；不用 `python3 -c "..."` 传多行脚本（git-bash trap），写 `.py` 文件再执行（`LRN-20260619-BP71/72`）
- **map→struct 测试桥梁**：`yaml.Marshal(map)` + `yaml.Unmarshal(&cfg)` 替代 viper，纯标准库 + yaml.v3（`LRN-20260618-GC12`）

### 子代理开发纪律

- **子代理提交后主流程必须跑全局 lint**：常见盲区为 errcheck（`CloseWithError`/`os.Chtimes`）、gofmt 表字段未对齐、未使用 import、shadow（`LRN-20260619-BP57`/`LRN-20260619-BP75`/`LRN-20260620-BP85`）
- **子代理常遗留临时脚本**（`.claude/analyze_sonar.py` 等）：提交前 `git add -A -- ':!.claude/'` 排除或手动清理（`LRN-20260620-BP86`）
- **并行代理改同一配置（`.golangci.yml`）会冲突**：多代理都会改的文件应在合并后统一修改（`LRN-20260618-GC7`）
- **S1192 常量提取后必须全文件 grep**：SonarQube 只报第一次出现位置，其余出现需 grep 补全（`LRN-20260622-BP88`/`LRN-20260622-BP91`）
- **跨 module 常量提取先规划位置**：常量只在本 module 内可见，独立 go.mod 不能引用根 module 常量（`LRN-20260622-BP89`）
- **纯重构不改测试文件，但错误消息必须兼容**：提取辅助函数时错误消息字面量要精确匹配原版（`LRN-20260620-BP82`）

### 重构方法论

- **先提取公共辅助函数再替换**（`writeFileAtomically`/`resolveFilePath` 跨 handler 复用）：比逐个 handler 提取更有效（`LRN-20260620-BP78`）
- **巨型 switch → map 分发表**：`map[FrameType]frameHandler` 替代 ~93 行 switch，加帧类型只加条目（`LRN-20260620-BP79`）
- **结构体包装 + 薄委托**：`StreamEncryptor`/`ChunkedUploader` 把状态与方法封装，原函数缩为 3 行委托（`LRN-20260620-BP80`/`LRN-20260620-BP81`）
- **CLI 顺序阶段提取**：`runServer` 按阶段边界拆独立函数，主函数只留编排（`LRN-20260620-BP83`）

### 并发安全（mux / goroutine 踩坑）

> 2026-06 mux 重构（commit `ec07f1a`）集中踩坑：同一批修复（CR1-CR7 / BP1-BP8 / GC22-GC23）沉淀以下模式，均已在代码落地，保留作审查对照。

- **acceptCh 满时静默丢弃流 ⇒ 对端 Read 永久阻塞**：同步 Open + 异步 Accept 的 mux 必须显式通知拒绝——新增 `FrameReject` 拒绝帧 + `ErrStreamRejected` 哨兵（`LRN-20260616-CR1`）
- **持有 m.mu 时阻塞在通道发送 ⇒ 与 Close() 死锁**：正确模式是「锁内查找 → 立即解锁 → 锁外做通道操作」；dataCh 的 close 与 send 并发是 data race，须用独立 `closeMu` 保护（`LRN-20260616-CR2`/`LRN-20260616-BP5`/`LRN-20260616-BP8`）
- **goroutine 内用 context.Background() 做 I/O 会永久阻塞**：长期运行 goroutine 必须用可取消的派生 context（`m.ctx`）；`m.done` + `context.WithCancel` 是标准关闭传播模式；重试循环必须同时监听 `ctx.Done()`（`LRN-20260616-CR3`/`LRN-20260619-GC23`）
- **清理先于确认发送 ⇒ 数据丢失**：先 `conn.Send(FrameClose)` 成功后再 `removeStream`（`LRN-20260616-CR4`）
- **计数器必须配对**：`activeStreams` 的 +1 在 accept-success 分支、拒绝路径直接 -1 ⇒ 下溢；拒绝路径应手动 delete + reject，不调 removeStream（`LRN-20260616-CR5`）
- **所有 I/O goroutine 的持久性错误必须触发 mux 关闭**：pingLoop Send 失败曾只 return 不关 mux ⇒ 失去心跳监控；readLoop 应区分临时错误（指数退避重试）与致命错误（`LRN-20260616-CR6`/`LRN-20260616-BP2`）
- **公共结构体暴露内部通道 ⇒ 外部直接写入无保护**：`Stream` 结构体 → 接口 + 私有实现，内部通道操作封装为方法；调用方需从 `*Stream` 迁移到 `Stream` 接口（`LRN-20260616-CR7`/`LRN-20260616-BP3`）
- **空写入 `Write([]byte{})` 被误判为结束标记**：`closeMarker` 与用户空数据无法区分 ⇒ 公共方法先验证输入边界（`LRN-20260616-BP1`）
- **FrameReject 经 writeCh 非阻塞发送可能被丢弃**：writeCh 满时拒绝帧静默丢 ⇒ dialer 永远不知被拒；当前靠 256 容量 + 30s context + 显式 Close 兜底，理想方案是独立高优先级控制帧通道（`LRN-20260616-BP6`，已知设计不足）
- **goroutine 写 http.ResponseWriter 必须监听 `r.Context().Done()`**：客户端断连后 `w.Write` 永久阻塞不返回错误 ⇒ goroutine 泄漏（`LRN-20260619-GC22`）
- **同步 Close() 幂等**：`close(ch)` 多次调用 panic ⇒ `sync.Once` 保护（`LRN-20260614-BP6`）

### 错误处理与安全

- **`_ = err` 是危险信号**：所有可能失败的函数必须处理错误（log + Close + return），不能静默吞掉（`LRN-20260616-BP4`）
- **哨兵错误用 errors.Is**：`fmt.Errorf("%w")` 包装后 `==` 失效（`LRN-20260616-BP7`）
- **typed-nil 陷阱**：`*ViperProvider(nil)` 赋给接口后 `!= nil` 为真 ⇒ 调用方法时 panic；接口字段声明为接口类型 + 守卫排除 typed nil（`LRN-20260618-GC9`）
- **路径注入防护**：用户输入拼接文件路径必须经 `joinSafePath`（filepath.Abs + HasPrefix 校验）或 `h.safePath` 二层模式（配置访问在方法层、参数传入在底层函数）；内部私有 helper（`saveVersion`/`cleanupOldVersions`）也要用——「当前调用者都校验过」不是放弃防御深度的理由（`LRN-20260619-BP61`/`LRN-20260619-BP70`/`LRN-20260619-BP73`）
- **`--server` flag 应同时覆盖连接模式**：`buildFileClient` 曾按 cfg.TunnelKey 一律加 WithTunnel ⇒ 显式 `--server` 时跳过隧道（`LRN-20260618-GC15`）
- **默认输出路径禁止写入 CWD**：`path.Base("/")` 回退 `index.html` 直接落 CWD ⇒ 用临时目录或工作目录 + TOCTOU 防护（`LRN-20260618-GC16`）

### 测试反模式（无效测试清单）

- **「不 panic 就通过」/ `t.Logf` 替代 `t.Errorf` / `_ = err` 吞错 / 测试名与实际行为颠倒** —— 比缺测试更危险，review 时逐条对照（`LRN-20260614-BP7`）
- **请求发到错误路由**：`TestBatchRenameHandler` 发 `/batch-rename`（真实是 `/api/batch/rename`）⇒ handler 覆盖率恒 0 且测试通过——无效占位测试必须删除或修复（`LRN-20260619-BP59`）
- **测试必须隔离本地配置**：`--server` 只覆盖 server_url 不阻止加载 `~/.sclient.yaml` ⇒ 测试用 `--config` 指向临时配置文件或显式清环境变量（`LRN-20260615-INFO3`/`LRN-20260618-GC19`）
- **-race 下超时需留余量**：goroutine 密集测试（mux/p2p）在 -race 下显著变慢，context timeout 留 3 倍余量（`LRN-20260615-INFO2`）
- **覆盖率口径**：`go test -cover ./...` 含 test/tools 稀释 total ⇒ 排除非核心包；grep `0.0%` 会误匹配 `80.0%`，用 `[[:space:]]0\.0%$`（`LRN-20260615-INFO4`）
- **gofmt 批量转换产生 diff 噪声**：修改文件前先 gofmt 目标文件，避免 400+ 行纯缩进 diff；pre-commit 拦截未格式化新文件 ⇒ 新文件写完立即 gofmt（`LRN-20260614-BP9`/`LRN-20260618-GC13`）
- **mock 子包结构**：放 `pkg/testutil/` 下 + `package xxx` / `package xxx_test` 双结构（`LRN-20260619-BP60`）
- **复杂 fixture 不适用 synctest 气泡**：真实 socket/HTTP 阻塞不算 durably blocked（`docs/testing/virtual-time-conversions.md` 台账）
- **Windows 回环绑定铁律**：测试必须绑定 `127.0.0.1`（`0.0.0.0`/`localhost` 触发防火墙弹窗 CI 卡死），`httptest.NewServer` 默认即回环；已由 `make check-loopback` 自动化校验（`LRN-20260615-BP1`）
- **xfertest 套件必须用于传输插件**：`pkg/tunnel/xfer/xfertest/`（非 internal/，外部 module 可 import）统一验证 ws/quic/tcp，新增 transport 禁止自写重复测试（`LRN-20260615-BP3`）
- **`go test -cover` 数据解析**：total 行字段位置不固定（tab 分隔），用 `fields[len(fields)-1]` 而非固定索引；per-package 概览需 Makefile 显式追加（`LRN-20260615-INFO1`）
- **mock 与真实实现锁定语义要一致**：`MockUploadStore` 空实现与真实 RWMutex 行为不一致 ⇒ 抽取 `ChunkFileLocker` 导出类型让 mock 委托，而非 mock 自写空版本（`LRN-20260619-BP62`）
- **测试重复 boilerplate 抽取辅助函数**：`t.Helper()` 标注的 `doBatchRename()`/`newMuxPair()` 可减 ~110 行/文件，消除 SonarCloud 重复告警（`LRN-20260619-BP63`）
- **接口避免未导出方法**：`UploadStoreIface` 含 `lockChunkIO` 未导出方法 ⇒ 外部包 `mockserver` 无法实现（死代码）；导出 `LockChunkIO` + `ChunkFileLocker` 委托（`LRN-20260619-BP64`）

### Cobra / 配置加载

- **viper.New() 必须显式 ReadInConfig()**：`viper.New()` 干净实例不会自动加载配置，且要处理 `ConfigFileNotFoundError` 放行纯 flag/env 模式（`LRN-20260614-BP1`/`LRN-20260614-001`）
- **Run → RunE 迁移**：`Run` + `os.Exit(1)` 会杀死测试进程 ⇒ 改 `RunE` + `return error`（`LRN-20260614-BP2`）
- **signal goroutine 泄漏**：`for sig := range signalChan` 在 ListenAndServe 失败时泄漏 ⇒ `stopSigCh` + select 或 `signal.NotifyContext`（`LRN-20260614-BP3`）
- **error 必须返回不可吞**：所有可能失败的操作返回 `(T, error)`；`os.IsNotExist` 返回空值不是错误、其余向上传播（`LRN-20260614-BP4`）
- **PersistentPreRunE 初始化不可假设**：测试常直接调 `runServer(cmd, nil)` 跳过 pre-run ⇒ `RunE` 必须对包级 `cfgProvider` 做 nil fallback 初始化（`LRN-20260618-GC11`）
- **提交前 go fix + gofmt + addlicense**：跳过会留 10 文件 stdlib 现代化残留（详见 `docs/archive/gofix-before-pr.md`）（`LRN-20260615-BP2`）

### Go module / worktree

- **go.work 管理多 module**：cmd 独立 go.mod 用 `replace` 指向根 module；`internal/` 对同级 module 不可见（`LRN-20260614-BP5`）
- **合并 worktree 先处理本地变更**：octopus merge 会被未提交改动（含 `.githooks/pre-commit` 权限位 755→644）卡住 ⇒ 先提交/stash 再 merge（`LRN-20260618-GC5`）
- **Makefile 修改用 Edit tool 不用 sed 多行替换**：`{}` 嵌套/反斜杠续行/`$$` 转义极易写坏且静默不执行（`LRN-20260615-MK1`）
- **互不依赖任务并行不用 worktree**：worktree 创建 200-500ms + 磁盘占用；仅分支隔离必需时用（`LRN-20260615-BP4`）
- **手动编辑深层嵌套测试文件极易出错**：连续 2 次 Edit 出错就放弃，用 `git checkout` 回滚；已有 lint 告警（非本次引入）不在改 PR 中修（`LRN-20260620-BP84`）

### SonarQube / 静态分析专项

- **SonarQube 6 PR 修复工程总纲**：S2083 路径注入（safePath 二层）→ S5144 SSRF + TLS → S3776 认知复杂度（公共辅助/分发表/struct 包装/顺序阶段）→ S1192 常量提取，共 33 文件 ~121 issues；安全类先提取通用防御函数全量替换，复杂度重构保持「零测试修改」（`LRN-20260620-BP87`）
- **S1192 常量提取必须全文件 grep**：SonarQube 只报第一次出现位置，其余遗漏靠 grep 补（`LRN-20260622-BP88`/`LRN-20260622-BP91`）；规格审查在 subagent 流程中捕获真实遗漏，两阶段审查不是形式主义（`LRN-20260619-BP74`）
- **S8242 不适用内部 lifecycle context**：request-scoped context 才该移除字段改传参；内部 `context.WithCancel` 自建的 lifecycle context 保留字段、Close 时 cancel 即可（`LRN-20260622-BP92`）
- **S5144 校验函数要列出覆盖场景确认**：`validateRelayPath` 逐项（空 path/`..`/scheme 注入/host 注入）验证后再关闭，Target 与 Path 两段独立校验无绕过（`LRN-20260622-BP93`）
- **golangci-lint 子 module 覆盖**：quic/ws 独立 go.mod 需显式指定路径跑 lint（`LRN-20260619-BP77`）；`qconn.CloseWithError` errcheck 易漏（`LRN-20260619-BP76`）
- **Windows Python 编码双重陷阱**：`open()` 默认 gbk 解 UTF-8 JSON 报 UnicodeDecodeError ⇒ 显式 `encoding='utf-8'`；`python3 -c` 传多行脚本被 git-bash `|| goto :error` 打断 ⇒ 写 `.py` 文件执行（`LRN-20260619-BP72`/`LRN-20260622-BP90`）

### 已解决但值得复用的边界

- **chunkSize=0 无限循环**：`io.ReadFull(r, buf)` 对 `len(buf)==0` 立即返回 nil ⇒ for 循环死转；生产不会传入（DefaultChunkSize 64KB），留作边界认知（`LRN-20260614-BP8`）
- **server 包覆盖率平台期**：74% 后核心 handler 已覆盖（stat 85%/rename 79%），剩余 chunked/version/share/hub 需专项测试文件而非 handler 边界测试（`LRN-20260619-BP58`）
- **纯过程记录不保留**：如「某子代理任务成功完成」（godre+小修）类无方法论价值的条目（`LRN-20260622-BP94`），不写入归档——git 历史可回溯
