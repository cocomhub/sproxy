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

test('consoleHashFor 带参数（省略空值，参数排序稳定）', () => {
  assert.strictEqual(m.consoleHashFor('cloud-tasks'), '#/console/cloud-tasks');
  assert.strictEqual(
    m.consoleHashFor('cloud-tasks', { failed: true, cls: 'throttled', volume: '', q: '' }),
    '#/console/cloud-tasks?cls=throttled&failed=1',
  );
  // 相邻两次构建必须字节一致（可分享、可幂等 replaceState）
  const a = m.consoleHashFor('cloud-tasks', { volume: 'baidu', failed: true });
  const b = m.consoleHashFor('cloud-tasks', { failed: true, volume: 'baidu' });
  assert.strictEqual(a, b);
});

test('consoleStateFromHash 解析 tab + 参数（与 consoleHashFor 对称）', () => {
  const st = m.consoleStateFromHash('#/console/cloud-tasks?failed=1&cls=throttled&volume=baidu&q=abc');
  assert.strictEqual(st.tab, 'cloud-tasks');
  assert.deepStrictEqual(st.params, { failed: true, cls: 'throttled', volume: 'baidu', q: 'abc' });
  // 往返一致
  assert.strictEqual(m.consoleHashFor(st.tab, st.params),
    '#/console/cloud-tasks?cls=throttled&failed=1&q=abc&volume=baidu');
  assert.deepStrictEqual(m.consoleStateFromHash('#/console/issues'), { tab: 'issues', params: {} });
  assert.deepStrictEqual(m.consoleStateFromHash('#/files').tab, '');
});

test('consoleNeighborTab 循环前后；consoleTabByOrdinal 1-based', () => {
  const ids = m.consoleTabIDs();
  assert.strictEqual(m.consoleNeighborTab('stats', 1), ids[1]);
  assert.strictEqual(m.consoleNeighborTab('stats', -1), ids[ids.length - 1]); // 循环到末尾
  assert.strictEqual(m.consoleNeighborTab('unknown', 1), ids[0]);
  assert.strictEqual(m.consoleTabByOrdinal(1), 'stats');
  assert.strictEqual(m.consoleTabByOrdinal(2), 'cloud-tasks');
  assert.strictEqual(m.consoleTabByOrdinal(99), '');
});

test('consoleHelpHtml 列出全部快捷键（与 CONSOLE_SHORTCUTS 单一事实源一致）', () => {
  const html = m.consoleHelpHtml();
  m.CONSOLE_SHORTCUTS.forEach((s) => {
    assert.ok(html.includes(s.key), '缺少快捷键 ' + s.key);
  });
  assert.ok(html.includes('id="console-help"'));
});
