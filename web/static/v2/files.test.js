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

test('parseListMeta：total 权威 / 无 total 时按 limit 推断 / 边界', () => {
  // 服务端给 total：以 total 为准（即使本页没满）。
  assert.deepStrictEqual(
    m.parseListMeta({ files: new Array(200).fill({ name: 'x' }), total: 250, offset: 0, limit: 200 }),
    { count: 200, offset: 0, limit: 200, total: 250, hasTotal: true, hasMore: true }
  );
  // 末页：offset + count == total ⇒ 没有更多。
  assert.strictEqual(
    m.parseListMeta({ files: new Array(50).fill({ name: 'x' }), total: 250, offset: 200, limit: 200 }).hasMore,
    false
  );
  // 没有 total（旧搜索契约）：返回条数 == limit ⇒ 可能还有更多。
  const noTotal = m.parseListMeta({ files: new Array(200).fill({ name: 'x' }), limit: 200 });
  assert.strictEqual(noTotal.hasTotal, false);
  assert.strictEqual(noTotal.hasMore, true);
  assert.strictEqual(m.parseListMeta({ files: new Array(199).fill({ name: 'x' }), limit: 200 }).hasMore, false);
  // 空/坏数据。
  assert.deepStrictEqual(m.parseListMeta(null), { count: 0, offset: 0, limit: 0, total: -1, hasTotal: false, hasMore: false });
});

test('nextSort：同列翻转、换列重置、非法字段回落 name', () => {
  assert.deepStrictEqual(m.nextSort({ sort: 'name', order: 'asc' }, 'name'), { sort: 'name', order: 'desc' });
  assert.deepStrictEqual(m.nextSort({ sort: 'name', order: 'desc' }, 'name'), { sort: 'name', order: 'asc' });
  assert.deepStrictEqual(m.nextSort({ sort: 'name', order: 'asc' }, 'size'), { sort: 'size', order: 'asc' });
  assert.deepStrictEqual(m.nextSort({}, 'bogus'), { sort: 'name', order: 'asc' });
  assert.deepStrictEqual(m.nextSort(null, 'time'), { sort: 'time', order: 'asc' });
});

test('sortMark：仅当前列显示箭头', () => {
  assert.strictEqual(m.sortMark({ sort: 'size', order: 'desc' }, 'size'), '↓');
  assert.strictEqual(m.sortMark({ sort: 'size', order: 'asc' }, 'size'), '↑');
  assert.strictEqual(m.sortMark({ sort: 'size', order: 'asc' }, 'time'), '');
  assert.strictEqual(m.sortMark(null, 'name'), '');
});

test('neighborFile：跳过目录、边界返回空', () => {
  const files = [
    { name: 'a.mp4' }, { name: 'dir/', isDir: true }, { name: 'b.mp4' }, { name: 'c.mp4' }
  ];
  assert.strictEqual(m.neighborFile(files, 'a.mp4', 1), 'b.mp4');   // 中间目录被跳过
  assert.strictEqual(m.neighborFile(files, 'b.mp4', 1), 'c.mp4');
  assert.strictEqual(m.neighborFile(files, 'b.mp4', -1), 'a.mp4');
  assert.strictEqual(m.neighborFile(files, 'c.mp4', 1), '');        // 末尾
  assert.strictEqual(m.neighborFile(files, 'a.mp4', -1), '');       // 开头
  assert.strictEqual(m.neighborFile(files, 'missing', 1), '');
  assert.strictEqual(m.neighborFile(null, 'a.mp4', 1), '');
  // 只有目录时没有可预览邻居。
  assert.strictEqual(m.neighborFile([{ name: 'x/', isDir: true }], 'x/', 1), '');
});

test('listURL：分页/排序/卷过滤/搜索各自拼对', () => {
  assert.strictEqual(m.listURL({ subdir: '' }), '/api/files');
  assert.strictEqual(m.listURL({}), '/api/files');
  assert.strictEqual(
    m.listURL({ subdir: 'a/b', volume: 'v1', sort: 'size', order: 'desc', limit: 200, offset: 400 }),
    '/api/files?subdir=a%2Fb&volume=v1&sort=size&order=desc&limit=200&offset=400'
  );
  // 未排序时不带 order（避免服务端无意义的 order 参数）。
  assert.strictEqual(m.listURL({ subdir: '', sort: '', order: 'desc' }), '/api/files');
  // 搜索走独立端点，仍支持分页。
  assert.strictEqual(m.listURL({ query: 'x y', limit: 200, offset: 200 }), '/api/files/search?q=x%20y&limit=200&offset=200');
});
