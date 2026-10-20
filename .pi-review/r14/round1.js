/**
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// 第一轮对抗评审：5 个 fresh-context 只读 reviewer（不同视角，互相不可见）。
const NL = String.fromCharCode(10);
const PRE = [ "你是**独立对抗式代码评审员**，只读（read/grep/find/ls），禁止修改文件、禁止运行命令/测试。", "", "仓库：D:/workdir/leon/cocomhub/sproxy（分支 master，HEAD=49c9910ae）。全部代码可读。", "注意：本会话工作目录可能不是该仓库，读文件请用上面的绝对路径。", "", "规则（必须遵守，否则报告作废）：", "1. 每条结论必须给 **file:line** 证据；不许凭印象、不许「可能存在」式臆测。推断性结论单独列到「未证伪/待验证」区。", "2. 分级：P0=生产事故/数据丢失/安全；P1=功能错误或重启/重试不可靠；P2=边界与可观测性缺口；P3=可维护性。", "3. 每条结论写一句「实现正确时该断言/代码是否仍成立」——防把 bug 当契约。", "4. 报告结构：① 该领域能否生产级的结论 ② 逐条发现（file:line、复现路径、影响、建议修法）③ 未证伪/待验证 ④ 本次未覆盖的文件（诚实说明）。", "5. 中文；不要客套；不要罗列文件清单；总长 1200 字内，只写高价值发现。", "" ].join(NL);
const LENSES = [
  { key: "resume-retry", task: PRE + NL + NL + [ "你的领域：**中断/异常自动重试恢复 + 服务重启自动继续**（下载/转存/上传/同步）。", "重点路径：pkg/cloud/（manager*.go、transfer.go、download*）、pkg/files/（chunked_upload.go、chunked_download.go、resume*、write_ops.go）、pkg/recovery/、pkg/syncexec/、cmd/sproxy/、任务持久化与幂等（WAL、Seq、MarkDone）。", "", "必须回答：", "1. 进程在下载/转存/上传进行中被 kill -9：重启后是否自动继续？哪些状态能继续，哪些会永久卡住或静默丢任务（给代码路径）？", "2. 网络瞬断/5xx/超时：是否有有界重试+退避？重试是否幂等（重复写/重复计费/重复审计/重复删除）？", "3. 取消 vs 自动重试竞态：是否存在「取消后被重试复活」「重试覆盖已完成结果」？", "4. 任务状态机是否有不可达状态/终态泄漏（completed 但文件缺失、failed 残留 partial、超时仍占 worker）？", "5. 「自动恢复」的限制条件是否显式可观测（日志/指标/文档），还是静默放弃？" ].join(NL) },
  { key: "quota-wait", task: PRE + NL + NL + [ "你的领域：**配额/容量不足时自动等待重试** + 容量记账真值 + staging 预留。", "重点路径：pkg/quota/（wait.go、waitq_adapter.go、allocation.go、quota.go、quota_writer.go、staging_tracker.go、task_account.go）、pkg/sync/staging_gate.go、pkg/volume/capacity/、pkg/server/（volumes.go 的 reserveVolumePool/volumeRoute、s3_part_ledger.go、s3_multipart.go、cloud_archive*、quota_reconcile.go）、pkg/waitq/。", "", "必须回答：", "1. 哪些写路径在配额不足时是「等待重试」，哪些是直接 507 拒绝？尽量列全（上传/分块/S3 单 PUT/multipart complete/转存/归档/版本恢复/同步 push/回收站恢复），差异是否有据还是漏接线。", "2. 等待的有界性：超时、ctx 取消、广播丢失、惊群、公平性；是否存在永久阻塞/饥饿？", "3. 等待与账户一致性：预留泄漏/双计（scope+pool+staging 三处），失败回滚是否幂等？", "4. 容量记账真值：CapacityFS/LedgerCounter、卷池、owner Scope、磁盘扫描对账四者口径是否一致？哪些操作导致漂移（覆盖写/改名/删除/回收站/版本/外部卷直写/归档）？", "5. 磁盘物理满（ENOSPC）是否被当成「配额不足」去等待（无限等待）？" ].join(NL) },
  { key: "audit-newcode", task: PRE + NL + NL + [ "你的领域：**最近合并的新代码**（PR #836/#837）正确性与假绿。", "重点：pkg/audit/cold.go（ReadTailCold/tailEntries/chunkEntriesDesc/PurgeCold/OpenRange）及其测试；pkg/audit/service.go（ContextFields.Volume/fillContext）；pkg/server/（audit.go 的 auditVolumeFallback/RecordAudit、audit_ctx.go、audit_middleware.go、dav.go、s3_server.go、audit_handler.go 的 auditColdSegHandler/readAuditCold）；internal/archcheck/（mutating_route_audit_gate_test.go、audit_readface_gate_test.go、audit_archive_prefix_gate_test.go、audit_action_gate_test.go）；pkg/server/routes_setup.go（wireAuditColdArchive/validateAuditArchiveVolume/newColdArchiveFS）。", "", "必须回答：", "1. ReadTailCold 正确性：跨块行被切断、多段合并顺序、Since 跳过、limit 截断、无 RangeReader 回落、空段/末行无换行、段正在被追加（并发增长）——逐个推演，指出会返回错误条目集的输入。", "2. 卷维度漏洞：ctx 绑定被覆盖？DAV/S3 绑定是否在错误分支泄漏？auditVolumeFallback 在多卷但实际只一个卷可达时是否留空？", "3. 冷段区间端点：白名单绕过（路径等价形态/前缀）、区间夹紧整数溢出、RangeReader 短读行为、未启用归档时的信息泄露。", "4. 三条门禁是否仍可假绿（给具体绕过写法）。", "5. 专用性校验可被配置绕过吗（archive_dir 与 archive_volume 同指向、ACL 运行时变更、卷改名）？" ].join(NL) },
  { key: "prod-crosscut", task: PRE + NL + NL + [ "你的领域：**跨切生产可用性**：静默失败、超时/ctx 传播、并发与锁序、资源泄漏、幂等、可观测性、优雅停机。", "扫描面：pkg/server（handlers/routes/writeGuard/transfer locks/staging/优雅停机）、pkg/files（runtime/write_ops/read_ops/trash/version_store）、pkg/cloud、pkg/sync、pkg/audit、pkg/tunnel（hub/relay 重连）、cmd/sproxy 生命周期。", "", "必须回答：", "1. 「best-effort 吞错且无可观测通道」的位置——按影响排序，前 10 条给 file:line。", "2. 优雅停机：在途上传/下载/转存/同步/审计 flush 是否被等待或标记？kill -9 后是否留下永久占用（锁、staging 预留、任务占位、partial 文件）？", "3. ctx 传播：长任务用 context.Background()/无超时的点（列最危险的），后果是什么？", "4. 锁：transferWriteLock/rel write lock/卷级锁的键空间一致性，以及锁内做远端 IO 的最危险 5 处。", "5. 并发安全：锁外读写共享状态（pkg/audit RingIndex、pkg/cloud manager、staging tracker、capacity 账本）。" ].join(NL) },
  { key: "quality-perf", task: PRE + NL + NL + [ "你的领域：**可测试性/可维护性/收敛性/可扩展性/性能/门禁强度/文档一致性**。", "重点：internal/archcheck（各门禁判据强度）、pkg/audit、pkg/files、pkg/server 热路径（下载 chunk/列表/du/审计读面/同步）、docs/（api.md、config.md、testing/audit-scenarios.md、designs/2026-10-19-audit-service.md）。", "", "必须回答：", "1. 假绿门禁：哪几条可「改写法就绕过」？给绕过示例（伪代码）。哪些关键不变量没有门禁（前 5）？", "2. 重复实现/未收敛：同一语义多份实现（file:line 成对列出）；「加适配层」本可上移的点。", "3. 性能：热路径 O(n) 全量扫描/重复 Stat（file:line + 量级 + 触发条件），尤其审计读面、冷段、列表、du、下载 chunk、同步 diff。", "4. 可测试性：核心不变量只能靠 e2e（缺接缝）的清单；锁住实现细节的测试（重构即红）。", "5. 文档漂移：docs 与代码事实性冲突前 10 条（两侧证据）。" ].join(NL) },
];

const results = await runs.all(LENSES.map(l => ({ key: l.key, agent: "reviewer", context: "fresh", task: l.task })));
const summary = {};
for (let i = 0; i < results.length; i++) {
  const r = results[i];
  summary[LENSES[i].key] = { runId: r.runId, output: r.output };
  emit(LENSES[i].key + " done");
}
return summary;
