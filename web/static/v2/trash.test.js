// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./trash.js');

test('normalizeEntries：字段归一 + 过滤坏条目', () => {
  const out = m.normalizeEntries({ entries: [
    { trash_rel: 'user/a.txt.__deleted__', name: 'a.txt', volume: 'default' },
    { trash_rel: 'user/b.txt.__deleted__' },
    null
  ] });
  assert.strictEqual(out.length, 2);
  assert.deepStrictEqual(out[0], { rel: 'user/a.txt.__deleted__', name: 'a.txt', volume: 'default' });
  assert.strictEqual(out[1].name, 'user/b.txt.__deleted__');
});

test('normalizeEntries：坏载荷返回空数组', () => {
  assert.deepStrictEqual(m.normalizeEntries(null), []);
  assert.deepStrictEqual(m.normalizeEntries({ entries: 'x' }), []);
});

test('volumeOptions：首次出现序去重、忽略空卷', () => {
  const opts = m.volumeOptions([
    { volume: 'main' }, { volume: '' }, { volume: 'v2' }, { volume: 'main' }, null
  ]);
  assert.deepStrictEqual(opts, ['main', 'v2']);
  assert.deepStrictEqual(m.volumeOptions(null), []);
});

test('filterByVolume：空 = 全部，非空按卷过滤', () => {
  const list = [{ volume: 'main' }, { volume: 'v2' }, { volume: '' }];
  assert.strictEqual(m.filterByVolume(list, '').length, 3);
  assert.deepStrictEqual(m.filterByVolume(list, 'v2'), [{ volume: 'v2' }]);
  assert.deepStrictEqual(m.filterByVolume(list, 'nope'), []);
  assert.deepStrictEqual(m.filterByVolume(null, 'v2'), []);
});

test('formatRetention：Go duration → 人类可读；坏值返回空（不编造天数）', () => {
  assert.strictEqual(m.formatRetention('168h0m0s'), '7 天');
  assert.strictEqual(m.formatRetention('24h0m0s'), '1 天');
  assert.strictEqual(m.formatRetention('36h0m0s'), '36 小时');
  assert.strictEqual(m.formatRetention('90m0s'), '90 分钟');
  assert.strictEqual(m.formatRetention('45s'), '45 秒');
  // 坏/空/零/负数 ⇒ 空串（调用方保留通用文案）。
  assert.strictEqual(m.formatRetention(''), '');
  assert.strictEqual(m.formatRetention(null), '');
  assert.strictEqual(m.formatRetention('0s'), '');
  assert.strictEqual(m.formatRetention('-1h0m0s'), '');
  assert.strictEqual(m.formatRetention('30 days'), '');
});
