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
