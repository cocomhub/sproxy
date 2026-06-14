## [LRN-20260614-BP6] sync.Once for Close() 幂等安全

**Logged**: 2026-06-14T19:00:00Z
**Priority**: high
**Status**: pending
**Area**: backend

### Summary
`close(ch)` 多次调用 panic，所有可多次调用的 Close() 必须用 `sync.Once` 保护 channel close。

### Details
`TcpListener.Close()` 原始代码直接 `close(l.closeCh)`，如果 Accept 因其他原因先退出并调用 Close，后续再次调用 Close 时 panic。修复方案：`closeOnce sync.Once` + `l.closeOnce.Do(func() { close(l.closeCh) })`。

关闭的 listener 可安全返回 `xfer.ErrConnClosed` 给阻塞的 Accept goroutine，不再需要重复 close。

### Metadata
- Source: test_fix (TestTcpWithSuite/SendAfterClose panic)
- Pattern-Key: concurrency.close_once
- Recurrence-Count: 1
- First-Seen: 2026-06-14
- Related Files: `pkg/tunnel/xfer/internal/tcp/tcp.go`

---

## [LRN-20260614-BP7] 测试不能验证 nil/空内容 — 无效测试的反模式

**Logged**: 2026-06-14T20:00:00Z
**Priority**: medium
**Status**: pending
**Area**: tests

### Summary
本次 code review 发现的多个测试反模式：
1. 测试体传递 nil cfgPtr 但永远不 panic（但将来 handler 变严谨就可能 panic）
2. `TestBatchRenameHandler` 使用 `_ = w` 丢弃结果，没有任何断言
3. `TestPrintBatchResults` 虽然 captureStdout 但不验证输出内容
4. tunnel flag 测试用 `_ = rootCmd.Execute()` 吞掉错误
5. 死代码 helper functions (`contains`/`searchString` 替代 `strings.Contains`)

### Details
这些测试的形式是"不 panic 就通过"的占位验证。如果测试有 table、有 mock server、有 capture，但最终断言缺失，测试只会消耗 CI 时间而不会捕获回归。

### Suggested Action
编写测试时必须确保：
- 有明确的预期断言（不是"不 panic"）
- 辅助函数被至少一个测试使用
- `_ =` 几乎总是需要替换为 `if err := ...; err != nil { t.Log(err) }`
- 手写 stdlib 替代函数（如 `stringsRepeat`、`searchString`）必须删除

### Metadata
- Source: code_review
- Pattern-Key: tests.assertion_completeness
- Recurrence-Count: 1
- First-Seen: 2026-06-14
- Related Files: `pkg/server/metrics_test_extra.go`, `cmd/sclient/cmd_rune_test.go`, `cmd/sclient/batch_test.go`, `pkg/tunnel/tracing/slog.go`

---

## [LRN-20260614-BP8] EncryptStreamWithChunkSize chunkSize=0 无限循环

**Logged**: 2026-06-14T20:00:00Z
**Priority**: low
**Status**: pending
**Area**: backend

### Summary
`EncryptStreamWithChunkSize(key, r, w, 0)` 调用 `getBuf(0)` 返回空切片，导致 `io.ReadFull(r, buf)` 死循环（读 0 字节 = 不读 = 一直等）。

### Details
`io.ReadFull` 的语义是"读满 len(buf) 字节"，如果 len(buf) == 0，它立即返回 (0, nil)，而 EncryptStreamWithChunkSize 的 for 循环接到 nil error 继续调用 getBuf(0) → ReadFull(0) → ... 无限循环。

生产代码不会传入 chunkSize=0，因为 DefaultChunkSize 是 64KB。但这是一个潜伏的 edge case。

### Suggested Action
有两个选择：(A) 在 EncryptStreamWithChunkSize 开头检查 `if chunkSize <= 0 { return 0, errors.New("chunkSize must be > 0") }`，(B) 关闭此问题为 NOTEST。当前选择 (B)，因为引入 error 检查会改变生产路径但无实际使用场景。

### Metadata
- Source: testing
- Pattern-Key: stream.encrypt.zero_chunk
- Recurrence-Count: 1
- First-Seen: 2026-06-14
- Related Files: `pkg/tunnel/stream.go`

---

## [LRN-20260614-BP9] gofmt 自动化缩进转换导致的测试 diff 噪声

**Logged**: 2026-06-14T20:00:00Z
**Priority**: low
**Status**: pending
**Area**: config

### Summary
`go fmt ./...`（作为 `make fmt` 的一部分）将 `mux_test.go` 等文件从 4 空格缩进批量转换为 tab 缩进，导致 400+ 行的纯缩进 diff 噪声，掩埋了真实变更。

### Details
项目代码风格是 Go 标准 tab 缩进，但部分测试文件（特别是新增的）被编辑工具以 4 空格格式写入。后续 `go fmt` 批量转换产生大量无意义 diff。Review 时几乎无法区分空格 → tab 变更与逻辑变更。

### Suggested Action
是否已解决：被 gofmt 修正的文件已全部提交，不会再次出现。教训：在修改文件前先 `go fmt file.go` 确保目标文件使用项目一致格式，避免后续产生纯格式 diff。

### Metadata
- Source: experience
- Pattern-Key: tooling.gofmt_noise
- Recurrence-Count: 1
- First-Seen: 2026-06-14
- Related Files: `pkg/tunnel/mux/mux_test.go`, `pkg/tunnel/tunnel_mux_test.go`

## [LRN-20260615-BP1] windows-loopback-bind

**Logged**: 2026-06-15T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: tests

### Summary
Windows 测试必须绑定 127.0.0.1。0.0.0.0/localhost 触发防火墙弹窗，CI 卡死。

### Details
httptest.NewServer 默认 127.0.0.1 ✅。原有测试中使用了 0.0.0.0 或 localhost 导致 Windows CI 上测试 hang。已通过 check-loopback make target 自动化校验。

### Metadata
- Source: user_feedback
- Pattern-Key: testing.windows_loopback
- Related Files: Makefile

---

## [LRN-20260615-BP2] commit-prep-chain

**Logged**: 2026-06-15T09:00:00Z
**Priority**: high
**Status**: promoted
**Area**: config

### Summary
提交前必须先 go fix + go fmt + addlicense，否则产生大量格式 diff 噪声。

### Details
go fmt 从 4 空格 → tab 的转换产生 400+ 行纯缩进 diff。修复：在修改目标文件前先 go fmt 它，确保一致。

### Metadata
- Source: user_feedback
- Pattern-Key: tooling.commit_chain
- Related Files: Makefile

---

## [LRN-20260615-BP3] xfertest-for-plugins

**Logged**: 2026-06-15T09:00:00Z
**Priority**: medium
**Status**: promoted
**Area**: tests

### Summary
第三方 transport 插件应当能用 xfertest 套件统一验证。xfertest 在 pkg/ 下（非 internal/），外部 go module 可直接 import。

### Details
ws/quic 已使用 xfertest.TestHarness。tcp 追加了 TestHarness 调用。新增 transport 必须也使用，禁止自写重复测试。

### Metadata
- Source: user_feedback
- Pattern-Key: testing.xfertest_plugin
- Related Files: pkg/tunnel/xfer/xfertest/

---

## [LRN-20260615-BP4] parallel-isolated-tasks-avoid-worktree

**Logged**: 2026-06-15T09:00:00Z
**Priority**: medium
**Status**: promoted
**Area**: infra

### Summary
互不依赖的任务（不同文件、不同包）优先并发操作，不要 worktree。worktree 只在分支隔离必需时使用。

### Details
worktree 创建开销 200-500ms + 磁盘占用。直接并发工具调用更高效。

### Metadata
- Source: user_feedback
- Pattern-Key: infra.parallel_avoid_worktree
