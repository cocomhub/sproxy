// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const vs = require('./volume-settings.js');

test('parseSize：人类可读与纯数字，非法返回 null', () => {
  assert.strictEqual(vs.parseSize('20MiB'), 20 * 1024 * 1024);
  assert.strictEqual(vs.parseSize('20971520'), 20971520);
  assert.strictEqual(vs.parseSize('1GB'), 1000 * 1000 * 1000);
  assert.strictEqual(vs.parseSize(4096), 4096);
  assert.strictEqual(vs.parseSize('abc'), null);
  assert.strictEqual(vs.parseSize(''), null);
  assert.strictEqual(vs.parseSize(null), null);
});

test('formatSize：字节 → 可读单位', () => {
  assert.strictEqual(vs.formatSize(20 * 1024 * 1024), '20 MiB');
  assert.strictEqual(vs.formatSize(0), '0');
  assert.strictEqual(vs.formatSize(2048), '2 KiB');
});

test('validateRules：single 缺 max_size 必拒', () => {
  const r = vs.validateRules([{ path_regex: 'x', mode: 'single' }], { hardMax: 64 << 20 });
  assert.strictEqual(r.ok, false);
  assert.match(r.errors[0].msg, /max_size/);
});

test('validateRules：超硬上限与超卷块上限都拒', () => {
  const overHard = vs.validateRules([{ path_regex: 'x', mode: 'single', max_size: (64 << 20) + 1 }], {});
  assert.strictEqual(overHard.ok, false);
  assert.match(overHard.errors[0].msg, /硬上限/);

  const overVol = vs.validateRules([{ path_regex: 'x', mode: 'single', max_size: 1 << 20 }], { volBlockMax: 1024 });
  assert.strictEqual(overVol.ok, false);
  assert.match(overVol.errors[0].msg, /卷块上限/);
});

test('validateRules：合法规则通过（含 random 无需上限）', () => {
  const ok = vs.validateRules([
    { path_regex: '^archive/', mode: 'single', max_size: '20MiB', priority: 100 },
    { path_regex: '\\.jpg$', mode: 'random', priority: 10 }
  ], { volBlockMax: 200 << 20 });
  assert.strictEqual(ok.ok, true, JSON.stringify(ok.errors));
});

test('validateRules：非法正则与条数上限', () => {
  const badRe = vs.validateRules([{ path_regex: '(', mode: 'random' }], {});
  assert.strictEqual(badRe.ok, false);
  assert.match(badRe.errors[0].msg, /无法编译/);

  const many = [];
  for (let i = 0; i <= vs.MAX_RULES; i++) { many.push({ path_regex: 'x' + i, mode: 'random' }); }
  const tooMany = vs.validateRules(many, {});
  assert.strictEqual(tooMany.ok, false);
  assert.match(tooMany.errors[0].msg, /超过上限/);
});

test('sortRules：按 priority 降序且稳定', () => {
  const out = vs.sortRules([
    { path_regex: 'a', priority: 1 },
    { path_regex: 'b', priority: 100 },
    { path_regex: 'c', priority: 100 }
  ]);
  assert.deepStrictEqual(out.map(r => r.path_regex), ['b', 'c', 'a']);
});

test('addRule / removeRule：默认 priority 高于现存，删除越界忽略', () => {
  const base = vs.addRule([]);
  assert.strictEqual(base.length, 1);
  assert.strictEqual(base[0].mode, 'single');
  assert.strictEqual(base[0].priority, 10);

  const next = vs.addRule(base);
  assert.strictEqual(next[1].priority, 20);

  assert.strictEqual(vs.removeRule(next, 0).length, 1);
  assert.strictEqual(vs.removeRule(next, 99).length, 2);
});

test('toPayload：输出人类可读 max_size 且省略零值', () => {
  const p = vs.toPayload([{ path_regex: '^a/', mode: 'single', max_size: 20 * 1024 * 1024, min_size: 0, priority: 5 }]);
  assert.deepStrictEqual(p, {
    rules: [{ path_regex: '^a/', mode: 'single', priority: 5, max_size: '20MiB' }]
  });
});

test('fromValue：后端 value → 行数组（缺字段归一）', () => {
  const rows = vs.fromValue({ version: 3, rules: [{ path_regex: '^a/', mode: 'single', max_size: 1024 }] });
  assert.strictEqual(rows.length, 1);
  assert.deepStrictEqual(rows[0], {
    path_regex: '^a/', min_size: 0, max_size: 1024, mode: 'single', priority: 0, note: ''
  });
  assert.deepStrictEqual(vs.fromValue(null), []);
});
