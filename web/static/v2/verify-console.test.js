// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./verify-console.js');

test('normalizeReport：缺省归零、非数组字段归一为空数组', () => {
  const r = m.normalizeReport(null);
  assert.deepStrictEqual(r, { total: 0, ok: 0, mismatched: [], missing: [], errors: [], skipped: 0, concurrent: false });
  const r2 = m.normalizeReport({ total: 3, ok: 2, missing: ['a'], errors: 'x', concurrent: true });
  assert.strictEqual(r2.total, 3);
  assert.deepStrictEqual(r2.missing, ['a']);
  assert.deepStrictEqual(r2.errors, []);
  assert.strictEqual(r2.concurrent, true);
});
