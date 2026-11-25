// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./files-ops.js');

test('validateName：空/点/非法字符/超长', () => {
  assert.strictEqual(m.validateName('').ok, false);
  assert.strictEqual(m.validateName('   ').ok, false);
  assert.strictEqual(m.validateName('.').ok, false);
  assert.strictEqual(m.validateName('..').ok, false);
  assert.strictEqual(m.validateName('a/b').ok, false);
  assert.strictEqual(m.validateName('a\b').ok, false);
  assert.strictEqual(m.validateName('a:b').ok, false);
  assert.strictEqual(m.validateName('a\u0001b').ok, false);
  assert.strictEqual(m.validateName('x'.repeat(256)).ok, false);
});

test('validateName：合法名称 trim 后返回', () => {
  assert.deepStrictEqual(m.validateName('  目录 A  '), { ok: true, name: '目录 A' });
  assert.deepStrictEqual(m.validateName('EXAMPLE-001.mp4'), { ok: true, name: 'EXAMPLE-001.mp4' });
});

test('uploadHeaders：与服务端 /upload 契约一致', () => {
  const h = m.uploadHeaders('jav/a.mp4', 'abc123', 1700000000000);
  assert.strictEqual(h['X-File-Checksum'], 'abc123');
  assert.strictEqual(h['X-File-Path'], 'jav/a.mp4');
  assert.strictEqual(h['X-File-MTime'], String(1700000000000 * 1e6));
});

test('checksumHeader：缺失时传空串（目录）', () => {
  assert.deepStrictEqual(m.checksumHeader('abc'), { 'X-File-Checksum': 'abc' });
  assert.deepStrictEqual(m.checksumHeader(''), { 'X-File-Checksum': '' });
  assert.deepStrictEqual(m.checksumHeader(undefined), { 'X-File-Checksum': '' });
});

test('selectedPaths：去重保序、过滤空值', () => {
  assert.deepStrictEqual(m.selectedPaths(['b', '', 'a', 'b', null]), ['b', 'a']);
  assert.deepStrictEqual(m.selectedPaths(null), []);
});

test('validateRelativePath：允许 a/b/new、拒绝 .. 与空段', () => {
  const m = require('./files-ops.js');
  assert.deepStrictEqual(m.validateRelativePath('a'), { ok: true, name: 'a' });
  assert.deepStrictEqual(m.validateRelativePath('a/b/c'), { ok: true, name: 'a/b/c' });
  assert.deepStrictEqual(m.validateRelativePath('/a/b'), { ok: true, name: 'a/b' });
  assert.strictEqual(m.validateRelativePath('../x').ok, false);
  assert.strictEqual(m.validateRelativePath('a//b').ok, false);
  assert.strictEqual(m.validateRelativePath('a/./b').ok, false);
  assert.strictEqual(m.validateRelativePath('').ok, false);
  assert.strictEqual(m.validateRelativePath('a/b:c').ok, false);
});
