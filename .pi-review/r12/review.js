/**
 * Copyright 2026 The Cocomhub Authors. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

// 第三轮代码审核（用户维度：功能正确性/可靠性/可维护性/整洁性/完备性/收敛/接线/可用）
// 三个独立 fresh-context read-only 审核员并行；简报短、指向路径，规避上游长上下文拒绝。
const repo = 'D:/workdir/leon/cocomhub/sproxy';

function brief(dims) {
  return [
    '你是独立代码审核员（read-only，fresh context）。',
    '仓库：' + repo + '（git worktree，HEAD=bb238229f，分支 docs/secretdata-roadmap，基线 origin/master）。',
    '审核范围：git diff origin/master..HEAD（17 文件 +2746/-161）。用 bash 跑该命令，或 read 直接读文件。',
    '',
    '你的审核维度：' + dims,
    '',
    '硬性要求：',
    '- 只读：不得 edit/write，不得 git commit/push，不得 gh。',
    "- 可用 read/grep/find/ls 与只读 bash；禁止全量测试/lint（用 grep -n -m N、sed -n 'a,bp' 定位）；单条命令 <=60s。",
    '- 每条发现必须给：文件:行号 + 代码/文档原文证据 + 影响 + 严重级别（P0 崩溃/数据丢失、P1 功能错误、P2 可靠性/可维护性）。',
    '- 必须列出「你无法验证的事项」，不要猜。',
    '- 不要提「建议加测试」这类空话；要具体到函数/分支/判据。',
    '- 中文输出；结论先行；最多 8 条发现（按严重级别排序）；末尾一行总结。'
  ].join('\n');
}

const rs = await runs.all([
  {
    key: 'revA',
    agent: 'reviewer',
    task: brief(
      '功能正确性 + 可靠性。重点文件：pkg/volume/secretdata/secretdata.go、features.go、erasure.go、' +
        'pkg/cryptox/shardseal/shardseal.go、pkg/volume/ext/baidupcs/storage.go、pkg/server/version.go。' +
        '特别核查：块级交错上传在各路径（数据分块/meta/parity/dedup/纠错）的正确性；失败回滚是否覆盖全部出口；' +
        '崩溃一致性（中途失败后能否恢复）；并发与锁（含持锁做远端 I/O）；边界（size=0/负/未知长度/空文件/超大文件）；' +
        '错误传播是否 fail-closed；有无新增数据丢失或静默接受坏数据；有无越界 panic 风险（切片索引/空切片）。'
    )
  },
  {
    key: 'revB',
    agent: 'reviewer',
    task: brief(
      '收敛（唯一实现）+ 接线正确 + 完备性。特别核查：(1) 同一功能是否有多份实现（staging 预留口径、暂存复用、' +
        '秒传刷新、流式加密、空间不足回退链、单块加密）；(2) 能力透传链是否完整（StagingQuotaCapable / ' +
        'StagedPathReuse / RangeReader / ChunkSink 等扩展接口在内层适配器与各装饰器上是否被转发，有无断链或静默忽略）；' +
        '(3) 装配层是否真的接上新能力（cmd/sproxy 装配、ApplyStagingQuota、WithStagingQuota 的调用点）；' +
        '(4) 完备性：所有写路径与所有失败出口是否一致覆盖（核查 docs/roadmap.md 里「先登记后上传未覆盖全部上传点」' +
        '是否属实、还有无遗漏点）；(5) 死代码/未使用导出/不可达分支。'
    )
  },
  {
    key: 'revC',
    agent: 'reviewer',
    task: brief(
      '可维护性 + 整洁性 + 可用性 + 测试保真 + 文档一致性。特别核查：(1) 测试是否自证式/弱断言（恒真断言、' +
        '只断言不报错、夹具忽略参数导致假绿、判据选在无法区分成功与失败的位置）；(2) 测试确定性（随机夹具、' +
        '时序依赖、sleep、依赖 map 顺序、依赖被阻塞 goroutine 的退出顺序）；(3) 注释与文档是否与实现一致' +
        '（仍在说谎的注释、过时函数名、把近似说成上界的夸大措辞）；(4) 命名与结构整洁性（超长函数、重复样板、' +
        '可内联封装、参数过多）；(5) 可用性（错误文案可诊断性、降级可观测性、用户可见行为合理性）。'
    )
  }
]);

return rs;
