// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// cloud-tasks.test.js node --test 单测（云端任务面板纯函数，2026-11-21）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./cloud-tasks.js');

test('ctStatusLabel/Color 覆盖全部状态且失败最醒目', () => {
  assert.strictEqual(m.ctStatusLabel('failed'), '失败');
  assert.strictEqual(m.ctStatusLabel('downloading'), '下载中');
  assert.notStrictEqual(m.ctStatusColor('failed'), m.ctStatusColor('completed'));
});

test('ctClassInfo 七类失败都有中文说明，未知类别回落 unknown', () => {
  for (const cls of ['source_unreachable', 'throttled', 'content_mismatch',
    'target_conflict', 'disk_full', 'auth', 'unknown']) {
    assert.ok(m.ctClassInfo(cls).label, cls + ' 缺少标签');
  }
  assert.strictEqual(m.ctClassInfo('whatever').label, '未知');
  assert.strictEqual(m.ctClassInfo('source_unreachable').retryable, true);
  assert.strictEqual(m.ctClassInfo('disk_full').retryable, false);
});

test('ctProgressPercent 边界与未知总量', () => {
  assert.strictEqual(m.ctProgressPercent({ total_size: 100, downloaded: 50 }), 50);
  assert.strictEqual(m.ctProgressPercent({ total_size: 100, downloaded: 200 }), 100);
  assert.strictEqual(m.ctProgressPercent({ total_size: 0, downloaded: 0 }), null);
  assert.strictEqual(m.ctProgressPercent({ total_size: 100, downloaded: 0 }), 0);
  // 失败后没有 downloaded 时用 diagnostics.partial_size 兜底
  assert.strictEqual(m.ctProgressPercent({ total_size: 100, diagnostics: { partial_size: 25 } }), 25);
});

test('ctFilterTasks 支持状态与仅失败过滤', () => {
  const tasks = [
    { id: 'a', status: 'failed' },
    { id: 'b', status: 'completed' },
    { id: 'c', status: 'cancelled' },
  ];
  assert.strictEqual(m.ctFilterTasks(tasks, {}).length, 3);
  assert.strictEqual(m.ctFilterTasks(tasks, { status: 'failed' }).length, 1);
  assert.strictEqual(m.ctFilterTasks(tasks, { failedOnly: true }).length, 2);
});

test('ctSummarize 统计进行中/完成/失败/取消', () => {
  const s = m.ctSummarize([
    { status: 'downloading' }, { status: 'pending' }, { status: 'completed' },
    { status: 'failed' }, { status: 'cancelled' },
  ]);
  assert.deepStrictEqual(s, { total: 5, running: 2, completed: 1, failed: 1, cancelled: 1 });
});

test('ctGroupByClass 只统计失败并按类别分组', () => {
  const g = m.ctGroupByClass([
    { status: 'failed', diagnostics: { last_error_class: 'throttled' } },
    { status: 'failed', diagnostics: { last_error_class: 'throttled' } },
    { status: 'failed' },
    { status: 'completed' },
  ]);
  assert.strictEqual(g.throttled, 2);
  assert.strictEqual(g.unknown, 1);
  assert.strictEqual(g.completed, undefined);
});

test('ctRetryPayload 构造重试请求体', () => {
  assert.deepStrictEqual(m.ctRetryPayload('t1'), { task_id: 't1', force: false });
  assert.deepStrictEqual(m.ctRetryPayload('t1', true), { task_id: 't1', force: true });
});

test('ctPanelHtml 空列表给明确提示，失败行含原因与重试按钮', () => {
  assert.ok(m.ctPanelHtml([], {}).includes('暂无云端下载任务'));
  const html = m.ctPanelHtml([{
    id: 't1', status: 'failed', filename: 'a.mp4',
    diagnostics: { last_error_class: 'source_unreachable', attempts: 3, partial_size: 1048576 },
  }], {});
  assert.ok(html.includes('源不可达'));
  assert.ok(html.includes('3 次'));
  assert.ok(html.includes('ct-retry'));
  assert.ok(html.includes('1 MiB')); // 部分产物大小可见
});

test('ctBatchRetryPayloads 逐条构造（跳过空 id）', () => {
  const payloads = m.ctBatchRetryPayloads(['t1', '', 't2']);
  assert.strictEqual(payloads.length, 2);
  assert.deepStrictEqual(payloads[0], { task_id: 't1', force: true });
});

test('ctFailuresOnly 只保留失败/取消', () => {
  const got = m.ctFailuresOnly([
    { id: 'a', status: 'failed' }, { id: 'b', status: 'completed' },
    { id: 'c', status: 'cancelled' }, null,
  ]);
  assert.deepStrictEqual(got.map((t) => t.id), ['a', 'c']);
});

test('ctDetailHtml 展开详情含失败原文与部分产物', () => {
  const html = m.ctDetailHtml({
    id: 't1', url: 'https://x/1', transfer: { volume: 'baidu', path: 'jav/1.mp4' },
    diagnostics: {
      stage: 'downloading', attempts: 4, last_error_class: 'throttled',
      last_error: '429 too many requests', partial_size: 2097152, partial_path: 'cloud/t1',
    },
  });
  assert.ok(html.includes('jav/1.mp4'));       // 转存目标
  assert.ok(html.includes('429 too many'));    // 失败原文
  assert.ok(html.includes('cloud/t1'));        // 部分产物路径
  assert.ok(html.includes('2 MiB'));           // 部分产物大小
  assert.ok(html.includes('源限流'));           // 类别中文
});

test('ctFilterTasks 支持按卷与关键字（文件名/转存路径）', () => {
  const tasks = [
    { id: 'a', status: 'failed', filename: 'AAA.mp4', transfer: { volume: 'baidu', path: 'jav/x/AAA.mp4' } },
    { id: 'b', status: 'failed', filename: 'BBB.mp4', transfer: { volume: 'main', path: 'tmp/BBB.mp4' } },
  ];
  assert.strictEqual(m.ctFilterTasks(tasks, { volume: 'baidu' }).length, 1);
  assert.strictEqual(m.ctFilterTasks(tasks, { query: 'bbb' }).length, 1);
  assert.strictEqual(m.ctFilterTasks(tasks, { query: 'JAV/X' }).length, 1);
  assert.strictEqual(m.ctFilterTasks(tasks, { query: 'zzz' }).length, 0);
});

test('ctVolumes 去重排序；ctCopyText 输出 ID/源/目标', () => {
  const vols = m.ctVolumes([
    { transfer: { volume: 'main' } }, { transfer: { volume: 'baidu' } }, {}, { transfer: { volume: 'main' } },
  ]);
  assert.deepStrictEqual(vols, ['baidu', 'main']);
  const txt = m.ctCopyText({ id: 't1', url: 'https://x/1', transfer: { volume: 'baidu', path: 'jav/1.mp4' } });
  assert.ok(txt.includes('t1') && txt.includes('https://x/1') && txt.includes('baidu:jav/1.mp4'));
  assert.strictEqual(m.ctCopyText(null), '');
});

test('ctFilterTasks 支持按失败类别过滤', () => {
  const tasks = [
    { id: 'a', status: 'failed', diagnostics: { last_error_class: 'throttled' } },
    { id: 'b', status: 'failed', diagnostics: { last_error_class: 'disk_full' } },
    { id: 'c', status: 'failed' }, // 无类别 ⇒ unknown
  ];
  assert.strictEqual(m.ctFilterTasks(tasks, { cls: 'throttled' }).length, 1);
  assert.strictEqual(m.ctFilterTasks(tasks, { cls: 'unknown' }).length, 1);
});

test('ctPageRange 夹取边界；ctPagerHtml 显示区间并可禁用按钮', () => {
  assert.deepStrictEqual(m.ctPageRange(0, 100, 250), [0, 100]);
  assert.deepStrictEqual(m.ctPageRange(200, 100, 250), [200, 250]);
  assert.deepStrictEqual(m.ctPageRange(999, 100, 250), [200, 250]); // 越界夹到最后一页起点
  assert.deepStrictEqual(m.ctPageRange(0, 100, 0), [0, 0]);
  const html = m.ctPagerHtml(250, 0, 100);
  assert.ok(html.includes('第 1–100 条 / 共 250'));
  assert.ok(html.includes('id="ct-prev"') && html.includes('disabled'));
});

test('ctExportTSV 输出表头 + 每行任务（TSV）', () => {
  const tsv = m.ctExportTSV([
    { id: 't1', status: 'failed', url: 'https://x/1',
      diagnostics: { stage: 'downloading', last_error_class: 'throttled', attempts: 2, partial_size: 1024 },
      transfer: { volume: 'baidu', path: 'jav/1.mp4' } },
    { id: 't2', status: 'completed' },
  ]);
  const lines = tsv.trim().split('\n');
  assert.strictEqual(lines[0], 'id\tstatus\tstage\terror_class\tattempts\tvolume\tpath\turl\tpartial_bytes');
  assert.ok(lines[1].startsWith('t1\tfailed\tdownloading\tthrottled\t2\tbaidu\tjav/1.mp4\t'));
  assert.strictEqual(lines.length, 3);
  assert.strictEqual(m.ctExportTSV([]).trim().split('\n').length, 1); // 只有表头
});

test('ctFailedLastHour 只统计近 1 小时内失败', () => {
  const now = Date.parse('2026-11-22T10:00:00Z');
  const tasks = [
    { status: 'failed', diagnostics: { last_error_at: '2026-11-22T09:30:00Z' } }, // 30 分钟前
    { status: 'failed', diagnostics: { last_error_at: '2026-11-22T07:00:00Z' } }, // 3 小时前
    { status: 'failed', diagnostics: {} },                                        // 无时间
    { status: 'completed', diagnostics: { last_error_at: '2026-11-22T09:59:00Z' } },
  ];
  assert.strictEqual(m.ctFailedLastHour(tasks, now), 1);
  assert.strictEqual(m.ctFailedLastHour(null, now), 0);
});

test('ctParseTSV 按表头解析（容错缺列/空行）', () => {
  const tsv = 'id\tstatus\tvolume\tpath\turl\n' +
    't1\tfailed\tbaidu\tjav/1.mp4\thttps://x/1\n' +
    '\n' +
    't2\tcompleted\t\t\t\n';
  const got = m.ctParseTSV(tsv);
  assert.strictEqual(got.rows.length, 2);
  assert.strictEqual(got.rows[0].id, 't1');
  assert.strictEqual(got.rows[0].volume, 'baidu');
  assert.strictEqual(got.skipped, 0);
  assert.deepStrictEqual(m.ctParseTSV(''), { rows: [], skipped: 0 });
});

test('ctImportRetryPayloads 只取需重试的行（id 优先，其次卷+路径）', () => {
  const got = m.ctImportRetryPayloads([
    { id: 't1', status: 'failed' },
    { id: '', status: 'failed', volume: 'baidu', path: 'jav/2.mp4' },
    { id: '', status: 'failed' },                       // 无法定位 → 跳过
    { id: 't3', status: 'completed' },                  // 已完成 → 跳过
    { id: 't4', status: '' },                           // 未知状态按需重试
  ]);
  assert.strictEqual(got.length, 3);
  assert.deepStrictEqual(got[0], { task_id: 't1', force: true });
  assert.deepStrictEqual(got[1], { transfer_volume: 'baidu', transfer_path: 'jav/2.mp4', force: true });
  assert.deepStrictEqual(got[2], { task_id: 't4', force: true });
});

test('ctBatchCancelTargets 只取排队中/下载中的勾选项', () => {
  const tasks = [
    { id: 'a', status: 'downloading' }, { id: 'b', status: 'pending' },
    { id: 'c', status: 'failed' }, { id: 'd', status: 'completed' },
  ];
  assert.deepStrictEqual(m.ctBatchCancelTargets(tasks, ['a', 'c', 'd']), ['a']);
  assert.deepStrictEqual(m.ctBatchCancelTargets(tasks, ['a', 'b']), ['a', 'b']);
  assert.deepStrictEqual(m.ctBatchCancelTargets(tasks, []), []);
  assert.deepStrictEqual(m.ctBatchCancelTargets(null, ['a']), []);
});

test('ctSortTasks 支持状态/名称/体积/更新时间（含方向与稳定兜底）', () => {
  const tasks = [
    { id: 'a', status: 'completed', filename: 'b.mp4', total_size: 300, updated_at: '2026-11-22T01:00:00Z' },
    { id: 'b', status: 'failed', filename: 'a.mp4', total_size: 100, updated_at: '2026-11-22T03:00:00Z' },
    { id: 'c', status: 'downloading', filename: 'c.mp4', total_size: 200, updated_at: '2026-11-22T02:00:00Z' },
  ];
  assert.deepStrictEqual(m.ctSortTasks(tasks, 'status', 'asc').map((t) => t.id), ['c', 'b', 'a']);
  assert.deepStrictEqual(m.ctSortTasks(tasks, 'name', 'asc').map((t) => t.id), ['b', 'a', 'c']);
  assert.deepStrictEqual(m.ctSortTasks(tasks, 'size', 'desc').map((t) => t.id), ['a', 'c', 'b']);
  assert.deepStrictEqual(m.ctSortTasks(tasks, 'updated', 'asc').map((t) => t.id), ['a', 'c', 'b']);
  // 不改入参
  assert.deepStrictEqual(tasks.map((t) => t.id), ['a', 'b', 'c']);
});

test('ctFilterTasks 支持体积阈值（minSizeMB）', () => {
  const tasks = [
    { id: 'a', status: 'failed', total_size: 2 * 1048576 },
    { id: 'b', status: 'failed', total_size: 500 * 1048576 },
    { id: 'c', status: 'failed' }, // 未知体积 ⇒ 0
  ];
  assert.deepStrictEqual(m.ctFilterTasks(tasks, { minSizeMB: 100 }).map((t) => t.id), ['b']);
  assert.deepStrictEqual(m.ctFilterTasks(tasks, { minSizeMB: 0 }).map((t) => t.id), ['a', 'b', 'c']);
});

test('ctClassRetryPayloads 按失败类别生成重试体（只取 failed/cancelled）', () => {
  const tasks = [
    { id: 'a', status: 'failed', diagnostics: { last_error_class: 'throttled' } },
    { id: 'b', status: 'failed', diagnostics: { last_error_class: 'throttled' } },
    { id: 'c', status: 'downloading', diagnostics: { last_error_class: 'throttled' } },
    { id: 'd', status: 'failed' }, // unknown
  ];
  assert.deepStrictEqual(m.ctClassRetryPayloads(tasks, 'throttled').map((p) => p.task_id), ['a', 'b']);
  assert.deepStrictEqual(m.ctClassRetryPayloads(tasks, 'unknown').map((p) => p.task_id), ['d']);
  assert.deepStrictEqual(m.ctClassRetryPayloads(tasks, 'disk_full'), []);
});

test('ctGroupByVolume 按卷汇总并按总数降序', () => {
  const groups = m.ctGroupByVolume([
    { status: 'failed', transfer: { volume: 'baidu' } },
    { status: 'completed', transfer: { volume: 'baidu' } },
    { status: 'downloading', transfer: { volume: 'main' } },
    { status: 'failed' }, // 无目标卷
  ]);
  assert.strictEqual(groups[0].volume, 'baidu');
  assert.strictEqual(groups[0].total, 2);
  assert.strictEqual(groups[0].failed, 1);
  const none = groups.find((g) => g.volume === '(无目标卷)');
  assert.strictEqual(none.total, 1);
  assert.ok(m.ctGroupByVolumeHtml([{ status: 'failed', transfer: { volume: 'v' } }]).includes('ct-vol-chip'));
  assert.strictEqual(m.ctGroupByVolumeHtml([]), '');
});

test('ctAppendAudit 追加并裁剪；ctAuditHtml 渲染最近操作', () => {
  let log = [];
  log = m.ctAppendAudit(log, { action: '重试', target: 't1', result: 'ok' });
  log = m.ctAppendAudit(log, { action: '取消', target: 't2', result: 'failed', detail: 'boom' });
  assert.strictEqual(log.length, 2);
  assert.strictEqual(log[0].action, '取消'); // 最新在前
  for (let i = 0; i < 5; i++) log = m.ctAppendAudit(log, { action: 'x' + i }, 3);
  assert.strictEqual(log.length, 3);
  const html = m.ctAuditHtml(log, 2);
  assert.ok(html.includes('最近操作'));
  assert.ok(html.includes('x4'));
  assert.ok(m.ctAuditHtml([]).includes('暂无操作'));
});

test('ctRateSeries/ctSparklinePath 生成速率曲线（过滤 0 点、相对归一化）', () => {
  const samples = [
    { at: 't0', rate_bps: 0 },
    { at: 't1', rate_bps: 100 },
    { at: 't2', rate_bps: 200 },
    { at: 't3', rate_bps: 0 },
  ];
  assert.deepStrictEqual(m.ctRateSeries(samples), [100, 200]);
  assert.strictEqual(m.ctRateSeries([]).length, 0);
  const d = m.ctSparklinePath(samples, 100, 20);
  // 两点：x=0 与 x=100；y 按最大值归一化（200 → y=0，100 → y=10）
  assert.strictEqual(d, 'M0.0 10.0 L100.0 0.0');
  // 单点/空 ⇒ 无曲线
  assert.strictEqual(m.ctSparklinePath([{ rate_bps: 5 }], 100, 20), '');
  assert.strictEqual(m.ctSparklinePath([], 100, 20), '');
});

test('ctSparklineSvg 带可访问标签与峰值；ctRateText 人类可读', () => {
  const svg = m.ctSparklineSvg([{ rate_bps: 1048576 }, { rate_bps: 2097152 }], 120, 30);
  assert.ok(svg.includes('<svg'));
  assert.ok(svg.includes('role="img"'));
  assert.ok(svg.includes('aria-label="速率曲线'));
  assert.ok(svg.includes('峰值 2.0 MB/s'));
  assert.strictEqual(m.ctSparklineSvg([], 120, 30), '');
  assert.strictEqual(m.ctRateText(0), '0 B/s');
  assert.strictEqual(m.ctRateText(2048), '2.0 KB/s');
  assert.strictEqual(m.ctRateText(3145728), '3.0 MB/s');
});

test('ctDetailHtml 含速率曲线槽（异步填充）', () => {
  const html = m.ctDetailHtml({ id: 'cloud-x-1', status: 'failed', diagnostics: {} });
  assert.ok(html.includes('ct-spark-slot'));
  assert.ok(html.includes('data-id="cloud-x-1"'));
});

test('ctBlockMatrix 按 1MiB 拆小块并标记 done/active/todo/failed', () => {
  const rows = m.ctBlockMatrix([
    { index: 0, phase: 'done', done: 4 << 20, length: 4 << 20, source: 'share' },
    { index: 1, phase: 'downloading', done: 1 << 20, length: 4 << 20, source: 'acct' },
    { index: 2, phase: 'failed', done: 0, length: 2 << 20 },
  ]);
  assert.strictEqual(rows.length, 3);
  assert.deepStrictEqual(rows[0].blocks, ['done', 'done', 'done', 'done']);
  assert.deepStrictEqual(rows[1].blocks, ['done', 'active', 'todo', 'todo']);
  assert.deepStrictEqual(rows[2].blocks, ['failed', 'failed']);
  assert.strictEqual(rows[0].source, 'share');
  // 排序：按 index 升序
  const sorted = m.ctBlockMatrix([{ index: 2, length: 1 << 20 }, { index: 0, length: 1 << 20 }]);
  assert.deepStrictEqual(sorted.map((r) => r.index), [0, 2]);
});

test('ctBlockMatrixHtml 渲染一行多小块；无状态给占位', () => {
  const html = m.ctBlockMatrixHtml([{ index: 0, phase: 'done', done: 2 << 20, length: 2 << 20, source: 'share' }]);
  assert.ok(html.includes('ct-block-row'));
  assert.ok(html.includes('ct-block-done'));
  assert.ok(html.includes('data-chunk="0"'));
  assert.ok(html.includes('share'));
  assert.ok(m.ctBlockMatrixHtml([]).includes('暂无分块状态'));
  assert.ok(m.ctBlockMatrixHtml(null).includes('暂无分块状态'));
});

test('ctDetailHtml 含分块进度槽', () => {
  const html = m.ctDetailHtml({ id: 'cloud-y-1', status: 'downloading', diagnostics: {} });
  assert.ok(html.includes('ct-matrix-slot'));
});

test('ctActionsFor 优先服务端 actions；无则按判据兜底（下载未完成/无备选源禁用）', () => {
  const serverPlan = [{ action: 'retry_download', label: '续传下载' }];
  assert.deepStrictEqual(m.ctActionsFor({ status: 'failed', actions: serverPlan }), serverPlan);
  assert.deepStrictEqual(m.ctActionsFor({ status: 'completed' }), []);
  const plan = m.ctActionsFor({ status: 'failed' });
  const by = {};
  plan.forEach((p) => { by[p.action] = p; });
  assert.ok(by.retry_download);
  assert.strictEqual(by.retry_transfer.disabled, true);
  assert.strictEqual(by.rotate_source.disabled, true);
  assert.ok(by.discard_partial);
  const full = m.ctActionsFor({
    status: 'failed', download_done: true, transfer: { volume: 'v' },
    fallback_sources: ['u1', 'u2'],
  });
  assert.strictEqual(full.filter((p) => p.disabled).length, 0);
});

test('ctActionControlsHtml 渲染下拉与执行按钮；禁用项带原因；非失败态为空', () => {
  const html = m.ctActionControlsHtml({ id: 't1', status: 'failed' });
  assert.ok(html.includes('ct-action-select'));
  assert.ok(html.includes('ct-action-apply'));
  assert.ok(html.includes('retry_download'));
  assert.ok(html.includes('下载未完成'));
  assert.strictEqual(m.ctActionControlsHtml({ id: 't2', status: 'completed' }), '');
  assert.strictEqual(m.ctActionControlsHtml(null), '');
});

test('ctRowHtml 失败行含动作控件（v2 不只「重试」）', () => {
  const html = m.ctRowHtml({ id: 't3', status: 'failed', filename: 'a.mp4', diagnostics: {} });
  assert.ok(html.includes('ct-action-select'));
  assert.ok(html.includes('ct-retry'));
});

test('ctActionDisabled 按服务端计划判定（未知动作按禁用处理）', () => {
  const plan = m.ctActionsFor({ status: 'failed' }); // 下载未完成 ⇒ retry_transfer 禁用
  assert.strictEqual(m.ctActionDisabled(plan, 'retry_transfer'), true);
  assert.strictEqual(m.ctActionDisabled(plan, 'retry_download'), false);
  assert.strictEqual(m.ctActionDisabled(plan, 'no_such_action'), true);
  assert.strictEqual(m.ctActionDisabled(null, 'retry_download'), true);
});

test('ctActionsFor 含覆盖转存与换链接（needs_url），并随前置条件禁用', () => {
  const plan = m.ctActionsFor({ status: 'failed', download_done: true, transfer: { volume: 'v' } });
  const by = {};
  plan.forEach((p) => { by[p.action] = p; });
  assert.ok(by.retry_transfer_overwrite, '应含覆盖转存');
  assert.ok(by.retry_from_url && by.retry_from_url.needs_url === true, '换链接应标记 needs_url');
  assert.strictEqual(m.ctActionNeedsURL(plan, 'retry_from_url'), true);
  assert.strictEqual(m.ctActionNeedsURL(plan, 'retry_download'), false);
  const noArtifact = m.ctActionsFor({ status: 'failed' });
  const by2 = {};
  noArtifact.forEach((p) => { by2[p.action] = p; });
  assert.strictEqual(by2.retry_transfer_overwrite.disabled, true);
});

test('ctBatchActionOptionsHtml 列出可批量动作（不含换链接）', () => {
  const html = m.ctBatchActionOptionsHtml();
  assert.ok(html.includes('retry_download'));
  assert.ok(html.includes('retry_transfer_overwrite'));
  assert.ok(!html.includes('retry_from_url'));
});

test('ctRowHtml/表格含批量动作控件', () => {
  const table = m.ctTableHtml([{ id: 't9', status: 'failed', filename: 'x.bin', diagnostics: {} }]);
  assert.ok(table.includes('ct-batch-action'));
  assert.ok(table.includes('ct-batch-apply'));
});

test('ctEtaSeconds 用最近速率中位数估算；无速率/已完成返回 null', () => {
  const samples = [
    { rate_bps: 1000 }, { rate_bps: 1000 }, { rate_bps: 5000 }, // 中位数 1000
  ];
  // 剩 10 MiB @ 1000 B/s ≈ 10486s
  assert.strictEqual(m.ctEtaSeconds(samples, 10 << 20, 0), Math.ceil((10 << 20) / 1000));
  assert.strictEqual(m.ctEtaSeconds([], 1000, 0), null);
  assert.strictEqual(m.ctEtaSeconds(samples, 100, 100), null, '已完成不估');
  assert.strictEqual(m.ctEtaSeconds(samples, 0, 0), null, '未知总量不估');
  assert.strictEqual(m.ctEtaSeconds([{ rate_bps: 0 }], 100, 0), null, '无有效速率不编造');
});

test('ctEtaText 人类可读且标明"约"', () => {
  assert.strictEqual(m.ctEtaText(null), '-');
  assert.strictEqual(m.ctEtaText(30), '约 30 秒');
  assert.strictEqual(m.ctEtaText(600), '约 10 分钟');
  assert.strictEqual(m.ctEtaText(3600), '约 1 小时');
  assert.strictEqual(m.ctEtaText(5400), '约 1 小时 30 分');
});

test('ctStageTimeline 提取阶段首点；HTML 渲染中文阶段', () => {
  const samples = [
    { at: '2026-11-26T10:00:00.123Z', stage: 'downloading', downloaded: 0 },
    { at: '2026-11-26T10:05:00Z', stage: 'downloading', downloaded: 1 },
    { at: '2026-11-26T10:06:00Z', stage: 'transferring', downloaded: 2 },
  ];
  const rows = m.ctStageTimeline(samples);
  assert.strictEqual(rows.length, 2);
  assert.strictEqual(rows[0].stage, 'downloading');
  assert.strictEqual(rows[1].stage, 'transferring');
  const html = m.ctStageTimelineHtml(samples);
  assert.ok(html.includes('下载中'));
  assert.ok(html.includes('转存中'));
  assert.strictEqual(m.ctStageTimelineHtml([]), '');
});

test('ctDetailHtml 含 ETA 与阶段时间轴槽', () => {
  const html = m.ctDetailHtml({ id: 't-eta', status: 'downloading', diagnostics: {} });
  assert.ok(html.includes('ct-eta-slot'));
  assert.ok(html.includes('ct-timeline-slot'));
});
