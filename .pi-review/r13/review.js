/**
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// R13：PR #833（审计 program）合并后的三路 fresh-context 代码审查（只读）。
// 运行：subagent({ workflow: './.pi-review/r13/review.js', async: false })

const common = [
  '仓库：D:/workdir/leon/cocomhub/sproxy（master = e81619a9a「feat(audit)!: pkg/audit 唯一提供方 + WAL 恢复 + 冷热归档 + 读面收敛 (#833)」）。',
  '待审对象：合并提交 HEAD~1..HEAD 的全部改动。',
  '  · 完整 diff 已导出：/tmp/pr833_audit_program.diff（21670 行）',
  '  · 变更文件清单：/tmp/pr833_files.txt（180 个文件）',
  '工作方式：**只读**。可读 diff、可直接读仓库源码、可 grep/find 交叉验证。**不要修改任何文件**。',
  '产出要求（中文）：',
  '  · 最多 12 条发现，按严重度排序；每条给：严重级别(P0 阻断/P1 应修/P2 建议/P3 备注) + `文件:行` + 证据（引用实际代码片段或你读到的实现）+ 影响 + 建议修复；',
  '  · 只报**能从代码证据推出**的问题；不确定的必须显式标「未验证」并说明缺什么证据；',
  '  · 不报纯风格/格式（golangci-lint 已覆盖）与「我更喜欢」类意见；',
  '  · 末尾单列「你无法验证的点」（例如需要运行测试/压测才能确认的）。',
  '纪律：**不要相信注释**，一切以实现为准；同一问题只报一次（不要为每个调用点重复计条）。',
].join('\n')

const lanes = [
  {
    key: 'A-correctness-prod',
    task: [
      '维度：**正确性 + 生产可用性**。重点：',
      '1) 并发与锁：审计 `Service`/`RingIndex`/`Journal`/`wakeup`、`pkg/recovery.Coordinator`、归档 `OnSeal` 回调（锁外触发）、`staging`/`quota` 交互；锁序、锁内做 IO、锁外读受锁字段（本次 CI 已抓到一个 `RingIndex.size` race，请找**同类**残留）。',
      '2) 崩溃/恢复语义：`MarkDone` 与副作用顺序、`initSeq` 跨进程续号、重放幂等前提、`Resume` 早退条件、`ListScopes` 的「最大 Seq 为末条」判据是否还有脆弱点。',
      '3) 错误处理与静默失败：best-effort 吞错是否有可观测通道（metrics/logger）、`nil` 兜底是否会把失败读成成功、`context` 取消/超时是否正确传播（归档、冷读、presign、AI）。',
      '4) 资源：文件句柄/被包装 reader 的 Close、TempDir 清理、goroutine 泄漏（每轮 spawn）、ticker/loop 的停止。',
      '5) fail-closed 语义：审计读面 admin-only、冷段不可读时应否静默跳过、`audit.archive_volume` 不存在时的降级是否会被误认为「已归档」。',
    ].join('\n'),
  },
  {
    key: 'B-convergence',
    task: [
      '维度：**收敛（去重）+ 完备性**。重点：',
      '1) 是否存在**同形重复实现**：审计的归档/冷读是否真的只经 `pkg/sync` 能力（`WriteIfAbsent`/`RangeReader`/`WalkEntries`），有没有残留的第二套 sink/store；`RecordAudit`/`audit.Entry` 的转换是否有两处实现；`pkg/recovery` 与 `pkg/cloud` 的恢复是否只剩一条编排路径。',
      '2) **单一构造点/单一事实源**：`newColdArchiveFS`、`auditColdFS`、`setupAuditServices`、`newSealArchiveHook`、`NewEmbedClient`、`attachTestAudit`（测试接线）是否各只有一处；是否存在绕过它们的直接构造。',
      '3) **能力复用**：是否在 server 层重复实现了 `pkg` 已提供的能力（配额等待、FS 遍历、时间探测、行解码）。',
      '4) **门禁覆盖是否名副其实**：R37/R38/R39 的断言是否真的能拦住它们声称的反模式（有无假绿路径，例如清单为空、正则漏匹配、只扫部分目录）；`docs/gates.md` 与门禁实现是否一致。',
      '5) **死代码/未用**：未使用的导出、未接线的配置项（如 `AuditConfig` 各字段是否都有实现）、`Action*` 常量是否都有真实埋点。',
    ].join('\n'),
  },
  {
    key: 'C-maintainability',
    task: [
      '维度：**可维护性 + 完整性（含测试保真）**。重点：',
      '1) 注释/文档与实现是否一致（尤其：注释声称的契约是否真的被代码保证，如「锁外触发」「不静默回落」「绝不记录文本」）。',
      '2) 复杂度与可读性：是否有函数复杂度逼近门禁阈值、可抽未抽、参数膨胀、布尔旗标爆炸。',
      '3) **测试保真**：新增的 §9.1 场景用例是否真能红（断言是否依赖实现细节/弱断言/重复断言）；`audit_cold_merge_test.go`、`audit_archive_volume_test.go`、`audit_presign_scenarios_test.go` 等是否存在「注释说验证 A，实际只验证 B」；`t.Parallel()`/串行预算是否合规。',
      '4) **完整性**：设计文档（`docs/designs/2026-10-19-audit-service.md` §9.1/§5.7/§6/§8.2）声明 vs 实现 vs 测试 三者是否闭环；`docs/testing/audit-scenarios.md` 的 60/60 声称是否与代码埋点一致（有无漏项或虚标）。',
      '5) 配置与运维：新增配置项（`audit.max_total_bytes`/`archive_dir`/`archive_keep`/`archive_volume`）的默认值、校验、文档、迁移口径是否一致；升级路径（旧 `<卷根>/audit/` 迁移）是否会丢数据。',
    ].join('\n'),
  },
]

const results = await runs.all(
  lanes.map((l) => ({ key: l.key, agent: 'reviewer', task: common + '\n\n' + l.task })),
)
return results.map((r) => ({ key: r.key, runId: r.runId, output: r.output }))
