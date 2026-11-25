// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./pending-upload.js');

test('sameFile：基名 + 大小 + mtime 全等才视为同一文件', () => {
  const rec = { base: 'a.bin', size: 10, mtime: 1000 };
  assert.strictEqual(m.sameFile(rec, { name: 'a.bin', size: 10, lastModified: 1000 }), true);
  assert.strictEqual(m.sameFile(rec, { name: 'a.bin', size: 11, lastModified: 1000 }), false);
  assert.strictEqual(m.sameFile(rec, { name: 'b.bin', size: 10, lastModified: 1000 }), false);
  assert.strictEqual(m.sameFile(rec, { name: 'a.bin', size: 10, lastModified: 1001 }), false);
  assert.strictEqual(m.sameFile(null, { name: 'a.bin' }), false);
});
