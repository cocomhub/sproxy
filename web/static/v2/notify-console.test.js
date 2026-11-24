// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./notify-console.js');

test('normalizeEntries：兼容大写键（HistoryEntry 无 json tag）与小写键', () => {
  const out = m.normalizeEntries({ entries: [
    { TS: '2026-01-01T00:00:00Z', Action: 'upload', Object: 'a.txt', Channel: 'webhook', Status: 'sent', Detail: 'ok' },
    { ts: '2026-01-02T00:00:00Z', action: 'delete', object: 'b.txt', channel: 'wecom', status: 'failed' },
    null
  ] });
  assert.strictEqual(out.length, 2);
  assert.strictEqual(out[0].action, 'upload');
  assert.strictEqual(out[0].status, 'sent');
  assert.strictEqual(out[1].channel, 'wecom');
  assert.strictEqual(out[1].detail, '');
});

test('normalizeEntries：坏载荷返回空数组', () => {
  assert.deepStrictEqual(m.normalizeEntries(null), []);
  assert.deepStrictEqual(m.normalizeEntries({ entries: 'x' }), []);
});
