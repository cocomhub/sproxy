// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./shell.js');

test('parseRoute：合法路由 / 未知回落 / 空值', () => {
  assert.strictEqual(m.parseRoute('#/transfer'), 'transfer');
  assert.strictEqual(m.parseRoute('/trash'), 'trash');
  assert.strictEqual(m.parseRoute('#/console/overview'), 'console');
  assert.strictEqual(m.parseRoute('#/bogus'), 'files');
  assert.strictEqual(m.parseRoute(''), 'files');
  assert.strictEqual(m.parseRoute(null), 'files');
  assert.strictEqual(m.parseRoute('#/'), 'files');
});

test('routeHref：稳定输出且非法值回落', () => {
  assert.strictEqual(m.routeHref('files'), '#/files');
  assert.strictEqual(m.routeHref('nope'), '#/files');
});

test('navItems：四个入口、分组与计数', () => {
  const items = m.navItems();
  assert.deepStrictEqual(items.map((x) => x.id), ['files', 'transfer', 'trash', 'console']);
  assert.deepStrictEqual(items.map((x) => x.group), ['工作区', '工作区', '工作区', '系统']);
  assert.strictEqual(items[0].count, 128);
  assert.strictEqual(items[3].count, undefined);
});

test('resolveTheme：显式优先，否则跟随系统', () => {
  assert.strictEqual(m.resolveTheme('dark', false), 'dark');
  assert.strictEqual(m.resolveTheme('light', true), 'light');
  assert.strictEqual(m.resolveTheme(null, true), 'dark');
  assert.strictEqual(m.resolveTheme('', false), 'light');
  assert.strictEqual(m.resolveTheme('bogus', true), 'dark');
});

test('TITLES：每个路由都有标题与副标题', () => {
  m.ROUTES.forEach((r) => {
    assert.ok(Array.isArray(m.TITLES[r]), r);
    assert.strictEqual(m.TITLES[r].length, 2);
  });
});
