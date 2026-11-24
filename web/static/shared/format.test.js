// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./format.js');

test('formatSize：0/边界/单位/非法', () => {
  assert.strictEqual(m.formatSize(0), '0 B');
  assert.strictEqual(m.formatSize(1023), '1023 B');
  assert.strictEqual(m.formatSize(1024), '1.00 KB');
  assert.strictEqual(m.formatSize(1536), '1.50 KB');
  assert.strictEqual(m.formatSize(5 * 1024 ** 3), '5.00 GB');
  assert.strictEqual(m.formatSize(2 * 1024 ** 4), '2.00 TB');
  assert.strictEqual(m.formatSize(-1), '—');
  assert.strictEqual(m.formatSize(null), '—');
  assert.strictEqual(m.formatSize('x'), '—');
});

test('formatTime：UnixNano 与毫秒都识别，0/非法为 —', () => {
  assert.strictEqual(m.formatTime(0), '—');
  assert.strictEqual(m.formatTime(NaN), '—');
  const d = new Date(2026, 10, 24, 9, 5, 0);
  assert.strictEqual(m.formatTime(d.getTime() * 1e6), '11-24 09:05');
  assert.strictEqual(m.formatTime(d.getTime()), '11-24 09:05');
});
