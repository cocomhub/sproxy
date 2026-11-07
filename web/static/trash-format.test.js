// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { trashTableHtml, trashVolumeOptions, trashFilterEntries } = require('./trash-format.js');

test('trashTableHtml 渲染条目 + 恢复按钮', () => {
  const html = trashTableHtml([
    { name: 'a.txt', trash_rel: 'trash/a.txt.__deleted__1' },
  ]);
  assert.ok(html.includes('a.txt'));
  assert.ok(html.includes('trash-restore-btn'));
  assert.ok(html.includes('trash/a.txt.__deleted__1'));
});

test('trashTableHtml 空 → 提示', () => {
  assert.ok(trashTableHtml([]).includes('回收站为空'));
});

test('trashTableHtml 汇总视图：卷列 + 卷筛选 + 两个清空动作', () => {
  const html = trashTableHtml([
    { name: 'a.txt', trash_rel: 'trash/a', volume: 'default' },
    { name: 'b.txt', trash_rel: 'trash/b', volume: 'v2' },
  ], '');
  assert.ok(html.includes('>卷<'), '应含卷列表头');
  assert.ok(html.includes('default') && html.includes('v2'), '卷列应显示各条所在卷');
  assert.ok(html.includes('id="trash-volume-filter"'), '应有卷筛选控件');
  assert.ok(html.includes('id="trash-empty-vol-btn"'), '应有「清空该卷」');
  assert.ok(html.includes('id="trash-empty-all-btn"'), '应有「清空全部卷」');
  assert.ok(html.includes('a.txt') && html.includes('b.txt'), '缺省应展示全部卷条目');
});

test('trashTableHtml 按卷筛选只渲染该卷条目（并标记选中）', () => {
  const html = trashTableHtml([
    { name: 'a.txt', trash_rel: 'trash/a', volume: 'default' },
    { name: 'b.txt', trash_rel: 'trash/b', volume: 'v2' },
  ], 'v2');
  assert.ok(html.includes('b.txt'), '应含所选卷条目');
  assert.ok(!html.includes('a.txt'), '不应含其它卷条目');
  assert.ok(html.includes('<option value="v2" selected>'), '所选卷应标记 selected');
});

test('trashTableHtml 所选卷无条目 → 该卷空提示（仍保留动作栏）', () => {
  const html = trashTableHtml([{ name: 'a.txt', trash_rel: 'trash/a', volume: 'default' }], 'v9');
  assert.ok(html.includes('该卷回收站为空'));
  assert.ok(html.includes('id="trash-empty-all-btn"'), '动作栏不应随筛选消失');
});

test('trashVolumeOptions 去重并保持出现序；trashFilterEntries 空卷=全部', () => {
  const entries = [
    { volume: 'v2' }, { volume: 'default' }, { volume: 'v2' }, { volume: '' },
  ];
  assert.deepStrictEqual(trashVolumeOptions(entries), ['v2', 'default']);
  assert.strictEqual(trashFilterEntries(entries, '').length, 4);
  assert.strictEqual(trashFilterEntries(entries, 'v2').length, 2);
  assert.strictEqual(trashFilterEntries(null, 'v2').length, 0);
});
