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
