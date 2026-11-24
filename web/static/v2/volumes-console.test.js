// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./volumes-console.js');

test('normalizeVolumes：字段归一 + 坏条目过滤', () => {
  const out = m.normalizeVolumes({ volumes: [
    { name: 'default', type: 'local', mode: 'allow', capacity: 100, usage: 30, state: 'healthy' },
    { name: 'ext', usage: 'x' }, null
  ] });
  assert.strictEqual(out.length, 2);
  assert.deepStrictEqual(out[0], { name: 'default', type: 'local', mode: 'allow', capacity: 100, usage: 30, state: 'healthy' });
  assert.deepStrictEqual(out[1], { name: 'ext', type: 'local', mode: '', capacity: 0, usage: 0, state: 'healthy' });
});

test('normalizeVolumes：坏载荷返回空数组', () => {
  assert.deepStrictEqual(m.normalizeVolumes(null), []);
  assert.deepStrictEqual(m.normalizeVolumes({ volumes: 'x' }), []);
});

test('normalizeUserVolumes：剔除无名条目', () => {
  const out = m.normalizeUserVolumes({ volumes: [{ name: 'u1', type: 'baidupcs', capacity: 0, usage: 5 }, {}] });
  assert.deepStrictEqual(out, [{ name: 'u1', type: 'baidupcs', capacity: 0, usage: 5 }]);
  assert.deepStrictEqual(m.normalizeUserVolumes(null), []);
});
