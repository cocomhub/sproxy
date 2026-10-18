// SPDX-License-Identifier: Apache-2.0
// quota-format.test.js —— 配额面板纯函数单测（node --test，无浏览器依赖）。
//
// 覆盖三条**契约语义**（后端与 CLI 同口径，UI 必须保真）：
//   ① 未知 ≠ 0；② 上限 0 = 不限；③ 百分比在未知/不限时**不编造 0**。
const test = require('node:test');
const assert = require('node:assert');
const q = require('./quota-format.js');

test('formatSize 与 app-render 同语义（GB 档）', () => {
  assert.strictEqual(q.formatSize(0), '0 B');
  assert.strictEqual(q.formatSize(512), '512 B');
  assert.strictEqual(q.formatSize(2048), '2.0 KB');
  assert.strictEqual(q.formatSize(3 * 1048576), '3.0 MB');
  assert.strictEqual(q.formatSize(2 * 1073741824), '2.00 GB');
  assert.strictEqual(q.formatSize(null), '-');
});

test('quotaCell：未知 ≠ 0（hasQuota=false 显示「未知」，不得渲染 0）', () => {
  assert.strictEqual(q.quotaCell(0, 0, false, false), '未知');
  assert.strictEqual(q.quotaCell(123, 456, false, false), '未知');
});

test('quotaCell：上限 0 / unlimited ⇒ 「不限」', () => {
  assert.strictEqual(q.quotaCell(1024, 0, true, false), '1.0 KB / 不限');
  assert.strictEqual(q.quotaCell(1024, 2048, true, true), '1.0 KB / 不限');
});

test('quotaCell：正常态「已用 / 上限」', () => {
  assert.strictEqual(q.quotaCell(1024, 2048, true, false), '1.0 KB / 2.0 KB');
});

test('usagePercent：未知/不限不编造 0（返回 null）', () => {
  assert.strictEqual(q.usagePercent(0, 0, false, false), null);
  assert.strictEqual(q.usagePercent(100, 0, true, false), null);
  assert.strictEqual(q.usagePercent(100, 200, true, true), null);
});

test('usagePercent：正常态取整并钳位到 [0,100]', () => {
  assert.strictEqual(q.usagePercent(50, 200, true, false), 25);
  assert.strictEqual(q.usagePercent(500, 200, true, false), 100);
});

test('volumeQuotaTableHtml：空列表给空态；正常渲染卷名与单元格', () => {
  assert.match(q.volumeQuotaTableHtml([]), /empty-msg/);
  // 真实后端形状（GET /api/quota 的逐卷明细：volume/used/capacity/has_quota）。
  const html = q.volumeQuotaTableHtml([
    { volume: 'v1', used: 1024, capacity: 2048, has_quota: true },
    { volume: 'v2', used: 0, capacity: 0, has_quota: false },
  ]);
  assert.match(html, /v1/);
  assert.match(html, /1\.0 KB \/ 2\.0 KB/);
  assert.match(html, /v2/);
  assert.match(html, /未知/);
});

test('federatedQuotaHtml：partial=true 必须显式提示汇总不完整', () => {
  const html = q.federatedQuotaHtml({ has_quota: false, partial: true, used: 0, capacity: 0, volumes: [] });
  assert.match(html, /quota-partial/);
  assert.match(html, /汇总不完整/);
  const clean = q.federatedQuotaHtml({ has_quota: true, partial: false, used: 1024, capacity: 4096, volumes: [] });
  assert.doesNotMatch(clean, /quota-partial/);
});

test('HTML 转义：卷名含尖括号不得注入', () => {
  const html = q.volumeQuotaTableHtml([{ volume: '<img src=x>', used: 1, capacity: 2, has_quota: true }]);
  assert.doesNotMatch(html, /<img/);
  assert.match(html, /&lt;img/);
});
