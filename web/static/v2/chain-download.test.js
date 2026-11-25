// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./chain-download.js');

test('parseLines：URL 与 URL<TAB>文件名 混排，忽略空行与注释', () => {
  const rows = m.parseLines([
    '# comment',
    'https://example.com/a.bin',
    '',
    'https://example.com/b.bin\tb-custom.bin',
    '   '
  ].join(String.fromCharCode(10)));
  assert.deepStrictEqual(rows, [
    { url: 'https://example.com/a.bin', filename: '' },
    { url: 'https://example.com/b.bin', filename: 'b-custom.bin' }
  ]);
  assert.deepStrictEqual(m.parseLines(null), []);
});

test('groupProgress：完成度与就绪判定', () => {
  assert.deepStrictEqual(m.groupProgress({ total_tasks: 4, completed: 4 }), { total: 4, done: 4, failed: 0, pct: 100, ready: true });
  const p = m.groupProgress({ total_tasks: 4, completed: 1, failed: 2 });
  assert.strictEqual(p.pct, 25);
  assert.strictEqual(p.ready, false);
  assert.strictEqual(m.groupProgress(null).ready, false);
});

test('defaultArchiveName：chain-<yyyymmdd-hhmmss>.tar.gz', () => {
  const fixed = new Date(2026, 10, 25, 9, 5, 7);
  assert.strictEqual(m.defaultArchiveName(fixed), 'chain-20261125-090507.tar.gz');
});
