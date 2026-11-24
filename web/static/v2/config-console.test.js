// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./config-console.js');

test('diffConfig：只保留变化的可写字段', () => {
  const orig = { log_level: 'info', max_upload_bytes: 100, web_tunnel: false, addr: ':1' };
  assert.deepStrictEqual(m.diffConfig(orig, { log_level: 'info', max_upload_bytes: 100, web_tunnel: false }), {});
  assert.deepStrictEqual(m.diffConfig(orig, { log_level: 'debug', max_upload_bytes: 200, web_tunnel: true }),
    { log_level: 'debug', max_upload_bytes: 200, web_tunnel: true });
  assert.deepStrictEqual(m.diffConfig(orig, { addr: ':2' }), {});
});

test('diffConfig：缺省原值视为变化', () => {
  assert.deepStrictEqual(m.diffConfig(null, { log_format: 'json' }), { log_format: 'json' });
});

test('parseBytes：仅接受非负整数字节串', () => {
  assert.strictEqual(m.parseBytes('1048576'), 1048576);
  assert.strictEqual(m.parseBytes(' 0 '), 0);
  assert.strictEqual(m.parseBytes(''), null);
  assert.strictEqual(m.parseBytes('-1'), null);
  assert.strictEqual(m.parseBytes('1GiB'), null);
  assert.strictEqual(m.parseBytes('1.5'), null);
});
