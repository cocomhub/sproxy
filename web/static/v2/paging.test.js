// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./paging.js');

test('pageMeta：有 total 时以 total 为准（首屏/追加/末页）', () => {
  const first = m.pageMeta({ tasks: new Array(100).fill({}), total: 250 }, 'tasks', 0);
  assert.deepStrictEqual(first, { pageCount: 100, loaded: 100, total: 250, hasTotal: true, hasMore: true });

  // 追加一页：loaded 累计。
  const second = m.pageMeta({ tasks: new Array(100).fill({}), total: 250 }, 'tasks', 100);
  assert.strictEqual(second.loaded, 200);
  assert.strictEqual(second.hasMore, true);

  // 末页：全部加载完 ⇒ 不再有更多。
  const last = m.pageMeta({ tasks: new Array(50).fill({}), total: 250 }, 'tasks', 200);
  assert.strictEqual(last.loaded, 250);
  assert.strictEqual(last.hasMore, false);
});

test('pageMeta：无 total ⇒ hasTotal=false 且不臆测 hasMore', () => {
  const meta = m.pageMeta({ groups: new Array(50).fill({}) }, 'groups', 0);
  assert.strictEqual(meta.hasTotal, false);
  assert.strictEqual(meta.total, -1);
  assert.strictEqual(meta.hasMore, false); // 由 inferHasMore 保守判断
  // total 显式 null / '' 也算未返回（Number(null)===0 的坑）。
  assert.strictEqual(m.pageMeta({ tasks: [], total: null }, 'tasks', 0).hasTotal, false);
  assert.strictEqual(m.pageMeta({ tasks: [], total: '' }, 'tasks', 0).hasTotal, false);
  // total=0 是**有**效值（空列表）。
  const zero = m.pageMeta({ tasks: [], total: 0 }, 'tasks', 0);
  assert.strictEqual(zero.hasTotal, true);
  assert.strictEqual(zero.hasMore, false);
});

test('pageMeta：坏载荷与缺字段', () => {
  assert.deepStrictEqual(m.pageMeta(null, 'tasks', 0), { pageCount: 0, loaded: 0, total: -1, hasTotal: false, hasMore: false });
  assert.strictEqual(m.pageMeta({ tasks: 'x' }, 'tasks', 0).pageCount, 0);
  assert.strictEqual(m.pageMeta({}, 'tasks', 5).loaded, 5);
});

test('inferHasMore：满页 ⇒ 可能还有；未满/limit<=0 ⇒ 没有', () => {
  assert.strictEqual(m.inferHasMore(100, 100), true);
  assert.strictEqual(m.inferHasMore(99, 100), false);
  assert.strictEqual(m.inferHasMore(0, 0), false);
  assert.strictEqual(m.inferHasMore(5, 0), false);
});

test('pageURL：不改既有查询串，只追加 offset/limit', () => {
  assert.strictEqual(m.pageURL('/api/cloud/tasks', 0, 100), '/api/cloud/tasks?limit=100');
  assert.strictEqual(m.pageURL('/api/cloud/tasks?status=failed', 100, 100), '/api/cloud/tasks?status=failed&limit=100&offset=100');
  // offset=0 不写（保持首屏 URL 干净）。
  assert.strictEqual(m.pageURL('/api/cloud/groups?status=running', 0, 50), '/api/cloud/groups?status=running&limit=50');
  assert.strictEqual(m.pageURL('/x?', 0, 10), '/x?limit=10');
  assert.strictEqual(m.pageURL('', 0, 0), '');
});

test('loadedText / partial：无 total 不编造总数；未全加载要标注', () => {
  assert.strictEqual(m.loadedText({ loaded: 100, total: 250, hasTotal: true }), '100 / 250 条');
  assert.strictEqual(m.loadedText({ loaded: 100, total: -1, hasTotal: false }), '100 条');
  assert.strictEqual(m.loadedText(null), '0 条');

  assert.strictEqual(m.partial({ hasTotal: true, hasMore: true }), true);
  assert.strictEqual(m.partial({ hasTotal: true, hasMore: false }), false);
  // 无 total ⇒ 必然是不完整视图（计数会低估）。
  assert.strictEqual(m.partial({ hasTotal: false, hasMore: false }), true);
  assert.strictEqual(m.partial(null), true);
});
