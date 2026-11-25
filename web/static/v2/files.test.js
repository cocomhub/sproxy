// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./files.js');

test('normalizeFiles：目录在前、自然排序、过滤坏条目', () => {
  const out = m.normalizeFiles({
    files: [
      { name: 'b10.mp4', size: 1, is_dir: false, mod_time: 0 },
      { name: 'a.mp4', size: 2, is_dir: false },
      { name: 'zz/', size: 0, is_dir: true },
      { name: 'aa/', is_dir: true },
      { name: '', size: 1 },
      { size: 3 }
    ]
  });
  assert.deepStrictEqual(out.map((x) => x.name), ['aa/', 'zz/', 'a.mp4', 'b10.mp4']);
  assert.strictEqual(out[2].size, 2);
});

test('normalizeFiles：坏 payload 返回空数组', () => {
  assert.deepStrictEqual(m.normalizeFiles(null), []);
  assert.deepStrictEqual(m.normalizeFiles({ files: 'x' }), []);
});

test('crumbsFor：根与多级累计路径', () => {
  assert.deepStrictEqual(m.crumbsFor(''), [{ label: '全部卷', path: '' }]);
  assert.deepStrictEqual(m.crumbsFor('/jav/NHDTB/'), [
    { label: '全部卷', path: '' },
    { label: 'jav', path: 'jav' },
    { label: 'NHDTB', path: 'jav/NHDTB' }
  ]);
});

test('joinSubdir/parentSubdir：拒绝 ..、处理空段', () => {
  assert.strictEqual(m.joinSubdir('', 'jav'), 'jav');
  assert.strictEqual(m.joinSubdir('jav', 'NHDTB'), 'jav/NHDTB');
  assert.strictEqual(m.joinSubdir('jav', '../etc'), 'jav/etc');
  assert.strictEqual(m.joinSubdir('jav/NHDTB', './x'), 'jav/NHDTB/x');
  assert.strictEqual(m.parentSubdir('jav/NHDTB'), 'jav');
  assert.strictEqual(m.parentSubdir('jav'), '');
  assert.strictEqual(m.parentSubdir(''), '');
});

test('resolveRel：搜索态行名已是完整相对路径，不得再拼 subdir', () => {
  const f = require('./files.js');
  // 目录浏览：subdir + 基名
  assert.strictEqual(f.resolveRel('v2demo', 'e2e.txt', false), 'v2demo/e2e.txt');
  // 搜索态：行名是 v2demo/e2e.txt（服务端返回完整相对路径）
  assert.strictEqual(f.resolveRel('v2demo', 'v2demo/e2e.txt', true), 'v2demo/e2e.txt');
  // 搜索态且停在根
  assert.strictEqual(f.resolveRel('', 'v2demo/e2e.txt', true), 'v2demo/e2e.txt');
  // 目录浏览、根目录
  assert.strictEqual(f.resolveRel('', 'a.txt', false), 'a.txt');
});
