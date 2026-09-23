// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';

const test = require('node:test');
const assert = require('node:assert');
const { trashTableHtml } = require('./trash-format.js');

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
