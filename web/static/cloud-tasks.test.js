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
