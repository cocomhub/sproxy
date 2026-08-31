# 阶段 4·文件同步 / 复制（工作项 2）学习记录

> 日期：2026-08-31
> 范围：PR #129–#133（A pkg/sync → B HTTPTransport → C SyncManager → D sclient sync CLI → E Web UI）
> 状态：全部合入 master（f472e8d）
> 设计：`docs/superpowers/specs/2026-08-30-sproxy-fullmesh-stage4-filesync.md`（v4）

## 1. 交付总览

| # | 子任务 | PR | 交付 | 关键决策 |
|---|--------|----|------|----------|
| A | pkg/sync 同步引擎核心 | #129 | FS 抽象/LocalFS/差异/冲突/并发编排 | 纯核心包，仅依赖核心 go.mod；LocalFS confine 防 symlink 逃逸 |
| B | HTTPTransport 传输层 | #130 | FS 的远程实现 + deadline-aware | FileClient OpenDownload/MakeDir 扩展；deadlineConn 活跃读写超时 |
| C | 服务端 SyncManager | #131 | 任务生命周期 + /api/sync/tasks | Executor 接口打破 pkg/server→pkg/sync 测试环；HTTP 直连远程 |
| D | sclient sync CLI | #132 | push/pull + 轮询 + JSON | cmd 薄；--json 退出码修复 |
| E | Web sync_task 频道 | #133 | 传输页 sync 频道 | sc.sync 领域 API + 轮询 + 表单 |

## 2. 关键架构决策（含偏离设计）

1. **模块边界（C，打破测试环）**：`pkg/sync.HTTPTransport` import `pkg/client`，而 `pkg/client` 的 e2e_test import `pkg/server` → 若 `pkg/server` 直接 import `pkg/sync` 会形成测试环。方案：`syncmgr` 定义 `Executor` 接口 + `QuotaStore` 接口，真实实现在独立包 `pkg/syncexec`，由 `cmd/sproxy` 装配注入。**pkg/server 不传递依赖 pkg/client。**
2. **远程访问 HTTP 直连（C，偏离设计 AD-1/AD-5）**：服务端（cmd/sproxy）不承担 mesh 客户端角色，第一版 SyncManager 用 HTTP 直连远程 sproxy（`sync_remotes` URL + SproxySig），fail-closed（未配置凭据拒绝）。mesh 通道为后续增强（`HTTPTransportConfig.Dial` 注入点不变）。设计文档 v4 已更新。
3. **同步模型**：单向 + 文件级增量（checksum 相同跳过）+ 递归/过滤器 + 冲突策略（skip 默认/overwrite/lww/conflict_rename）+ 空文件走轻量 Upload（分块管线拒绝 total_size<=0）+ 空目录默认跳过/符号链接默认跳过。
4. **并发模型**：HTTPTransport 单连接串行分块 + 文件级并发（MaxConnsPerHost=1，避免每并发一条 mesh 流）。
5. **配额**：pull 本地落盘预留 1GiB 占位 + 完成按 BytesDone 对账；恢复任务 Restored 标志不双预留（磁盘扫描已记账）。

## 3. 审查发现与修复（逐子任务）

### A（pkg/sync）：0 Critical + 4 Important + 11 Minor + 6 参考级 → 全修
- I-1 include 目录剪枝漏文件 → MatchFiltersDir（目录仅受 exclude 剪枝）+ 无分隔符 pattern 匹配 basename（rsync 风格）
- I-2 文件覆盖目录残留 .sync-tmp → 拒绝覆盖目录
- I-3 LocalFS 不检查 ctx → copyWithCtx + 全方法 ctx.Err() 快速失败
- I-4 类型冲突误判 skipped → ComputeDiff 显式 Decide

### B（HTTPTransport）：首轮 1 Critical + 3 Important + 4 Minor + 2 参考级；二轮 1 Important + 4 Minor + Info → 全修
- C-1 ListDir 分页拉全（>1000 条目静默漏同步）
- I-1/I-2 写路径无超时 / deadlineConn 惰性 → **活跃读写超时**（Go 1.26 http.Transport HTTP/1.1 从不调 SetDeadline，写路径对端停读靠活跃写超时兜底）
- I-3 Close 阻塞 → forceClose 探测 Abort()（MuxStreamConn.Close 经 writeCh 可能阻塞）
- 二轮 Important：deadlineConn 单一共享 timer 跨方向串扰 → 拆 rdTimer/wdTimer

### C（SyncManager）：0 Critical + 3 Important + 8 Minor + 测试参考级 → 全修
- I-1 CreateTask 去重 TOCTOU → 写锁内去重+预留+插入
- I-2 恢复任务配额二次预留 → Restored 标志
- I-3 validateSyncPath 拒绝 .__ 内部目录（防 push 外泄/ pull 状态篡改）

### D（sclient sync CLI）：0 Critical + 1 Important + 5 Minor + 4 参考级 → 全修
- I-1 --json 模式 failed/cancelled 退出码错误为 0 → 状态错误先算好，输出 JSON 后仍返回

### E（Web UI）：0 Critical + 1 Important + 5 Minor + 5 参考级 → 全修
- I-1 refreshSyncTasks 区分 400 未配置（静默）与真错误（保留 stale + toast）

## 4. 安全闭环（自动安全审查 MEDIUM）

- **IDOR / 任务无 owner**（C）：书面确认——同步任务是 server-global（与 CloudTask 一致，所有认证操作者受信任），task.go 注释说明；未来多租户需加 owner 字段 + 过滤。
- **Path Traversal (Symlink Bypass via FollowSymlinks)**（C）：LocalFS.confine()（EvalSymlinks 逐级解析 + 前缀校验）覆盖全部 8 个文件操作，follow_symlinks=true 时枚举层 Stat 对逃逸 symlink 返回 error → 引擎跳过；补 TestEngineSync_FollowSymlinks_NoEscape。
- **Plaintext Transport**（C）：config.Validate 拒绝 sync_remotes 明文 http 非 loopback（AK/SK 明文上线，对齐联邦 TLS 边界）。

## 5. 验证与质量

- 每子任务：TDD（先失败测试）+ `-race -count=1` + `golangci-lint` 0 issues（主 + 全部子 go.mod）+ `make build-all` + `make test-all` + `make check-loopback` + gofmt/vet。
- 前端：`make web-test`（9 文件 167 tests）+ `node --check` + 浏览器手动验证。
- 对抗式审查每子任务必跑，Critical/Important/Minor/参考级全部修复，无未解决 Critical/Important。
- CI：5 个 PR 全部 Build×6 / Lint / Test(ubuntu+windows) / Benchmark / UI E2E / SonarQube 全绿。
- 核心 go.mod 零三方新增。

## 6. 踩坑与教训

1. **测试环（Go 模块依赖）**：pkg/client 的 e2e_test import pkg/server，任何 pkg/server→pkg/client 依赖都会破坏 `go test ./pkg/client/...`。用接口 + 实现包打破环。
2. **CI flake 用确定性信号而非死等超时**：TestConcurrency_Semaphore 5s→15s 死等仍在 Windows -race+cover 超时 → 改 mock executor 的 started channel 确定性等待（Run 被调用 = 已拿信号量）。**死等固定超时必然 flake，用产品代码的标准同步。**
3. **http.Transport HTTP/1.1 不调用 conn.SetDeadline**：超时全走内部 timer + pc.close()。写路径对端停读 → TCP 缓冲满 → Write 永久阻塞（无 SetWriteDeadline 可依赖）→ 需活跃写超时（每次 Write 用 timer 监督，到点强制 Close）。
4. **MuxStreamConn.Close 可能阻塞**：经 writeCh 发 FrameClose，写通道打满且 done 未关时永久阻塞；到期强制关闭应探测 `Abort() error` 接口。
5. **deadline 拆双 timer**：单一共享 timer 在 HTTP/1.1 persistConn 的 readLoop/writeLoop 并发下会跨方向串扰（读完成的 disarm 清掉写超时 timer）。
6. **CREATE 去重 TOCTOU**：findActive 检查 + 预留 + 插入必须整体在写锁内，否则并发同 key 双任务并发写同一 dst 路径（文件损坏）。
7. **安全审查的书面确认也是闭环**：IDOR（无 owner 设计）以注释书面确认（与 CloudTask 一致）即可通过审查。
8. **前端与服务端同裁定**：sync_task 不入 localStorage（server 唯一权威），与云任务一致；data-id 即服务端任务 id 无前缀。
9. **git hook（.githooks/pre-commit）跑 make build**：D agent 半成品文件会瞬时导致 commit 失败；切分支处理 C 修复时需 gofmt D agent 文件或确认其不冲突。
10. **子代理并行与分支隔离**：后台子代理在分支上工作时，主流程切分支会带过去其未跟踪文件，污染 PR；需先停止/隔离或确认文件不重叠。

## 7. 对后续（虚拟 IP）的建议

- 沿用「全新上下文子代理 + TDD + 对抗式审查（修全部含 Minor）+ CI + 人工 squash 合并 + learnings」流程。
- 虚拟 IP 的 `NewVirtualIPDialPolicy` 端口白名单、`vipTable` 数据源（/api/hub/nodes）、REG_OK 能力位下发、hub 持久化 NodeSnap.VirtualIP、mDNS 确定性回落——见设计文档 v2。
- 复用本阶段的安全边界模式：默认 loopback、fail-closed、mesh 隔离、HTTP/直连 TLS 明文限制。
