# TASK: 同步校验与统计报告（roadmap 4.3 P1）

## 背景
pkg/sync 引擎（engine.go）已支持文件级 diff + 逐文件结果（Job.Results []FileResult 含 Checksum），
sclient `sync push/pull`（cmd/sclient/sync.go）已能执行任务，但**无校验核对报告、无统计汇总**——
用户看不到「成功/跳过/冲突/失败清单」与逐文件校验和是否一致。roadmap 4.3 P1「同步校验与统计」：
「每次同步后校验和核对报告（成功/跳过/冲突/失败清单）」+「大同步可审计逐文件结果；失败可重试单个文件」。

## 交付内容
1. **pkg/sync 引擎加校验核对**（新文件 `pkg/sync/verify.go`）：
   - `Verify(ctx, fs, job, results)`：对 ActionCreated/ActionUpdated 的目标文件逐文件重读校验和，与源 checksum 比对
   - 不一致 → FileResult 标 `ActionError`（校验失败）+ 新 Action 常量（如 `ActionVerifyFailed`）或错误字段
   - 上下文取消可中断；错误聚合（不中断整体，收集校验失败清单）
   - **注意**：校验重读大文件有 I/O 成本——默认关闭（`VerifyAfter bool` Job 字段，默认 false 零回归），
     `--verify` 显式开启；测试必须覆盖默认关闭（零开销断言）
2. **汇总统计**（pkg/sync 新增 `Summary(results)` 或 Job 加统计字段）：
   - 按 Action 分组计数（created/updated/skipped/conflict/error/deleted…）
   - 传输字节数、文件数、耗时（Engine.Sync 已接受 ctx，可从 ctx 计时或 Job 加 StartedAt）
3. **sclient sync 输出报告**（cmd/sclient/sync.go）：
   - 默认表格：`成功 N 跳过 M 冲突 K 失败 L 删除 D` 一行汇总 + 失败清单（路径+错误，最多 20 条）
   - `--verify` flag：开启校验核对，输出 `校验失败: N`（有则详细列出）
   - `--json`：结构化报告（Summary + Results 数组）——参考 output.go 现有 JSON 输出模式
4. **测试**：
   - pkg/sync/verify_test.go：TDD——校验一致 pass、内容被篡改 fail（变异验证：故意改错 checksum 比对 → 红）
   - 默认关闭零回归断言；`--verify` 开启后校验失败可检测
   - sclient sync 输出测试（CaptureStdout 断言汇总行）

## 硬约束
- 纯标准库测试；只绑 127.0.0.1；新增测试默认 `t.Parallel()`（无法并发需 `// sproxy:serial:` + 理由）
- 中文注释，UTF-8 无 BOM；SPDX 头
- `make fmt` 通过（addlicense + gofmt -s + go fix）
- 提交前 `export PATH="$PATH:$(go env GOPATH)/bin"`

## 验收标准
- `go test -count=1 -race ./pkg/sync/` 全绿（含新增 verify_test.go + 变异验证记录）
- `go test -count=1 -race ./cmd/sclient/` 全绿（sync 输出测试）
- `make lint` 0 issues
- `make deadcode-check`（若加新 Action 常量/函数无使用方会拦）
- 文档：docs/cli.md 的 sync 命令补 `--verify` flag 说明（门禁 R15 docs_cli_flags_test.go 会拦未登记 flag）

## 流程
1. 先写红灯测试（verify 不一致 → 红；默认关 → 断言无校验调用）
2. 实现 verify.go + Summary + sclient 输出 + --verify flag
3. 全量验证（上表）+ 变异验证记录
4. 提交：`feat(sync): 同步校验与统计报告（--verify 核对 checksum，汇总+失败清单输出）`
   - 只 git add 本任务文件；多重 -m；**禁 Co-authored-by；禁 --no-verify；禁 git add -A**
5. 写 REPORT.md
