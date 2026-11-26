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

test('navItems：四个入口与分组（计数改由运行时广播，不再硬编码）', () => {
  const items = m.navItems();
  assert.deepStrictEqual(items.map((x) => x.id), ['files', 'transfer', 'trash', 'console']);
  assert.deepStrictEqual(items.map((x) => x.group), ['工作区', '工作区', '工作区', '系统']);
  // 假数据回归守卫：导航项不得内置计数（曾硬编码 128/3/4，永不更新）。
  items.forEach((it) => assert.strictEqual(it.count, undefined, it.id));
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

test('TITLES 副标题：不得内嵌统计/阈值（假数据回归守卫）', () => {
  // 曾写死「/ 3 个进行中」「/ 保留 30 天」——数字与真实状态无关。副标题只允许静态说明，
  // 计数一律由视图经 v2:subtitle / v2:badge 事件实时写入。
  m.ROUTES.forEach((r) => {
    const sub = m.TITLES[r][1];
    assert.ok(!/\d/.test(sub), r + ' 副标题含数字: ' + sub);
  });
});
