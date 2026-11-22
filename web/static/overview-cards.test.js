// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./overview-cards.js');

test('overviewCards 汇总四张卡片（失败/进行中/待处理/需人工）', () => {
  const cards = m.overviewCards(
    { failed: 3, running: 2, completed: 10, cancelled: 1, total: 16 },
    { open: 4, retrying: 1, needs_human: 2, resolved: 9 },
  );
  const byKey = Object.fromEntries(cards.map((c) => [c.key, c]));
  assert.strictEqual(byKey['cloud-failed'].value, 3);
  assert.strictEqual(byKey['cloud-failed'].tone, 'bad');
  assert.strictEqual(byKey['cloud-running'].value, 2);
  assert.strictEqual(byKey['issues-pending'].value, 5); // open + retrying
  assert.strictEqual(byKey['issues-human'].value, 2);
  assert.strictEqual(byKey['issues-human'].tone, 'bad');
});

test('overviewCards 空数据全 0 且为正常色（不误报）', () => {
  const cards = m.overviewCards(null, null);
  assert.ok(cards.every((c) => c.value === 0));
  assert.strictEqual(cards.find((c) => c.key === 'cloud-failed').tone, 'ok');
});

test('overviewCardsHtml 渲染每张卡片（0 也显示）', () => {
  const html = m.overviewCardsHtml({ failed: 0 }, { needs_human: 1 });
  assert.ok(html.includes('data-card="cloud-failed"'));
  assert.ok(html.includes('data-card="issues-human"'));
  assert.ok(html.includes('>1<'));
});

test('overviewCardAction 卡片→分区与过滤映射', () => {
  assert.deepStrictEqual(m.overviewCardAction('cloud-failed'), { tab: 'cloud-tasks', filters: { failedOnly: true } });
  assert.deepStrictEqual(m.overviewCardAction('cloud-running'), { tab: 'cloud-tasks', filters: { status: 'downloading' } });
  assert.deepStrictEqual(m.overviewCardAction('issues-human'), { tab: 'issues', status: 'needs_human' });
  assert.deepStrictEqual(m.overviewCardAction('nope'), { tab: 'stats', filters: {} });
});

test('overviewAuditHtml 渲染最近事件（最多 5 条，空有提示）', () => {
  assert.ok(m.overviewAuditHtml([]).includes('暂无事件'));
  const html = m.overviewAuditHtml([
    { action: 'cloud_task_retry', object: 'cloud-1', result: 'success' },
    { action: 'delete', object: 'a.mp4', result: 'error' },
  ]);
  assert.ok(html.includes('cloud_task_retry'));
  assert.ok(html.includes('#c0392b')); // 失败用红色
  const many = m.overviewAuditHtml(new Array(10).fill({ action: 'x', result: 'success' }));
  assert.strictEqual((many.match(/<li>/g) || []).length, 5);
});

test('overviewCards 失败卡片 hint 带「近 1 小时新增」', () => {
  const cards = m.overviewCards({ failed: 5 }, {}, 2);
  const card = cards.find((c) => c.key === 'cloud-failed');
  assert.ok(card.hint.includes('近 1 小时新增 2 个'));
  const noTrend = m.overviewCards({ failed: 5 }, {}, 0).find((c) => c.key === 'cloud-failed');
  assert.ok(!noTrend.hint.includes('近 1 小时'));
});
