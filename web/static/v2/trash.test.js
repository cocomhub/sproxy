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
