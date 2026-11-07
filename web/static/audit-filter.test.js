// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-filter.test.js — S4 审计筛选纯函数的单元测试（node --test，经 make web-test 执行）。

const test = require('node:test');
const assert = require('node:assert');
const f = require('./audit-filter.js');

const EVENTS = [
  { action: 'upload', owner: 'alice', result: 'success', kind: 'event' },
  { action: 'download', owner: 'alice', result: 'success', kind: 'event' },
  { action: 'delete', owner: 'bob', result: 'denied', kind: 'event' },
  { action: 'quota.wait', owner: 'bob', result: 'success', kind: 'state' },
];

test('空条件返回全部且保序、不修改入参', function () {
  const before = EVENTS.map(function (e) { return e.action; });
  const out = f.auditFilterRows(EVENTS, f.auditFilterEmpty());
  assert.strictEqual(out.length, 4);
  assert.deepStrictEqual(out.map(function (e) { return e.action; }), before);
  assert.deepStrictEqual(EVENTS.map(function (e) { return e.action; }), before);
});

test('action 为不区分大小写的子串匹配', function () {
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { action: 'LOAD' }).map(function (e) { return e.action; }),
    ['upload', 'download'],
  );
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { action: ' quota. ' }).map(function (e) { return e.action; }),
    ['quota.wait'],
  );
});

test('result / kind 为精确匹配（大小写不敏感但非子串）', function () {
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { result: 'denied' }).map(function (e) { return e.action; }),
    ['delete'],
  );
  // 子串不得命中（精确匹配语义）
  assert.deepStrictEqual(f.auditFilterRows(EVENTS, { result: 'deni' }), []);
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { kind: 'state' }).map(function (e) { return e.action; }),
    ['quota.wait'],
  );
});

test('owner 为子串匹配', function () {
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { owner: 'ali' }).map(function (e) { return e.action; }),
    ['upload', 'download'],
  );
});

test('多维度同时生效（AND）', function () {
  assert.deepStrictEqual(
    f.auditFilterRows(EVENTS, { owner: 'bob', result: 'success' }).map(function (e) { return e.action; }),
    ['quota.wait'],
  );
  assert.deepStrictEqual(f.auditFilterRows(EVENTS, { owner: 'alice', result: 'denied' }), []);
});

test('isActive 判定与空值处理', function () {
  assert.strictEqual(f.auditFilterIsActive(f.auditFilterEmpty()), false);
  assert.strictEqual(f.auditFilterIsActive({ action: 'u' }), true);
  assert.strictEqual(f.auditFilterIsActive({ action: '   ' }), false);
  assert.strictEqual(f.auditFilterIsActive(null), false);
  // 入参缺失/非数组不得抛错
  assert.deepStrictEqual(f.auditFilterRows(null, null), []);
  assert.deepStrictEqual(f.auditFilterRows([null, undefined], {}), []);
});

test('summary 文案', function () {
  assert.strictEqual(f.auditFilterSummary(200, 200), '共 200 条');
  assert.strictEqual(f.auditFilterSummary(200, 3), '显示 3 / 200 条');
  assert.strictEqual(f.auditFilterSummary(0, 0), '共 0 条');
  assert.strictEqual(f.auditFilterSummary(undefined, undefined), '共 0 条');
});
