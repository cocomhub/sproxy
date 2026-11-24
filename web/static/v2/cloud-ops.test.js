// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./cloud-ops.js');

test('parseTSV：识别表头、保留仅有 url 的行、跳过空行与注释', () => {
  const text = [
    '# exported',
    'id\tstatus\tfilename\ttotal_size\turl\tupdated_at',
    'task-1\tfailed\ta.bin\t12\thttp://x/a\t2026-01-01',
    '\t\t\t0\thttp://127.0.0.1:1/nope2.bin\t',
    '\t\t\t\t\t'
  ].join('\n');
  const rows = m.parseTSV(text);
  assert.strictEqual(rows.length, 2);
  assert.strictEqual(rows[0].id, 'task-1');
  assert.strictEqual(rows[0].url, 'http://x/a');
  assert.strictEqual(rows[1].id, '');
  assert.strictEqual(rows[1].url, 'http://127.0.0.1:1/nope2.bin');
});

test('parseTSV：无表头时按导出列序解析', () => {
  const rows = m.parseTSV('t2\tcompleted\tb.bin\t9\thttp://y/b\t');
  assert.strictEqual(rows.length, 1);
  assert.strictEqual(rows[0].status, 'completed');
  assert.deepStrictEqual(m.parseTSV(null), []);
});
