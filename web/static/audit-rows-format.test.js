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

// ---- 完整性渲染（B5-I1：审计面板承接 download 行 integrity_status/integrity_sames） ----

test('auditRowsHtml download 行说明列渲染完整性状态（damaged ×2 / verified / unverified）', () => {
  const mkRow = (integrity_status, integrity_sames) => ({
    type: 'download', step: 'download', dur_ms: 120, bytes: 2048,
    meta: { integrity_status: integrity_status, integrity_sames: integrity_sames },
  });
  // damaged + sames=2 → 「完整性: 完整性异常 ×2」
  const damaged = auditRowsHtml({ rows: [mkRow('damaged', 2)] });
  assert.match(damaged, /完整性: 完整性异常 ×2/, 'damaged 应显示中文文案 + 重下次数');
  // verified（sames 0/缺省）→ 「完整性: 已验证」，无 ×0
  const verified = auditRowsHtml({ rows: [mkRow('verified', 0)] });
  assert.match(verified, /完整性: 已验证/);
  assert.ok(verified.indexOf('×0') === -1, 'sames=0 不显示 ×0');
  // unverified → 「完整性: 未校验」
  const unverified = auditRowsHtml({ rows: [mkRow('unverified', 1)] });
  assert.match(unverified, /完整性: 未校验 ×1/);
});

test('auditRowsHtml 完整性状态为空不渲染；未知状态转义防注入；带 err 并存', () => {
  // 无 integrity_status（普通 download 行）→ 不出现「完整性」字样
  const plain = auditRowsHtml({ rows: [{ type: 'download', step: 'download', dur_ms: 1, meta: {} }] });
  assert.ok(plain.indexOf('完整性') === -1, 'status 空不渲染完整性');
  // 未知状态值原样转义（防注入）
  const weird = auditRowsHtml({ rows: [{ type: 'download', step: 'x', dur_ms: 1, meta: { integrity_status: '<b>hax</b>' } }] });
  assert.ok(weird.indexOf('<b>hax</b>') === -1, '未知 status 不得直接输出原始 HTML');
  assert.match(weird, /&lt;b&gt;hax&lt;\/b&gt;/);
  // damaged 行同时带 err（must-pass 阻断 Fail 行）→ 完整性 + err 并存（；分隔）
  const both = auditRowsHtml({ rows: [{ type: 'download', step: 'download', dur_ms: 1, err: 'integrity: 语义校验失败，重下重试', meta: { integrity_status: 'damaged', integrity_sames: 2 } }] });
  assert.match(both, /完整性: 完整性异常 ×2/);
  assert.match(both, /integrity: 语义校验失败，重下重试/);
});
// ---- 审核修复（2026-10-15）：数值列不得依赖「服务端一定发数字」的隐式不变式 ----

test('auditRowsHtml 数值列遇非数字类型必须转义（防类型漂移成注入面）', () => {
  // 服务端字段是 int64，但 meta 是 any（多层埋点写入）⇒ 类型漂移时必须转义而非原样拼接。
  const html = auditRowsHtml({ rows: [{
    type: 'encrypt', step: 'encrypt', dur_ms: '<img src=x onerror=alert(1)>',
    bytes: '"><script>bad()</script>',
    bw_bps: 'x<b>y</b>',
    meta: { cipher_bytes: '<svg onload=alert(1)>', algorithm: 'ok' },
  }] });
  assert.ok(html.indexOf('<img') === -1, 'dur_ms 字符串不得原样输出');
  assert.ok(html.indexOf('<script>') === -1, 'bytes 字符串不得原样输出');
  assert.ok(html.indexOf('<b>') === -1, 'bw_bps 字符串不得原样输出');
  assert.ok(html.indexOf('<svg') === -1, 'cipher_bytes 字符串不得原样输出');
  assert.match(html, /&lt;img/);
  assert.match(html, /&lt;svg/);
});

test('auditRowsHtml 非有限数（NaN/Infinity）不渲染为字面量', () => {
  const html = auditRowsHtml({ rows: [{ type: 'download', step: 'x', dur_ms: NaN, bytes: Infinity, bw_bps: -Infinity }] });
  assert.ok(html.indexOf('NaN') === -1, 'NaN 不应出现在表格里');
  assert.ok(html.indexOf('Infinity') === -1, 'Infinity 不应出现在表格里');
});

test('auditRowsHtml 不产生内联 style（样式统一走 style.css .audit-* class）', () => {
  const html = auditRowsHtml({ rows: [
    { type: 'encrypt', step: 'encrypt', dur_ms: 30, bytes: 1024, meta: { cipher_bytes: 2048, algorithm: 'a' } },
    { type: 'download', step: 'download', dur_ms: 120, bytes: 2048, bw_bps: 4096, node: 'n1', err: 'e' },
  ] });
  assert.ok(html.indexOf('style="') === -1, '渲染函数不得写内联 style');
  assert.match(html, /class="audit-table"/);
  assert.match(html, /class="audit-cell"/);
});
