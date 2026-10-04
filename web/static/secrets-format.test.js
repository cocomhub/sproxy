// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// secrets-format.test.js node --test 单测（secret 卷管理渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { secretsTableHtml, secretsFormHtml, secretsPanelHtml, secretExportValue, isSecretHex, escHtml } = require('./secrets-format.js');

test('secretsTableHtml 空列表显示空提示', () => {
  const html = secretsTableHtml({ secrets: [] });
  assert.ok(html.includes('暂无 secret'));
});

test('secretsTableHtml 渲染名称行 + 导出/删除按钮', () => {
  const html = secretsTableHtml({ secrets: ['myvault', 'other'] });
  assert.ok(html.includes('myvault'));
  assert.ok(html.includes('other'));
  assert.ok(html.includes('secret-export-btn'));
  assert.ok(html.includes('secret-delete-btn'));
  assert.ok(html.includes('data-name="myvault"'));
});

test('secretsTableHtml 名称转义（防 XSS）', () => {
  const html = secretsTableHtml({ secrets: ['a"b<script>'] });
  assert.ok(html.includes('&quot;'));
  assert.ok(html.includes('&lt;script&gt;'));
  assert.ok(!html.includes('<script>'));
});

test('secretsFormHtml 含创建表单字段', () => {
  const html = secretsFormHtml();
  assert.ok(html.includes('secret-new-name'));
  assert.ok(html.includes('secret-add-btn'));
  assert.ok(html.includes('创建随机 secret'));
});

test('secretsPanelHtml 组合列表 + 表单', () => {
  const html = secretsPanelHtml({ secrets: ['x'] });
  assert.ok(html.includes('x'));
  assert.ok(html.includes('secret-new-name'));
});

test('secretExportValue 提取 value（兼容 origin 字段）', () => {
  assert.strictEqual(secretExportValue({ name: 'v', value: 'abcdef' }), 'abcdef');
  assert.strictEqual(secretExportValue({ name: 'v', value: 'abcdef', origin: 'export' }), 'abcdef');
  assert.strictEqual(secretExportValue({}), '');
});

test('isSecretHex 判定 64 位小写 hex', () => {
  const ok = '0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef';
  assert.ok(isSecretHex(ok));
  assert.ok(!isSecretHex(''));
  assert.ok(!isSecretHex(ok.toUpperCase())); // 只接受小写（与 Go 一致）
  assert.ok(!isSecretHex(ok.slice(0, 40)));
  assert.ok(!isSecretHex('zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz'));
});

test('escHtml 转义特殊字符', () => {
  assert.strictEqual(escHtml('a<b&c"d\''), 'a&lt;b&amp;c&quot;d&#39;');
  assert.strictEqual(escHtml(null), '');
});
