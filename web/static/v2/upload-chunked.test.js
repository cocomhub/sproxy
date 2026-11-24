// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./upload-chunked.js');

test('calcChunkSize：4MiB 起、按 512 倍增长、封顶 64MiB', () => {
  assert.strictEqual(m.calcChunkSize(1 * 1024 * 1024), 4 * 1024 * 1024);
  assert.strictEqual(m.calcChunkSize(8 * 1024 * 1024), 4 * 1024 * 1024);
  assert.strictEqual(m.calcChunkSize(1024 ** 3), 4 * 1024 * 1024); // 4MiB*512 = 2GiB > 1GiB
  assert.strictEqual(m.calcChunkSize(3 * 1024 ** 4), 64 * 1024 * 1024);
});

test('buildChunkIndices：缺失列表优先、否则全量', () => {
  assert.deepStrictEqual(m.buildChunkIndices(3, [1]), [1]);
  assert.deepStrictEqual(m.buildChunkIndices(3, null), [0, 1, 2]);
  assert.deepStrictEqual(m.buildChunkIndices(0, null), []);
});

test('uploadIdFor：确定性 + 前 32 hex', async () => {
  const fake = (s) => Promise.resolve('abcdef0123456789abcdef0123456789ffff');
  assert.strictEqual(await m.uploadIdFor('a.mp4', 10, 1700000000000, 'chk', fake), 'abcdef0123456789abcdef0123456789');
  let seen = '';
  await m.uploadIdFor('a.mp4', 10, 1700000000000, 'chk', (s) => { seen = s; return Promise.resolve('x'); });
  assert.strictEqual(seen, 'a.mp4|10|1700000000000|chk');
});

test('SIMPLE_LIMIT：8MiB 边界', () => {
  assert.strictEqual(m.SIMPLE_LIMIT, 8 * 1024 * 1024);
});
