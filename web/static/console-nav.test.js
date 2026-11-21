// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const m = require('./console-nav.js');

test('分区 id 合法性与回落（未知 id 回概览，避免空白）', () => {
  assert.ok(m.consoleTabValid('cloud-tasks'));
  assert.ok(m.consoleTabValid('issues'));
  assert.strictEqual(m.consoleTabValid('nope'), false);
  assert.strictEqual(m.consoleHashFor('cloud-tasks'), '#/console/cloud-tasks');
  assert.strictEqual(m.consoleHashFor('nope'), '#/console/stats');
});

test('hash 解析：仅识别控制台深链', () => {
  assert.strictEqual(m.tabFromHash('#/console/audit'), 'audit');
  assert.strictEqual(m.tabFromHash('#/console/bogus'), 'stats'); // 非法分区回落概览
  assert.strictEqual(m.tabFromHash('#/files'), '');
  assert.strictEqual(m.tabFromHash(''), '');
  assert.strictEqual(m.tabFromHash(null), '');
});

test('导航渲染：当前分区高亮 + 计数徽标', () => {
  const html = m.consoleNavHtml('cloud-tasks', { 'cloud-tasks': 3 });
  assert.ok(html.includes('data-console-tab="cloud-tasks"'));
  assert.ok(html.includes('active'));
  assert.ok(html.includes('>3<'));         // 徽标数字
  assert.ok(html.includes('待处理事项'));   // 其余分区仍在导航里
});

test('全屏尺寸：宽 96vw（720–1600 限幅）、高 92vh（≥520）', () => {
  assert.deepStrictEqual(m.consoleLayoutStyle(1280, 800), { width: '1229px', height: '736px' });
  assert.strictEqual(m.consoleLayoutStyle(600, 400).width, '720px');  // 下限
  assert.strictEqual(m.consoleLayoutStyle(4000, 3000).width, '1600px'); // 上限
});
