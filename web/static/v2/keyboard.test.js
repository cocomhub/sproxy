// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./keyboard.js');

test('filterCommands：空查询返回全部、按标签/关键字匹配、无匹配返回空', () => {
  const items = m.commandItems();
  assert.strictEqual(m.filterCommands(items, '').length, items.length);
  const cfg = m.filterCommands(items, '配置');
  assert.ok(cfg.length >= 1 && cfg[0].id === 'cs-config');
  assert.strictEqual(m.filterCommands(items, 'console 控制台').length, items.filter((x) => (x.label + ' ' + x.keys).toLowerCase().includes('console 控制台')).length);
  assert.deepStrictEqual(m.filterCommands(items, '不存在的命令'), []);
});

test('nextIndex：循环前进/后退、空列表为 0', () => {
  assert.strictEqual(m.nextIndex(0, 3, 1), 1);
  assert.strictEqual(m.nextIndex(2, 3, 1), 0);
  assert.strictEqual(m.nextIndex(0, 3, -1), 2);
  assert.strictEqual(m.nextIndex(0, 0, 1), 0);
});

test('clampIndex：越界收敛到边界', () => {
  assert.strictEqual(m.clampIndex(-3, 4), 0);
  assert.strictEqual(m.clampIndex(9, 4), 3);
  assert.strictEqual(m.clampIndex(0, 0), 0);
});
