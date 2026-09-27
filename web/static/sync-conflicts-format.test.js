// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// sync-conflicts-format.test.js node --test 单测（B3 同步冲突渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { syncConflictsHtml, conflictResolveQuery } = require('./sync-conflicts-format.js');

test('syncConflictsHtml 空列表空提示', () => {
  assert.ok(syncConflictsHtml({ conflicts: [] }).includes('暂无未解决冲突'));
});

test('syncConflictsHtml 渲染冲突行 + resolve 按钮', () => {
  const html = syncConflictsHtml({ conflicts: [{ id: 'c1', path: 'a/b.txt', hunk_count: 2, ours: ['x'], theirs: ['y'] }] });
  assert.ok(html.includes('a/b.txt'));
  assert.ok(html.includes('conflict-ours-btn'));
  assert.ok(html.includes('conflict-theirs-btn'));
  assert.ok(html.includes('data-id="c1"'));
  assert.ok(html.includes('x'));
  assert.ok(html.includes('y'));
});

test('syncConflictsHtml 路径/内容转义', () => {
  const html = syncConflictsHtml({ conflicts: [{ id: 'c"1', path: 'p<q', ours: ['<script>'] }] });
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(html.includes('c&quot;1'));
});

test('conflictResolveQuery URL 编码 id', () => {
  assert.strictEqual(conflictResolveQuery('c 1', 'ours'), '/api/sync/conflicts/c%201/resolve');
});
