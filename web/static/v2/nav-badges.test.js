// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./nav-badges.js');

test('badgeText：未知隐藏 / 0 显示 / 上限折叠', () => {
  assert.strictEqual(m.badgeText(null), '');
  assert.strictEqual(m.badgeText(undefined), '');
  assert.strictEqual(m.badgeText(-1), '');
  assert.strictEqual(m.badgeText('x'), '');
  assert.strictEqual(m.badgeText(0), '0');
  assert.strictEqual(m.badgeText(7), '7');
  assert.strictEqual(m.badgeText(99), '99');
  assert.strictEqual(m.badgeText(100), '99+');
  assert.strictEqual(m.badgeText(12345), '99+');
});

test('accountModel：匿名与已登录', () => {
  const anon = m.accountModel(null);
  assert.strictEqual(anon.anon, true);
  assert.strictEqual(anon.name, '匿名');
  assert.strictEqual(anon.initial, '?');

  const c = m.accountModel({ ak: 'EXAMPLE-access-key-01', entryID: 'k1' });
  assert.strictEqual(c.anon, false);
  assert.strictEqual(c.name, 'EXAMPL…');
  assert.strictEqual(c.initial, 'EX');
  assert.ok(c.sub.indexOf('k1') !== -1);

  // 短 AK 不截断；无 entryID 时副标题不回显空 Key。
  const short = m.accountModel({ ak: 'abc123', entryID: '' });
  assert.strictEqual(short.name, 'abc123');
  assert.strictEqual(short.sub, '已登录');
});
