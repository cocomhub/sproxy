// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./audit-console.js');

test('buildQuery：空过滤返回空串、值做转义', () => {
  assert.strictEqual(m.buildQuery({}), '');
  assert.strictEqual(m.buildQuery({ action: '  ' }), '');
  assert.strictEqual(m.buildQuery({ action: 'ai.', actor: 'demo' }), 'action=ai.&actor=demo');
  assert.strictEqual(m.buildQuery({ volume: 'a b&c' }), 'volume=a%20b%26c');
});

test('buildQuery：导出时间键为 after_ts', () => {
  assert.strictEqual(m.buildQuery({ since: '2026-01-02T03:04:05Z' }, 'after_ts'),
    'after_ts=2026-01-02T03%3A04%3A05Z');
});

test('normalizeEvents：过滤坏条目、坏载荷返回空数组', () => {
  assert.deepStrictEqual(m.normalizeEvents(null), []);
  assert.deepStrictEqual(m.normalizeEvents({ events: 'x' }), []);
  const out = m.normalizeEvents({ events: [{ ts: 't1', action: 'a' }, { action: 'b' }, null] });
  assert.strictEqual(out.length, 1);
  assert.strictEqual(out[0].action, 'a');
});
