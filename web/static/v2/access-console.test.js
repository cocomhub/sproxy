// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./access-console.js');

test('normalizeAKs：字段归一 + 过滤坏条目', () => {
  const out = m.normalizeAKs({ ak: [
    { ak: 'AK1', owner: 'demo', sk_count: 2, alive_sk: 1 },
    { ak: '', owner: '' },
    null
  ] });
  assert.strictEqual(out.length, 2);
  assert.deepStrictEqual(out[0], { ak: 'AK1', owner: 'demo', skCount: 2, aliveSK: 1 });
  assert.deepStrictEqual(out[1], { ak: '', owner: '', skCount: 0, aliveSK: 0 });
});

test('normalizeAKs：坏载荷返回空数组', () => {
  assert.deepStrictEqual(m.normalizeAKs(null), []);
  assert.deepStrictEqual(m.normalizeAKs({ ak: 'x' }), []);
});
