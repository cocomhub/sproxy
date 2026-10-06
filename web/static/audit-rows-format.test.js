// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// audit-rows-format.test.js node --test 单测（任务审计行渲染纯函数）。

'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const path = require('node:path');

// audit-rows-format.js 引用共享 appRender.formatSize/escHtml（禁自建，见 CLAUDE.md 记录）——
// node 环境下 app-render.js 以 module 形态提供，须先注入全局。
global.appRender = require(path.join(__dirname, 'app-render.js'));

const { auditRowsHtml } = require('./audit-rows-format.js');

test('auditRowsHtml renders type/step/dur + encrypt before-after', () => {
  const html = auditRowsHtml({ rows: [
    { type: 'download', step: 'download', dur_ms: 120, bytes: 2048, bw_bps: 4096 },
    { type: 'encrypt', step: 'encrypt', dur_ms: 30, bytes: 1024, meta: { cipher_bytes: 2048, algorithm: 'shardseal-high' } },
  ]});
  // 类型/步骤/耗时
  assert.match(html, /download/);
  assert.match(html, /120/);
  // 加密行：明文 bytes + 密文 cipher_bytes + algorithm
  assert.match(html, /shardseal-high/);
  assert.match(html, /1024/);
  assert.match(html, /2048/);
});

test('auditRowsHtml empty rows returns placeholder', () => {
  const html = auditRowsHtml({ rows: [] });
  assert.match(html, /暂无审计记录/);
});

test('auditRowsHtml escapes untrusted type/step/detail', () => {
  const html = auditRowsHtml({ rows: [
    { type: '<script>', step: 'a&b', dur_ms: 1, err: '"><img onerror=x>' },
  ] });
  assert.ok(html.indexOf('<script>') === -1, 'raw <script> 不得直接输出');
  assert.match(html, /&lt;script&gt;/);
  assert.ok(html.indexOf('a&b') === -1, 'step 的 & 必须转义');
});

test('auditRowsHtml 说明列展示 node（契约对齐 Row.NodeID json tag = "node"）', () => {
  const html = auditRowsHtml({ rows: [{ type: 'download', step: 'x', dur_ms: 1, node: 'node-9' }] });
  assert.match(html, /node: node-9/, 'node 应进说明列');
  // 缺 node/err/algorithm → 说明列空，不出现 undefined。
  const noNode = auditRowsHtml({ rows: [{ type: 'download', step: 'x', dur_ms: 1 }] });
  assert.ok(noNode.indexOf('undefined') === -1, '缺 node 不渲染 undefined');
  // node 转义（防 XSS）。
  const xss = auditRowsHtml({ rows: [{ type: 'download', step: 'x', dur_ms: 1, node: '<img src=x>' }] });
  assert.ok(xss.indexOf('<img') === -1, 'node 必须转义');
});