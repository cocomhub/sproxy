// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// credentials-format.test.js node --test 单测（B2 凭据管理渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { credentialsTableHtml, credentialsFormHtml, credentialsPanelHtml, credAddBody } = require('./credentials-format.js');

test('credentialsTableHtml 空列表显示空提示', () => {
  const html = credentialsTableHtml({ ak: [] });
  assert.ok(html.includes('暂无凭据'));
});

test('credentialsTableHtml 渲染 AK 行 + 删除按钮', () => {
  const html = credentialsTableHtml({ ak: [{ ak: 'ak-1', owner: 'o1', sk_count: 3, alive_sk: 2 }] });
  assert.ok(html.includes('ak-1'));
  assert.ok(html.includes('o1'));
  assert.ok(html.includes('cred-delete-btn'));
  assert.ok(html.includes('data-ak="ak-1"'));
});

test('credentialsTableHtml AK 转义', () => {
  const html = credentialsTableHtml({ ak: [{ ak: 'a"k<1' }] });
  assert.ok(html.includes('&quot;'));
  assert.ok(!html.includes('a"k<1'));
});

test('credentialsFormHtml 含新增表单字段', () => {
  const html = credentialsFormHtml();
  assert.ok(html.includes('cred-ak'));
  assert.ok(html.includes('cred-owner'));
  assert.ok(html.includes('cred-add-btn'));
  assert.ok(html.includes('新增凭据'));
});

test('credentialsPanelHtml 组合列表 + 表单', () => {
  const html = credentialsPanelHtml({ ak: [{ ak: 'x' }] });
  assert.ok(html.includes('x'));
  assert.ok(html.includes('cred-add-btn'));
});

test('credAddBody role user 省略', () => {
  assert.deepStrictEqual(credAddBody('ak1', 'o1', 'user'), { ak: 'ak1', owner: 'o1' });
});

test('credAddBody role node 保留', () => {
  assert.deepStrictEqual(credAddBody('ak1', 'o1', 'node'), { ak: 'ak1', owner: 'o1', role: 'node' });
});
