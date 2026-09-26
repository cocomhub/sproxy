// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// volume-ops.test.js node --test 单测（B1 卷操作纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert');
const { volumeOpsBarHtml, volumeOpQuery, volumeOpLabel, escapeAttr } = require('./volume-ops.js');

test('volumeOpsBarHtml 少于两卷显示提示', () => {
  const html = volumeOpsBarHtml([{ name: 'a' }]);
  assert.ok(html.includes('至少两个卷'));
});

test('volumeOpsBarHtml 多卷含复制/移动/再平衡按钮', () => {
  const html = volumeOpsBarHtml([{ name: 'v1' }, { name: 'v2' }]);
  assert.ok(html.includes('vol-copy-btn'));
  assert.ok(html.includes('vol-move-btn'));
  assert.ok(html.includes('vol-rebalance-btn'));
  assert.ok(html.includes('v1'));
  assert.ok(html.includes('v2'));
  assert.ok(html.includes('data-vol="v1"'));
});

test('volumeOpsBarHtml 卷名 HTML 转义', () => {
  const html = volumeOpsBarHtml([{ name: 'a"b<c' }, { name: 'd' }]);
  assert.ok(html.includes('&quot;'));
  assert.ok(html.includes('&lt;'));
});

test('volumeOpQuery copy 带 filename', () => {
  const q = volumeOpQuery('copy', 'src', 'dst', 'f x.txt', 0);
  assert.strictEqual(q, '/api/volumes/copy?from_volume=src&to_volume=dst&filename=f%20x.txt');
});

test('volumeOpQuery move 带 filename', () => {
  const q = volumeOpQuery('move', 'src', 'dst', 'y.txt', 0);
  assert.strictEqual(q, '/api/volumes/move?from_volume=src&to_volume=dst&filename=y.txt');
});

test('volumeOpQuery rebalance 带 max_bytes', () => {
  const q = volumeOpQuery('rebalance', 'src', 'dst', '', 1073741824);
  assert.strictEqual(q, '/api/volumes/rebalance?from_volume=src&to_volume=dst&max_bytes=1073741824');
});

test('volumeOpQuery rebalance 无 max_bytes 不加参数', () => {
  const q = volumeOpQuery('rebalance', 'src', 'dst', '', 0);
  assert.strictEqual(q, '/api/volumes/rebalance?from_volume=src&to_volume=dst');
});

test('volumeOpLabel 中文标签', () => {
  assert.strictEqual(volumeOpLabel('copy'), '复制');
  assert.strictEqual(volumeOpLabel('move'), '移动');
  assert.strictEqual(volumeOpLabel('rebalance'), '再平衡');
});

test('escapeAttr 转义引号与尖括号', () => {
  assert.strictEqual(escapeAttr('a"b<c>d&e'), 'a&quot;b&lt;c&gt;d&amp;e');
});
